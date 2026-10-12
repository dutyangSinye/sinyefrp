package pack

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"strings"
	"time"
)

// PkgFile 是部署包内的一个文件。
type PkgFile struct {
	Name string
	Data []byte
	Exec bool
}

// Package 是一个已组装完成的部署包，随时可流式写出。
//
// 内容全部驻留内存：单个包约 16-20 MB，相比「先落盘再读取」，
// 省掉了磁盘残留敏感 ZIP 的风险（01-prd §8.5：只即时生成并流式下载，
// 不长期保存完整 ZIP）。
type Package struct {
	FileName  string // 下载文件名
	Kind      string // "client" / "server"
	Version   string // 生成时使用的 FRP 版本
	ExeSHA256 string // 主程序哈希，写入审计（01-prd F15）
	Summary   string // 非敏感摘要，写入审计（01-prd F16）
	Files     []PkgFile
}

// Size 返回未压缩总大小，用于界面提示。
func (p *Package) Size() int {
	n := 0
	for _, f := range p.Files {
		n += len(f.Data)
	}
	return n
}

// WriteZIP 将部署包以 ZIP 流写出。
//
// 直接写 io.Writer（在 HTTP 场景下就是 http.ResponseWriter），
// 全程不在磁盘上产生 ZIP 文件。
func (p *Package) WriteZIP(w io.Writer) error {
	zw := zip.NewWriter(w)
	for _, f := range p.Files {
		hdr := &zip.FileHeader{Name: f.Name, Method: zip.Deflate}
		hdr.Modified = time.Now()
		if f.Exec {
			hdr.SetMode(0o755)
		} else {
			hdr.SetMode(0o644)
		}
		fw, err := zw.CreateHeader(hdr)
		if err != nil {
			return fmt.Errorf("写入 %s 失败: %w", f.Name, err)
		}
		if _, err := fw.Write(f.Data); err != nil {
			return fmt.Errorf("写入 %s 内容失败: %w", f.Name, err)
		}
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("收尾 ZIP 失败: %w", err)
	}
	return nil
}

// BuildClientPackage 生成客户端部署包（01-prd F09/F10/F15）。
func BuildClientPackage(ctx context.Context, bins *Binaries, spec *ClientSpec) (*Package, error) {
	spec.Normalize()
	if errs := spec.Validate(); len(errs) > 0 {
		return nil, joinErrs(errs)
	}

	pf := spec.Platform

	tomlContent, err := RenderClientTOML(spec)
	if err != nil {
		return nil, err
	}

	// 打包前先让 frpc 自己裁决配置是否合法（01-prd F15）。
	// 校验不通过就到这里为止，绝不会生成一个装上去跑不起来的包。
	//
	// 校验刻意用**宿主平台**那份二进制：TOML 的语法与字段校验和目标机器无关，
	// 而本机只能执行自己这一种格式。这样既正确，又不必为校验内嵌各平台副本。
	vr, err := bins.VerifyClientConfig(ctx, tomlContent)
	if err != nil {
		return nil, err
	}

	// 交付的二进制必须来自目标平台。这一步排在校验之后是刻意的：
	// 先确认配置合法，再去动那几十兆的二进制，省掉一大半无效开销。
	binData, err := bins.ClientBinaryFor(pf)
	if err != nil {
		return nil, err
	}

	exeName := pf.ClientExeName()
	svcName := pf.ServiceName("frpc", spec.ClientID)

	scripts, err := renderScripts(pf, scriptVars{
		Role:        "客户端",
		ExeName:     exeName,
		ConfName:    "frpc.toml",
		TaskName:    svcName,
		DisplayName: spec.Name,
		Version:     vr.Version,
		ConfFlag:    "-c",
		DataPaths:   []string{"frpc.log", "logs"},
	})
	if err != nil {
		return nil, err
	}

	fileList := buildFileList(pf, []fileDesc{
		{exeName, "FRP 客户端主程序"},
		{"frpc.toml", "客户端配置（含认证 token，属敏感文件）"},
	}, scripts)

	readme, err := renderReadme(ReadmeData{
		Role:         "客户端",
		Rule:         strings.Repeat("=", 22),
		Platform:     pf,
		ServiceName:  svcName,
		ExeName:      exeName,
		ConfName:     "frpc.toml",
		DisplayName:  spec.Name,
		ClientID:     spec.ClientID,
		RobotType:    RobotTypeLabel(spec.RobotType),
		Version:      vr.Version,
		IsClient:     true,
		FileList:     fileList,
		ExampleDir:   exampleDirFor(pf, spec.ClientID),
		LogName:      "frpc.log",
		LogDays:      spec.LogMaxDays,
		ServerTarget: fmt.Sprintf("%s:%d", spec.ServerAddr, spec.ServerPort),
	})
	if err != nil {
		return nil, err
	}

	files := assemble(pf,
		[]PkgFile{
			{Name: exeName, Data: binData, Exec: true},
			{Name: "frpc.toml", Data: tomlContent},
		},
		scripts,
		readme,
	)

	summary := fmt.Sprintf("客户端包 · %s(%s) · 代理 %d 条 · 目标 %s:%d",
		spec.Name, spec.ClientID, len(spec.Proxies), spec.ServerAddr, spec.ServerPort)
	// 机器人类型只在填了的时候追加，避免在生成记录里出现一长串「未分类」。
	if label := RobotTypeLabel(spec.RobotType); label != "" {
		summary = fmt.Sprintf("客户端包 · %s(%s) · %s · 代理 %d 条 · 目标 %s:%d",
			spec.Name, spec.ClientID, label, len(spec.Proxies), spec.ServerAddr, spec.ServerPort)
	}
	// 平台只在非默认时追加：存量与绝大多数包都是 Windows 的，
	// 给它们挨个加上后缀只会让生成记录变长，且不利于与历史记录对照。
	// 但**必须**在跨到别的平台时写出来 —— 否则「两个同名包内容不同」会变成悬案。
	if pf != DefaultPlatform {
		summary = fmt.Sprintf("%s · %s", summary, pf.Label())
	}

	// 文件名同理：默认平台保持原样（存量脚本与运维习惯都按这个名字找），
	// 非默认平台加后缀，避免同名覆盖 —— 同 clientID 的两个平台包
	// 下载到同一个目录时，后者会把前者悄悄盖掉。
	fileName := fmt.Sprintf("SinyeFRP-client-%s.zip", spec.ClientID)
	if pf != DefaultPlatform {
		fileName = fmt.Sprintf("SinyeFRP-client-%s-%s.zip", spec.ClientID, pf)
	}

	return &Package{
		FileName:  fileName,
		Kind:      "client",
		Version:   vr.Version,
		ExeSHA256: bins.ClientSHA256For(pf),
		Summary:   summary,
		Files:     files,
	}, nil
}

// exampleDirFor 给出 README 里的示例部署目录。
//
// 按平台分开：Windows 用盘符路径，Linux 用 /opt 下的约定位置。
// 写反了不只是难看 —— 运维会把示例路径直接粘到终端里执行，
// 在 Linux 上粘一个 C:\ 路径只会得到一句「No such file or directory」。
func exampleDirFor(p Platform, id string) string {
	if p.IsWindows() {
		return `C:\SinyeFRP\` + id
	}
	return "/opt/sinyefrp/" + id
}

// BuildServerPackage 生成服务器部署包（01-prd F11/F12/F15）。
func BuildServerPackage(ctx context.Context, bins *Binaries, spec *ServerSpec) (*Package, error) {
	spec.Normalize()
	if errs := spec.Validate(); len(errs) > 0 {
		return nil, joinErrs(errs)
	}

	tomlContent, err := RenderServerTOML(spec)
	if err != nil {
		return nil, err
	}
	// 服务器包目前只提供 Windows 版：仓库里没有 Linux 的 frps 二进制，
	// 而服务端包一旦走 Linux 就要连带处理 allowPorts 对应的防火墙语义差异。
	// 这里固定取默认平台，等补上二进制时把 ServerSpec 也加上 Platform 字段即可。
	pf := DefaultPlatform

	vr, err := bins.VerifyServerConfig(ctx, tomlContent)
	if err != nil {
		return nil, err
	}

	binData, err := bins.ServerBinaryFor(pf)
	if err != nil {
		return nil, err
	}

	svcName := pf.ServiceName("frps", fmt.Sprintf("port%d", spec.BindPort))
	scripts, err := renderScripts(pf, scriptVars{
		Role:        "服务器",
		ExeName:     pf.ServerExeName(),
		ConfName:    "frps.toml",
		TaskName:    svcName,
		DisplayName: fmt.Sprintf("frps (bind %d)", spec.BindPort),
		Version:     vr.Version,
		ConfFlag:    "-c",
		DataPaths:   []string{"frps.log", "logs"},
	})
	if err != nil {
		return nil, err
	}

	fileList := buildFileList(pf, []fileDesc{
		{pf.ServerExeName(), "FRP 服务端主程序"},
		{"frps.toml", "服务端配置（含 token 与 Dashboard 密码，属敏感文件）"},
	}, scripts)

	readme, err := renderReadme(ReadmeData{
		Role:        "服务器",
		Rule:        strings.Repeat("=", 22),
		Platform:    pf,
		ServiceName: svcName,
		ExeName:     pf.ServerExeName(),
		ConfName:    "frps.toml",
		DisplayName: fmt.Sprintf("frps (bind %d)", spec.BindPort),
		Version:     vr.Version,
		IsServer:    true,
		FileList:    fileList,
		ExampleDir:  exampleDirFor(pf, "frps"),
		LogName:     "frps.log",
		LogDays:     spec.LogMaxDays,
		BindPort:    spec.BindPort,
		DashPort:    spec.DashboardPort,
		DashAddr:    spec.DashboardAddr,
		ExtraPorts:  extraPortLines(spec),
	})
	if err != nil {
		return nil, err
	}

	files := assemble(pf,
		[]PkgFile{
			{Name: pf.ServerExeName(), Data: binData, Exec: true},
			{Name: "frps.toml", Data: tomlContent},
		},
		scripts,
		readme,
	)

	summary := fmt.Sprintf("服务器包 · bind %s:%d · Dashboard %s:%d",
		spec.BindAddr, spec.BindPort, spec.DashboardAddr, spec.DashboardPort)

	return &Package{
		FileName:  fmt.Sprintf("SinyeFRP-server-%d.zip", spec.BindPort),
		Kind:      "server",
		Version:   vr.Version,
		ExeSHA256: bins.ServerSHA256For(pf),
		Summary:   summary,
		Files:     files,
	}, nil
}

// ---------- 内部辅助 ----------

// taskName 构造计划任务名。
//
// 形如 SinyeFRP-frpc-<id>：前缀让运维一眼看出归属，后缀保证多实例并存时
// 互不覆盖（01-prd F13 要求重复安装更新原任务，而不是留下多个副本）。
func taskName(role, id string) string {
	safe := sanitizeForTask(id)
	name := "SinyeFRP-" + role + "-" + safe
	if len(name) > 200 {
		name = name[:200]
	}
	return name
}

// sanitizeForTask 把标识收敛到计划任务名允许的字符集。
func sanitizeForTask(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-', r == '_', r == '.':
			b.WriteRune('-')
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "default"
	}
	return out
}

type fileDesc struct {
	Name string
	Desc string
}

// scriptOrder 是生命周期脚本的固定展示顺序。
//
// 与生成顺序、README 描述顺序、ZIP 内排列顺序共用同一份来源：
// 运维在命令行里按 tab 看到的补全顺序，和文档里写的顺序完全一致。
//
// 双击入口排在最前：解压后目录里默认按名称排序，入口排在脚本之前，
// 现场人员一眼能看见「先点这个」。它同时出现在 ZIP 顺序里，
// 所以 ClientFileNames / ServerFileNames / PlatformFileNames 三个预览清单
// 都由这一个切片推导，加文件不会漏掉任何一处。
// ClientFileNames 返回客户端部署包内的文件清单，供界面预览（01-prd F10）。
//
// 清单随目标平台变化（Windows 是 .ps1 加双击入口，Linux 是 .sh 加 systemd unit），
// 因此必须带平台参数 —— 写死一份会让界面预览与实际产物不符，
// 而「预览对、下载不对」正是最难被发现的那一类偏差。
func ClientFileNames(p Platform) []string {
	names := p.ScriptFileNames()
	out := make([]string, 0, len(names)+4)
	out = append(out, p.ClientExeName(), "frpc.toml")
	out = append(out, names...)
	return append(out, "README.txt", "SHA256SUMS.txt")
}

// ServerFileNames 返回服务器部署包内的文件清单（01-prd F12）。
func ServerFileNames(p Platform) []string {
	names := p.ScriptFileNames()
	out := make([]string, 0, len(names)+4)
	out = append(out, p.ServerExeName(), "frps.toml")
	out = append(out, names...)
	return append(out, "README.txt", "SHA256SUMS.txt")
}

// PlatformFileNames 返回管理平台自身部署包内的文件清单（01-prd F18）。
func PlatformFileNames() []string {
	// 管理平台自身包目前只有 Windows 版：它的自启动依赖计划任务与
	// NTFS ACL，而凭据加密依赖 DPAPI，两者都没有 Linux 对应物。
	// 固定取 Windows 清单，等真要做 Linux 平台包时再引入参数。
	names := PlatformWindowsAMD64.ScriptFileNames()
	out := make([]string, 0, len(names)+4)
	out = append(out, appExeName, "config.toml")
	out = append(out, names...)
	return append(out, "README.txt", "SHA256SUMS.txt")
}

// JoinErrsText 把多条校验错误合并为多行文本，供 HTTP 400 响应直接呈现。
func JoinErrsText(errs []error) string {
	msgs := make([]string, 0, len(errs))
	for _, e := range errs {
		msgs = append(msgs, "- "+e.Error())
	}
	return "配置校验未通过：\n" + strings.Join(msgs, "\n")
}

// buildFileList 生成 README 中的文件清单段落。
// scriptDescriptions 返回某平台下各脚本在 README 文件清单里的一句话说明。
//
// 与平台绑定而不是共用一份：脚本名、参数风格（--json vs -Json）、
// 以及「需不需要提权」在两类系统上都不一样，共用一份必然有一边是错的。
func scriptDescriptions(p Platform) map[string]string {
	if p.IsWindows() {
		return map[string]string{
			launcherName:    "【双击这个】自动请求管理员权限并完成安装",
			"install.ps1":   "安装：校验文件 → 注册开机自启 → 立即启动（命令行方式，需管理员）",
			"uninstall.ps1": "卸载：移除开机自启 → 停止进程（默认保留配置与日志）",
			"start.ps1":     "启动",
			"stop.ps1":      "停止",
			"restart.ps1":   "重启",
			"status.ps1":    "查看状态（加 -Json 输出单行 JSON，便于监控采集）",
		}
	}
	return map[string]string{
		"install.sh":   "安装：校验文件 → 注册开机自启 → 立即启动（需 sudo）",
		"uninstall.sh": "卸载：移除开机自启 → 停止进程（默认保留配置与日志）",
		"start.sh":     "启动",
		"stop.sh":      "停止",
		"restart.sh":   "重启",
		"status.sh":    "查看状态（加 --json 输出单行 JSON，便于监控采集）",
		unitFileName:   "systemd 服务定义模板（路径占位符由 install.sh 安装时代入）",
	}
}

func buildFileList(p Platform, base []fileDesc, scripts map[string][]byte) string {
	// 脚本按固定顺序展示，与生成顺序一致，避免每次生成的 README 排列不同。
	order := p.ScriptFileNames()
	descOf := scriptDescriptions(p)

	var rows []fileDesc
	rows = append(rows, base...)
	for _, name := range order {
		if _, ok := scripts[name]; !ok {
			continue
		}
		rows = append(rows, fileDesc{name, descOf[name]})
	}

	width := 0
	for _, r := range rows {
		// 按**字符数**而不是字节数算列宽：包内现在有一个中文文件名
		// （一键安装.bat），它的字节长度是 13、字符数是 8，而 fmt 的
		// %-*s 是按字符补空格的。用 len() 会让这一行多顶出 5 列，
		// 整个文件清单的对齐就散了 —— 这份清单是现场人员最先看的东西，
		// 歪掉会显得交付很潦草。
		if n := len([]rune(r.Name)); n > width {
			width = n
		}
	}
	var b strings.Builder
	for i, r := range rows {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%-*s  %s", width, r.Name, r.Desc)
	}
	return b.String()
}

// extraPortLines 汇总服务器端可能需要额外放行的端口。
func extraPortLines(s *ServerSpec) []string {
	out := make([]string, 0, len(s.AllowPorts))
	for _, r := range s.AllowPorts {
		if r.Start == r.End {
			out = append(out, fmt.Sprintf("  %-6d 代理可用端口（allowPorts 区间）", r.Start))
		} else {
			out = append(out, fmt.Sprintf("  %d-%d   代理可用端口范围（allowPorts 区间）", r.Start, r.End))
		}
	}
	return out
}

// assemble 按固定顺序把主程序、配置、脚本、README 与校验文件组装成最终清单。
//
// 顺序刻意稳定：解压后的目录浏览顺序、README 的描述顺序与 ZIP 内顺序保持一致，
// 运维在命令行里 tab 补全时看到的就是文档里写的顺序。
func assemble(p Platform, head []PkgFile, scripts map[string][]byte, readme []byte) []PkgFile {
	files := make([]PkgFile, 0, len(head)+len(scripts)+2)

	// 文本文件按**目标平台**规范形态（Windows: BOM + CRLF；Linux: 无 BOM + LF），
	// 二进制原样保留。
	//
	// 只在这一个汇聚点做，不散到各个模板里：默认值、预览、下载三条出站路径
	// 都经过这里，而分散改法迟早会漏掉一个 —— 漏掉的那个会让包内文件与
	// SHA256SUMS.txt 对不上，症状是「装不上」而不是「格式不对」，很难查回来。
	//
	// 平台作为参数传入而不是读宿主：本平台跑在 Windows 上，却常常要产出
	// Linux 包，两者的文本规范恰好相反（详见 Platform.WantsBOM 的注释）。
	for _, f := range head {
		files = append(files, PkgFile{
			Name: f.Name,
			Data: normalizeText(p, f.Name, f.Data),
			Exec: f.Exec || p.NeedsExecBit(f.Name),
		})
	}

	for _, name := range p.ScriptFileNames() {
		if data, ok := scripts[name]; ok {
			files = append(files, PkgFile{
				Name: name,
				Data: normalizeText(p, name, data),
				Exec: p.NeedsExecBit(name),
			})
		}
	}
	files = append(files, PkgFile{Name: "README.txt", Data: normalizeText(p, "README.txt", readme)})

	// SHA256SUMS.txt 覆盖其前的全部文件，但不包含自己
	//（自校验在数学上不可能，标准做法是把它排除在外）。
	var sums strings.Builder
	for _, f := range files {
		fmt.Fprintf(&sums, "%s  %s\n", SumBytes(f.Data), f.Name)
	}
	files = append(files, PkgFile{Name: "SHA256SUMS.txt", Data: []byte(sums.String())})

	return files
}

// joinErrs 把多条校验错误合并成一条，便于一次性呈现给管理员。
func joinErrs(errs []error) error {
	msgs := make([]string, 0, len(errs))
	for _, e := range errs {
		msgs = append(msgs, e.Error())
	}
	return fmt.Errorf("%w\n- %s", ErrInvalid, strings.Join(msgs, "\n- "))
}
