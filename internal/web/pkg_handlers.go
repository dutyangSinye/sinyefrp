package web

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"sinyefrp/internal/buildinfo"
	"sinyefrp/internal/endpoint"
	"sinyefrp/internal/frpapi"
	"sinyefrp/internal/pack"
	"sinyefrp/internal/store"
)

// 本文件实现部署包生成接口（01-prd F09-F17）。
//
// 密钥处理原则：
//   - token 与 Dashboard 密码在响应中一律遮蔽后再返回；
//   - 前端需要明文时通过「显示」交互自行展开，而明文只在前端内存中，
//     服务端不落库、不写日志（F17 / §8.5）；
//   - ZIP 内含明文密钥，属敏感文件，响应一律 no-store。

// 遮蔽时的正则：只匹配配置里的赋值行，避免误伤注释与其它文本。
// 三组捕获：赋值前缀（含开引号）、值、闭引号。
var (
	reTokenAssign    = regexp.MustCompile(`(?m)^(\s*token\s*=\s*")([^"]*)(")`)
	rePasswordAssign = regexp.MustCompile(`(?m)^(\s*password\s*=\s*")([^"]*)(")`)
	reSecretAssign   = regexp.MustCompile(`(?m)^(\s*secretKey\s*=\s*")([^"]*)(")`)
)

// maskRes 是全部需要遮蔽的字段。
var maskRes = []*regexp.Regexp{reTokenAssign, rePasswordAssign, reSecretAssign}

// maskSecret 遮蔽敏感值：保留前 4 位与后 2 位，便于管理员确认「是不是我填的那个」，
// 同时使中间部分无法被读取或抄写。
func maskSecret(v string) string {
	if v == "" {
		return ""
	}
	if len(v) <= 8 {
		return strings.Repeat("*", len(v))
	}
	return v[:4] + strings.Repeat("*", 10) + v[len(v)-2:]
}

// maskTOML 把渲染结果中的敏感字段替换为遮蔽值。
// （当前统一走 maskAll，保留此函数用于需要保留首末位提示的场景。）
func maskTOML(s string) string {
	s = reTokenAssign.ReplaceAllStringFunc(s, func(m string) string {
		sub := reTokenAssign.FindStringSubmatch(m)
		return sub[1] + maskSecret(strings.Trim(sub[2-1+1], `"`)) + sub[2]
	})
	return s
}

// pkgMetaResp 描述打包能力的元信息。
type pkgMetaResp struct {
	FRPVersion string `json:"frp_version"`
	// 支持范围是一个集合而不是单个前缀：frps 与内嵌 frpc 分批升级时
	// 两端版本必然不一致，前端若只按一个前缀 startsWith 判断，
	// 过渡期会把合法的那半边误报成「不兼容」。
	SupportedPrefixes []string `json:"supported_prefixes"`
	VersionWarning    string   `json:"version_warning"`
	Source          string   `json:"source"`
	ClientExeSize   int      `json:"client_exe_size"`
	ServerExeSize   int      `json:"server_exe_size"`
	ClientExeSHA256 string   `json:"client_exe_sha256"`
	ServerExeSHA256 string   `json:"server_exe_sha256"`
	ProxyTypes      []string `json:"proxy_types"`

	// ClientPlatforms / ServerPlatforms 是可选的目标平台。
	//
	// 只下发**真的有二进制**的平台（由 Binaries 按内嵌资产算出）：
	// 下拉里放一个选了必然失败的选项，比不放更糟 —— 管理员要到装包那一步
	// 才知道这台机器不被支持，而那时错误信息已经离现场很远了。
	ClientPlatforms []string `json:"client_platforms"`
	ServerPlatforms []string `json:"server_platforms"`

	// —— 管理平台自身（01-prd F18）——
	//
	// 与上面两组不同，这里的「二进制」是当前运行的这个 exe，而不是内嵌资源，
	// 因此多一个可用性字段：读不到自己时前端要把下载按钮置灰并说明原因，
	// 而不是让管理员点了之后才收到 503。
	PlatformVersion     string `json:"platform_version"`
	PlatformExeSize     int    `json:"platform_exe_size"`
	PlatformExeSHA256   string `json:"platform_exe_sha256"`
	PlatformAvailable   bool   `json:"platform_available"`
	PlatformUnavailable string `json:"platform_unavailable_reason"`
}

// platformIDs 把平台列表转成 ID 字符串列表。
//
// 下发的 ID 与后端常量逐字一致（windows-amd64 / linux-amd64 / linux-arm64）：
// 前端下拉的选项值必须能原样回传给后端，中间任何一次改名都会让
// 「界面能选、提交被拒」，而这种错误只在提交时才暴露。
func platformIDs(ps []pack.Platform) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, string(p))
	}
	return out
}

// handlePkgMeta 返回 FRP 版本、内嵌二进制信息与支持的代理类型（01-prd F15/F22 前置）。
func (s *Server) handlePkgMeta(w http.ResponseWriter, r *http.Request) {
	platform := pkgMetaResp{
		PlatformVersion:     buildinfo.Version,
		PlatformAvailable:   s.self.Available(),
		PlatformUnavailable: s.self.Reason(),
	}
	if s.self.Available() {
		platform.PlatformExeSize = s.self.Size()
		platform.PlatformExeSHA256 = s.self.SHA256()
	}

	if s.bins == nil {
		// FRP 二进制缺失只影响客户端/服务端包，平台包照样能生成，
		// 因此这里返回 200 而不是 503，把「哪一项不可用」交给前端分别呈现。
		writeJSON(w, http.StatusOK, platform)
		return
	}

	// 版本要执行一次子进程才能拿到，因此在这里按需触发并缓存，
	// 而不是放在启动路径上拖慢服务就绪。探测失败不影响其余字段返回。
	if _, err := s.bins.EnsureVersion(r.Context()); err != nil {
		s.log.Warn("探测 FRP 版本失败", "err", err)
	}

	types := make([]string, 0, len(pack.AllProxyTypes))
	for _, t := range pack.AllProxyTypes {
		types = append(types, string(t))
	}

	platform.FRPVersion = s.bins.Version()
	platform.SupportedPrefixes = frpapi.SupportedVersionPrefixes
	platform.VersionWarning = s.bins.VersionWarning()
	platform.Source = s.bins.Source
	platform.ClientExeSize = s.bins.ClientSize()
	platform.ServerExeSize = s.bins.ServerSize()
	platform.ClientExeSHA256 = s.bins.ClientSHA256()
	platform.ServerExeSHA256 = s.bins.ServerSHA256()
	platform.ProxyTypes = types
	platform.ClientPlatforms = platformIDs(s.bins.ClientPlatforms())
	platform.ServerPlatforms = platformIDs(s.bins.ServerPlatforms())

	writeJSON(w, http.StatusOK, platform)
}

// handleClientDefaults 给出客户端表单的默认值，其中 clientID 由服务端生成。
//
// 连接目标（server_addr / server_port）与 token 优先取「客户端接入点」配置
// （设置页维护）：生成的客户端包解压即可连上，不需要管理员手工回填，
// 也保证它与同一接入点下生成的服务端包用的是同一个 token。
//
// 接入点未配置或读取失败时回退为原来的行为（空地址、FRP 默认端口、随机 token），
// 前端据此提示管理员去设置页补全。
//
// 用 GET + no-store 而不是 POST：本接口不改变服务端任何状态，
// 但它返回的默认值含密钥，必须禁止任何中间层缓存，否则会出现多个管理员拿到同一份 token。
func (s *Server) handleClientDefaults(w http.ResponseWriter, r *http.Request) {
	noStore(w)

	ep, err := endpoint.Load(r.Context(), s.st)
	if err != nil {
		// 降级而不是报错：接入点读不出来不该让整个表单打不开。
		// 最常见的情况是 DPAPI 密文随 SQLite 换机器后解不开了。
		s.log.Warn("读取客户端接入点失败，客户端表单回退为手工填写", "err", err)
	}

	token := ep.Token
	if token == "" {
		// 接入点未配 token 时仍给一个随机值，让表单开箱可用；
		// 前端会提示管理员把它固化到接入点，否则两端 token 对不上。
		token = pack.NewToken()
	}

	writeJSON(w, http.StatusOK, pack.ClientSpec{
		ServerAddr: ep.Addr,
		ServerPort: ep.PortOrDefault(),
		ClientID:   pack.NewClientID(),
		Token:      token,
		LogLevel:   "info",
		LogMaxDays: 7,
		// 这条默认代理是字面量构造、不经过 ClientSpec.Normalize()，
		// 因此两个切片必须显式初始化成空切片：nil 会被编码成 null（见 spec.emptyIfNil）。
		// 这里不能图省事改调 Normalize()——那会把 Name 填成 clientID，
		// 让「显示名称」默认框里带上一个随机串，界面上看着像已经填过了。
		//
		// RobotType 刻意不给默认值：这是个需要管理员如实选择的业务分类，
		// 预选任何一项都是在替用户猜（猜「其他」更是把一个具体机器人标成了兜底项）。
		// 空值即「未分类」，界面会提示但不拦。
		Proxies: []pack.ProxySpec{{
			Name:        "app-tcp",
			Type:        pack.ProxyTCP,
			LocalIP:     "127.0.0.1",
			LocalPort:   8080,
			CustomHosts: []string{},
			Locations:   []string{},
		}},
	})
}

// handleServerDefaults 给出服务器表单的默认值，token 与 Dashboard 密码随机生成。
//
// token 若已在「客户端接入点」里配置，则以接入点为准：两端共用同一个 token
// 是 frpc 能连上的前提，服务端包与客户端包必须一致。
//
// bind_port 刻意不取接入点的端口——那是 frps 的内部监听端口，
// frps 前面存在 NAT 映射时对外端口与之不同（内部 7000、对外可能 12123）。
func (s *Server) handleServerDefaults(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	var spec pack.ServerSpec
	spec.Normalize() // 空入参即触发全部默认值生成

	if ep, err := endpoint.Load(r.Context(), s.st); err != nil {
		s.log.Warn("读取客户端接入点失败，服务端 token 使用随机值", "err", err)
	} else if ep.Token != "" {
		spec.Token = ep.Token
	}

	writeJSON(w, http.StatusOK, spec)
}

// pkgPreviewResp 是配置预览的响应。
type pkgPreviewResp struct {
	FileName   string   `json:"file_name"`
	TOML       string   `json:"toml"`
	Warnings   []string `json:"warnings"`
	FRPVersion string   `json:"frp_version"`
	Summary    string   `json:"summary"`
	Files      []string `json:"files"`
	SizeBytes  int      `json:"size_bytes"`
	// Blocking 是「会阻止下载」的问题，与 Warnings 分开。
	//
	// 分开的理由：Warnings 是「包能生成，但你应该知道」，Blocking 是
	// 「这个包生成出来也跑不起来」。混在一个列表里，前端无法区分
	// 该不该把下载按钮置灰，管理员也会习惯性忽略整块黄色提示。
	// 当前只有一类：远端端口与已签发或已在线的东西撞了（01-prd F19）。
	Blocking []string `json:"blocking"`

	// Platform 是本次预览对应的目标平台，回显给前端。
	//
	// 回显而不是让前端自己记：表单里选了 Linux、预览又按 Windows 渲染的话，
	// 管理员看到的文件清单与实际下载到的包就不是同一个东西，
	// 而这种不一致在界面上完全看不出来。
	Platform string `json:"platform"`
}

// endpointTokenWarnings 检查本次提交的 token 与「客户端接入点」保存的是否一致。
//
// 两端 token 不一致是 frpc 连不上 frps 的直接原因，而它在静态校验里看不出来——
// 单看任何一侧都是合法配置，只有把两侧放在一起比才知道对不上。
// 因此必须在预览阶段显式提示，否则管理员要等到包装到目标机器上才发现。
//
// 返回 nil 表示不做提示：接入点没有配 token 时无从比对，
// 此时报「不一致」会误导（frps 侧可能本来就没启用 auth.token）。
//
// side 取 "client" 或 "server"，两者提示的后果不同，不能共用一句文案。
func (s *Server) endpointTokenWarnings(ctx context.Context, side, token string) []string {
	ep, err := endpoint.Load(ctx, s.st)
	if err != nil || ep.Token == "" {
		return nil
	}
	if token == ep.Token {
		return nil
	}

	if side == "client" {
		return []string{
			"当前 token 与「设置」页保存的客户端接入点 token 不一致。" +
				"若 frps 侧用的是接入点里的 token，这个客户端将连不上。",
		}
	}
	return []string{
		"当前 token 与「设置」页保存的客户端接入点 token 不一致。" +
			"之后生成的客户端包会使用接入点里的 token，两端将对不上。",
	}
}

// handleClientPreview 校验并预览客户端配置（01-prd F15 的「打包前校验」）。
func (s *Server) handleClientPreview(w http.ResponseWriter, r *http.Request) {
	var spec pack.ClientSpec
	if !s.decodeJSON(w, r, &spec) {
		return
	}
	if s.bins == nil {
		writeError(w, http.StatusServiceUnavailable, errKindUnavailable, "未加载 FRP 二进制")
		return
	}

	spec.Normalize()
	if errs := spec.Validate(); len(errs) > 0 {
		writeError(w, http.StatusBadRequest, errKindBadRequest, pack.JoinErrsText(errs))
		return
	}

	tomlContent, err := pack.RenderClientTOML(&spec)
	if err != nil {
		s.log.Error("渲染客户端配置失败", "err", err)
		writeError(w, http.StatusInternalServerError, errKindInternal, "渲染配置失败")
		return
	}

	// 预览也真的跑一次 verify：让管理员在下载前就看到「配置能不能被 frpc 接受」，
	// 而不是下载之后装到目标机器上才发现字段写错。
	vr, err := s.bins.VerifyClientConfig(r.Context(), tomlContent)
	if err != nil {
		var ve *pack.VerifyError
		if errors.As(err, &ve) {
			writeError(w, http.StatusBadRequest, errKindBadRequest,
				"FRP 校验未通过："+ve.Output)
			return
		}
		s.log.Error("校验客户端配置失败", "err", err)
		writeError(w, http.StatusInternalServerError, errKindInternal, "配置校验失败："+err.Error())
		return
	}

	warnings := clientWarnings(&spec)
	warnings = append(warnings, s.endpointTokenWarnings(r.Context(), "client", spec.Token)...)

	// 端口冲突在这里**不报错**，而是作为 Blocking 返回。
	//
	// 预览的价值正是让管理员看到完整配置与全部问题；如果直接 400，
	// 管理员拿不到 TOML，反而不好判断该把哪个端口改到多少。
	// 真正的拦截放在下载接口（见 handleClientDownload）。
	blocking := conflictMessages(s.checkClientPortConflicts(r.Context(), &spec))

	writeJSON(w, http.StatusOK, pkgPreviewResp{
		FileName:   "frpc.toml",
		TOML:       maskAll(tomlContent),
		Warnings:   warnings,
		Blocking:   blocking,
		FRPVersion: vr.Version,
		Summary:    clientSummary(&spec),
		// 文件清单与体积都按**目标平台**给：Windows 包是 .ps1 + 双击入口，
		// Linux 包是 .sh + systemd unit，二进制体积也不同。
		// 这里若固定按 Windows 给，管理员在界面上看到的清单会与实际下载到的不一致。
		Files:      pack.ClientFileNames(spec.Platform),
		SizeBytes:  s.bins.ClientSizeFor(spec.Platform),
		Platform:   string(spec.Platform),
	})
}

// handleServerPreview 校验并预览服务器配置。
func (s *Server) handleServerPreview(w http.ResponseWriter, r *http.Request) {
	var spec pack.ServerSpec
	if !s.decodeJSON(w, r, &spec) {
		return
	}
	if s.bins == nil {
		writeError(w, http.StatusServiceUnavailable, errKindUnavailable, "未加载 FRP 二进制")
		return
	}

	spec.Normalize()
	if errs := spec.Validate(); len(errs) > 0 {
		writeError(w, http.StatusBadRequest, errKindBadRequest, pack.JoinErrsText(errs))
		return
	}

	tomlContent, err := pack.RenderServerTOML(&spec)
	if err != nil {
		s.log.Error("渲染服务器配置失败", "err", err)
		writeError(w, http.StatusInternalServerError, errKindInternal, "渲染配置失败")
		return
	}
	vr, err := s.bins.VerifyServerConfig(r.Context(), tomlContent)
	if err != nil {
		var ve *pack.VerifyError
		if errors.As(err, &ve) {
			writeError(w, http.StatusBadRequest, errKindBadRequest, "FRP 校验未通过："+ve.Output)
			return
		}
		s.log.Error("校验服务器配置失败", "err", err)
		writeError(w, http.StatusInternalServerError, errKindInternal, "配置校验失败："+err.Error())
		return
	}

	warnings := spec.Warnings()
	warnings = append(warnings, s.endpointTokenWarnings(r.Context(), "server", spec.Token)...)

	writeJSON(w, http.StatusOK, pkgPreviewResp{
		FileName:   "frps.toml",
		TOML:       maskAll(tomlContent),
		Warnings:   warnings,
		Blocking:   []string{},
		FRPVersion: vr.Version,
		Summary:    serverSummary(&spec),
		// 服务器包目前只有 Windows 版（见 BuildServerPackage 的注释），
		// 因此固定按默认平台给清单与体积。
		Files:      pack.ServerFileNames(pack.DefaultPlatform),
		SizeBytes:  s.bins.ServerSizeFor(pack.DefaultPlatform),
		Platform:   string(pack.DefaultPlatform),
	})
}

// handleClientDownload 生成并流式下载客户端部署包（01-prd F10/F15/F16/F17）。
func (s *Server) handleClientDownload(w http.ResponseWriter, r *http.Request) {
	var spec pack.ClientSpec
	if !s.decodeJSON(w, r, &spec) {
		return
	}
	if s.bins == nil {
		writeError(w, http.StatusServiceUnavailable, errKindUnavailable, "未加载 FRP 二进制")
		return
	}

	// 先归一再查冲突：client_id 为空时由 Normalize 生成，
	// 而冲突判定要用它来区分「自己的端口」与「别人的端口」。
	spec.Normalize()

	// 端口冲突在生成之前拦下（01-prd F19 / §8.2）。
	//
	// 拦下的代价是管理员多点一次；不拦的代价是一个装到目标机器上才会
	// 注册失败的包，而失败现象（frpc 日志里一句端口被占用）离管理台很远。
	// 端口是全局资源，宁可在这里挡住。
	if conflicts := s.checkClientPortConflicts(r.Context(), &spec); len(conflicts) > 0 {
		msgs := conflictMessages(conflicts)
		s.recordPkgFailure(r, "client", spec.ClientID, errors.New(strings.Join(msgs, " ")))
		summary := "远端端口冲突，已阻止生成：" + msgs[0]
		if len(msgs) > 1 {
			summary = "远端端口冲突（" + strconv.Itoa(len(msgs)) + " 处），已阻止生成。请逐条调整后重试。"
		}
		writeErrorDetails(w, http.StatusConflict, errKindConflict, summary, msgs)
		return
	}

	pkg, err := pack.BuildClientPackage(r.Context(), s.bins, &spec)
	if err != nil {
		s.recordPkgFailure(r, "client", spec.ClientID, err)
		s.writeBuildError(w, err)
		return
	}

	// 记账在写出之前：ZIP 一旦开始流向客户端就无法回滚，
	// 若等到写失败才记账，会漏掉「已发出但传输中断」的情况（F16）。
	s.recordPkgSuccess(r, pkg, spec.ClientID, spec.Name)
	s.recordClientRegistry(r, &spec)
	s.streamPackage(w, pkg)
}

// handleServerDownload 生成并流式下载服务器部署包（01-prd F12/F15/F16）。
func (s *Server) handleServerDownload(w http.ResponseWriter, r *http.Request) {
	var spec pack.ServerSpec
	if !s.decodeJSON(w, r, &spec) {
		return
	}
	if s.bins == nil {
		writeError(w, http.StatusServiceUnavailable, errKindUnavailable, "未加载 FRP 二进制")
		return
	}

	pkg, err := pack.BuildServerPackage(r.Context(), s.bins, &spec)
	if err != nil {
		s.recordPkgFailure(r, "server", "", err)
		s.writeBuildError(w, err)
		return
	}

	s.recordPkgSuccess(r, pkg, "", "frps (bind "+strconv.Itoa(spec.BindPort)+")")
	s.streamPackage(w, pkg)
}

// handlePkgRecords 返回部署包生成记录（01-prd F16）。
//
// 记录只含非敏感摘要；本接口不提供「重新下载历史包」——
// 历史 ZIP 从未落盘，服务端也不保留当时的密钥，因此无法也不应重发（§8.5）。
func (s *Server) handlePkgRecords(w http.ResponseWriter, r *http.Request) {
	recs, err := s.st.ListPkgRecords(r.Context(), 100)
	if err != nil {
		s.log.Error("查询部署包记录失败", "err", err)
		writeError(w, http.StatusInternalServerError, errKindInternal, "查询部署包记录失败")
		return
	}

	type row struct {
		ID          int64  `json:"id"`
		At          any    `json:"at"`
		Actor       string `json:"actor"`
		Kind        string `json:"kind"`
		ClientID    string `json:"client_id"`
		DisplayName string `json:"display_name"`
		FRPVersion  string `json:"frp_version"`
		ExeSHA256   string `json:"exe_sha256"`
		Summary     string `json:"summary"`
	}
	out := make([]row, 0, len(recs))
	for _, p := range recs {
		out = append(out, row{
			ID: p.ID, At: timePtr(p.At), Actor: p.Actor, Kind: p.Kind,
			ClientID: p.ClientID, DisplayName: p.DisplayName,
			FRPVersion: p.FRPVersion, ExeSHA256: p.ExeSHA256, Summary: p.Summary,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"records": out})
}

// ---------- 内部辅助 ----------

// writeBuildError 把生成失败映射为合适的状态码。
func (s *Server) writeBuildError(w http.ResponseWriter, err error) {
	var ve *pack.VerifyError
	switch {
	case errors.As(err, &ve):
		writeError(w, http.StatusBadRequest, errKindBadRequest, ve.Error())
	case errors.Is(err, pack.ErrInvalid):
		writeError(w, http.StatusBadRequest, errKindBadRequest, err.Error())
	case errors.Is(err, pack.ErrBinaryMissing):
		writeError(w, http.StatusServiceUnavailable, errKindUnavailable, err.Error())
	case errors.Is(err, pack.ErrSelfMissing):
		// 与缺 FRP 二进制同一类：不是请求写错了，而是这台机器上暂时产不出这个包。
		writeError(w, http.StatusServiceUnavailable, errKindUnavailable, err.Error())
	default:
		s.log.Error("生成部署包失败", "err", err)
		writeError(w, http.StatusInternalServerError, errKindInternal, "生成部署包失败："+err.Error())
	}
}

// streamPackage 以流式方式把 ZIP 写给客户端。
func (s *Server) streamPackage(w http.ResponseWriter, pkg *pack.Package) {
	h := w.Header()
	h.Set("Content-Type", "application/zip")
	h.Set("Content-Disposition", "attachment; filename=\""+pkg.FileName+"\"")
	// 敏感文件：禁止任何缓存，包括浏览器内存缓存与中间代理。
	noStore(w)
	w.WriteHeader(http.StatusOK)

	if err := pkg.WriteZIP(w); err != nil {
		// 响应头已发出，此时无法再改状态码；只能留痕，便于排查「下载到一半中断」。
		s.log.Error("写出部署包失败", "file", pkg.FileName, "err", err)
	}
}

// recordPkgSuccess 记录一次成功的打包（01-prd F16）。
//
// 同时写两处，因为二者服务不同目的：
//   - audit：谁在何时做了什么，安全视角的流水，包含失败尝试；
//   - pkg_record：生成过哪些包，业务视角的台账，带结构化字段供界面筛选。
func (s *Server) recordPkgSuccess(r *http.Request, pkg *pack.Package, clientID, displayName string) {
	actor := actorOf(r)
	if err := s.st.AddAudit(r.Context(), store.Audit{
		Actor: actor, Action: "pkg.generate", Target: pkgTarget(clientID, pkg.Kind),
		Result: "success", ClientIP: clientIP(r), Detail: pkg.Summary,
	}); err != nil {
		s.log.Warn("写入打包审计失败", "err", err)
	}

	if err := s.st.AddPkgRecord(r.Context(), store.PkgRecord{
		Actor: actor, Kind: pkg.Kind, ClientID: clientID, DisplayName: displayName,
		FRPVersion: pkg.Version, ExeSHA256: pkg.ExeSHA256, Summary: pkg.Summary,
	}); err != nil {
		s.log.Warn("写入部署包记录失败", "err", err)
	}
}

// recordClientRegistry 把本次签发的客户端写入台账（01-prd F19）。
//
// 这里刻意用 Error 级别而不是 Warn：写台账失败意味着这台客户端从此不参与
// 端口冲突判定，之后很可能给别的机器分配同一个端口，而那时没有任何提示。
// 包本身已经生成好，不该因此中断下载，但必须留下足够醒目的痕迹。
//
// 只有 TCP/UDP 记远端端口：其余类型不占 frps 上的全局端口，
// 表单里残留的数字（用户先填了端口再改类型）不能被当成占用。
func (s *Server) recordClientRegistry(r *http.Request, spec *pack.ClientSpec) {
	ports := make([]store.ClientPort, 0, len(spec.Proxies))
	for _, p := range spec.Proxies {
		remote := 0
		if p.Type.NeedsRemotePort() {
			remote = p.RemotePort
		}
		ports = append(ports, store.ClientPort{
			ClientID:   spec.ClientID,
			ProxyName:  p.Name,
			ProxyType:  string(p.Type),
			RemotePort: remote,
			LocalPort:  p.LocalPort,
		})
	}

	if _, err := s.st.UpsertClient(r.Context(), store.ClientRegistry{
		ClientID:    spec.ClientID,
		DisplayName: spec.Name,
		RobotType:   spec.RobotType,
		// 平台必须跟着这次签发写进台账：改包功能全靠它知道该产什么形态的包。
		// 漏写的话 Linux 盒子下一次改包会退回 Windows 默认，而安装脚本不会报错，
		// 症状要等上线才发现。
		//
		// 取 spec.Platform（**目标机器**的平台），不是本进程的运行平台 ——
		// 平台自己跑在 Windows Server 上，照样要能产出 Linux 的包。
		Platform:   string(platformOf(spec)),
		ServerAddr: spec.ServerAddr,
		ServerPort: spec.ServerPort,
		Status:     store.ClientStatusActive,
	}, ports); err != nil {
		s.log.Error("写入客户端台账失败", "client_id", spec.ClientID, "err", err)
	}
}

// platformOf 取这次签发的**目标平台**，并保证结果一定是个合法值。
//
// 为什么不直接用 spec.Platform：spec.go 的 ClientSpec.Normalize() 才把空串补成
// DefaultPlatform，而本函数是台账写入路径，不该依赖上游有没有调过 Normalize ——
// 一旦某条路径漏了，写进台账的就是空串，而空串在改包时会被界面当成
// 「未记录」去问管理员，等于把一次确定已知的签发变成了待确认。
//
// 这里只做这一件兜底，不整体调用 Normalize()：那会连带改写 Name 等其它字段。
func platformOf(spec *pack.ClientSpec) pack.Platform {
	p := pack.Platform(strings.TrimSpace(string(spec.Platform)))
	if p == "" {
		return pack.DefaultPlatform
	}
	return p
}

// recordPkgFailure 记录一次失败的打包尝试。
//
// 失败只进审计流，不进 pkg_record —— 后者是「生成过的包」台账，
// 把失败尝试混进去会让运维误以为存在一个可以重新下载的包。
func (s *Server) recordPkgFailure(r *http.Request, kind, clientID string, cause error) {
	if err := s.st.AddAudit(r.Context(), store.Audit{
		Actor: actorOf(r), Action: "pkg.generate", Target: pkgTarget(clientID, kind),
		Result: "error", ClientIP: clientIP(r), Detail: cause.Error(),
	}); err != nil {
		s.log.Warn("写入打包审计失败", "err", err)
	}
}

// actorOf 取出当前登录用户名，未登录时返回空串。
func actorOf(r *http.Request) string {
	if u := currentUser(r.Context()); u != nil {
		return u.Username
	}
	return ""
}

// pkgTarget 给出审计记录里的目标标识。
func pkgTarget(clientID, kind string) string {
	if clientID != "" {
		return clientID
	}
	return kind
}

// noStore 关闭所有层级的缓存。
func noStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
}

// maskAll 把渲染结果中所有敏感赋值行遮蔽。
func maskAll(b []byte) string {
	s := string(b)
	for _, re := range maskRes {
		s = re.ReplaceAllStringFunc(s, func(m string) string {
			sub := re.FindStringSubmatch(m)
			if len(sub) < 4 {
				return m
			}
			return sub[1] + maskSecret(sub[2]) + sub[3]
		})
	}
	return s
}

// clientWarnings 汇总客户端配置的提醒项。
func clientWarnings(c *pack.ClientSpec) []string {
	var out []string
	if !c.EnableTLS {
		out = append(out, "未启用传输加密（transport.tls）：token 与代理数据将以明文经过网络。"+
			"若服务器在公网，建议在 frps 与 frpc 两侧同时启用。")
	}
	if c.ServerAddr == "" {
		out = append(out, "未填写服务器地址：客户端装好后无法连接。")
	}
	seen := map[int]string{}
	for _, p := range c.Proxies {
		if p.Type.NeedsRemotePort() {
			if prev, ok := seen[p.RemotePort]; ok {
				out = append(out, "远端端口 "+strconv.Itoa(p.RemotePort)+" 被多个代理使用（"+prev+"）。")
			}
			seen[p.RemotePort] = p.Name
		}
	}
	return out
}

func clientSummary(c *pack.ClientSpec) string {
	s := "客户端包 · " + c.Name + "(" + c.ClientID + ")"
	if label := pack.RobotTypeLabel(c.RobotType); label != "" {
		s += " · " + label
	}
	return s + " · 代理 " + strconv.Itoa(len(c.Proxies)) + " 条"
}

func serverSummary(s *pack.ServerSpec) string {
	return "服务器包 · bind " + s.BindAddr + ":" + strconv.Itoa(s.BindPort) +
		" · Dashboard " + s.DashboardAddr + ":" + strconv.Itoa(s.DashboardPort)
}
