package web

import (
	"fmt"
	"net/http"
	"strings"

	"sinyefrp/internal/buildinfo"
	"sinyefrp/internal/pack"
)

// 本文件实现「管理平台自身部署包」的预览与下载（01-prd F18）。
//
// 与其他两个包的关键区别：这里没有任何 FRP 配置参与校验。配置的权威是
// internal/config 的 Validate —— 它同时就是启动路径上的那道校验，
// 因此不存在「生成时通过、装上去启动失败」的空档。

// platformSpec 依据当前生效配置构造平台包参数。
//
// 生成参数一律取自**当前部署**而不是写死的默认值：把一个监听 7801 的部署
// 生成的包装成 7800，会让管理员在毫不知情的情况下换了端口 —— 而端口不对
// 恰恰是最不容易立刻发现的问题（管理台打不开时，第一反应通常是服务没起来）。
func (s *Server) platformSpec() pack.PlatformSpec {
	spec := pack.PlatformSpec{
		Version:    buildinfo.Version,
		ListenAddr: s.cfg.Server.ListenAddr,
		Port:       s.cfg.Server.Port,
		NodeName:   s.cfg.Node.Name,
		NodeAPIURL: s.cfg.Node.APIURL,
		LogLevel:   s.cfg.Log.Level,
	}
	// FRP 版本只用于 README 里的一句说明。这里读的是已探测并缓存的值，
	// 刻意不触发探测：探测要跑一次子进程，不该让「打开打包页」多等一次。
	if s.bins != nil {
		spec.FRPVersion = s.bins.Version()
	}
	return spec
}

// platformWarnings 汇总平台自身部署包的提醒项。
//
// 这几条都属于「装上去才会发现、而发现时已经付出了代价」的问题，
// 因此必须在下载之前就说清楚，而不是写进包内 README 等运维自己读到。
func (s *Server) platformWarnings(spec *pack.PlatformSpec) []string {
	return []string{
		fmt.Sprintf("包内 config.toml 的监听端口是 %d，取自当前部署。若目标机器上已有管理台"+
			"占用该端口，新实例会启动失败。同一台机器不建议部署两个管理台：它们会各自向"+
			"同一个 FRP Dashboard 采集，界面上的数字会来回跳。", spec.Port),

		"包内 config.toml 的节点地址是 " + spec.NodeAPIURL + "。若目标机器与 frps 不同机，" +
			"请在安装后到「节点配置」改成本机能访问到的地址。",

		"这是一次全新部署：解压后的 data\\ 目录是空的，安装完成后需要重新走一遍初始化向导。" +
			"若要从现有机器迁移，请先把旧目录的 data\\ 整个复制过去。注意数据库里的凭据用 " +
			"DPAPI 机器范围加密，换机器后解不开，需要在界面上重新填写节点密码与接入点 token。",
	}
}

// platformSummary 生成包的非敏感摘要（写入审计与生成记录）。
func platformSummary(spec *pack.PlatformSpec) string {
	return fmt.Sprintf("平台自身包 · 监听 %s:%d · 版本 %s",
		spec.ListenAddr, spec.Port, spec.Version)
}

// handlePlatformPreview 预览平台自身部署包（01-prd F18）。
//
// 与其他两个包不同，这里返回的是包内 config.toml 的**全文**，而不是遮蔽后的草稿：
// 该文件里既没有 token 也没有密码（节点密码本来就不写进文件，改用 DPAPI 加密后存进
// SQLite）。让管理员在下载前把即将发出去的监听地址与节点地址看清楚，比遮蔽它更有价值——
// 遮蔽一件本来就不敏感的东西，只会制造「里面藏了什么」的错觉。
func (s *Server) handlePlatformPreview(w http.ResponseWriter, r *http.Request) {
	noStore(w)

	if !s.self.Available() {
		writeError(w, http.StatusServiceUnavailable, errKindUnavailable,
			"无法读取管理平台自身可执行文件，暂时不能生成平台部署包："+s.self.Reason())
		return
	}

	spec := s.platformSpec()
	conf, err := pack.RenderPlatformConfig(&spec)
	if err != nil {
		s.log.Error("渲染平台配置失败", "err", err)
		writeError(w, http.StatusInternalServerError, errKindInternal, "渲染配置失败")
		return
	}

	writeJSON(w, http.StatusOK, pkgPreviewResp{
		FileName: pack.PlatformFileName(spec.Version),
		// 预览是给人看的，去掉 UTF-8 BOM —— 它在 ZIP 里必须留着（记事本要靠它
		// 认出 UTF-8），但在网页的 <pre> 里只会渲染成一个看不见的方块。
		TOML:       strings.TrimPrefix(string(conf), "\ufeff"),
		Warnings:   s.platformWarnings(&spec),
		FRPVersion: spec.FRPVersion,
		Summary:    platformSummary(&spec),
		Files:      pack.PlatformFileNames(),
		SizeBytes:  s.self.Size() + len(conf),
	})
}

// handlePlatformDownload 生成并流式下载平台自身部署包（01-prd F18）。
//
// 包内主程序就是「正在运行的自己」：管理员拿到的永远是与当前界面同一版的 exe，
// 不存在「界面是新版、包里是旧版」的可能。
func (s *Server) handlePlatformDownload(w http.ResponseWriter, r *http.Request) {
	if !s.self.Available() {
		writeError(w, http.StatusServiceUnavailable, errKindUnavailable,
			"无法读取管理平台自身可执行文件，暂时不能生成平台部署包："+s.self.Reason())
		return
	}

	spec := s.platformSpec()
	pkg, err := pack.BuildPlatformPackage(s.self, &spec)
	if err != nil {
		s.recordPkgFailure(r, "platform", "", err)
		s.writeBuildError(w, err)
		return
	}

	// 记账在写出之前：ZIP 一旦开始流向客户端就无法回滚（01-prd F16）。
	s.recordPkgSuccess(r, pkg, "", "SinyeFRP 管理平台 "+spec.Version)
	s.streamPackage(w, pkg)
}
