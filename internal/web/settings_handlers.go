package web

import (
	"net/http"
	"os"
	"strconv"
	"time"

	"sinyefrp/internal/node"
)

// nodeConfigRequest 是设置页提交的节点配置。
//
// Password 为空表示「保持原密码」：界面只拿到 password_configured，
// 因此管理员改超时或改地址时无需重新输入密码。
type nodeConfigRequest struct {
	Name           string `json:"name"`
	APIURL         string `json:"api_url"`
	Username       string `json:"username"`
	Password       string `json:"password"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	Enabled        *bool  `json:"enabled"`
}

func (r nodeConfigRequest) toSettings() node.Settings {
	enabled := true
	if r.Enabled != nil {
		enabled = *r.Enabled
	}
	return node.Settings{
		Name:           r.Name,
		APIURL:         r.APIURL,
		Username:       r.Username,
		Password:       r.Password,
		TimeoutSeconds: r.TimeoutSeconds,
		Enabled:        enabled,
	}
}

// handleGetNodeConfig 返回节点配置（不含凭据）与配置来源。
func (s *Server) handleGetNodeConfig(w http.ResponseWriter, r *http.Request) {
	eff := s.nodes.Current()

	persisted, err := s.nodes.Persisted(r.Context())
	if err != nil {
		s.log.Error("查询节点记录失败", "err", err)
		writeError(w, http.StatusInternalServerError, errKindInternal, "服务内部错误")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"node":      eff.Redacted(),
		"source":    eff.Source,
		"persisted": persisted,
		"overridden": map[string]any{
			// 命令行参数或环境变量覆盖生效时为 true。此时保存配置不会改变
			// 实际的采集目标，界面必须提示，否则会出现「保存成功但没生效」。
			"active": s.nodes.Overridden(),
		},
	})
}

// handleSaveNodeConfig 保存节点配置并立即生效。
func (s *Server) handleSaveNodeConfig(w http.ResponseWriter, r *http.Request) {
	var req nodeConfigRequest
	if !s.decodeJSON(w, r, &req) {
		return
	}

	user := currentUser(r.Context())
	actor := ""
	if user != nil {
		actor = user.Username
	}

	in := req.toSettings()
	needsRestart, err := s.nodes.Save(r.Context(), in, actor, clientIP(r))
	if err != nil {
		s.audit(r, "node.save", in.Name, "fail", err.Error())
		writeError(w, http.StatusBadRequest, errKindBadRequest, err.Error())
		return
	}

	eff := s.nodes.Current()
	s.log.Info("节点配置已更新", "name", eff.Name, "api_url", eff.APIURL, "actor", actor)

	writeJSON(w, http.StatusOK, map[string]any{
		"node":   eff.Redacted(),
		"source": eff.Source,
		// needs_restart 为 true 表示本进程存在临时覆盖，保存的配置要重启后才生效。
		"needs_restart": needsRestart,
		"message":       saveMessage(needsRestart),
	})
}

func saveMessage(needsRestart bool) string {
	if needsRestart {
		return "配置已保存。当前进程被命令行参数或环境变量覆盖，需重启管理平台后该配置才会生效。"
	}
	return "配置已保存并立即生效。"
}

// handleTestNode 用给定配置试连一次，不落库、不改变生效配置（01-prd F03）。
func (s *Server) handleTestNode(w http.ResponseWriter, r *http.Request) {
	var req nodeConfigRequest
	if !s.decodeJSON(w, r, &req) {
		return
	}

	res := s.nodes.Test(r.Context(), req.toSettings())

	// 审计只记结论与目标地址，不记密码。
	result := "fail"
	if res.OK {
		result = "ok"
	}
	s.audit(r, "node.test", res.TargetAPIURL, result, res.Kind)

	writeJSON(w, http.StatusOK, res)
}

// handleDataStatus 返回当前数据库的落盘状态（01-prd F16 补充）。
//
// 这个接口的存在理由来自一次真实的排障：现场反馈「重启后下午签发的记录没了」，
// 而第一件必须确认、却完全没有界面可看的事就是 —— **平台到底在用哪个数据库文件**。
// 当时只能靠翻磁盘、比时间戳、猜相对路径的解析基准，绕了很多弯路。
//
// 因此这里把三件事直接摆到界面上：
//   - 主库与 WAL 的**绝对路径**（相对路径按配置文件目录解析，不是 CWD）；
//   - 各自的实际字节数与最后写入时间；
//   - WAL 是否偏大（相对主库）。WAL 里是「尚未回写主库、且暂无第二副本」的数据，
//     它越大，一次异常掉电的暴露面就越大。
func (s *Server) handleDataStatus(w http.ResponseWriter, _ *http.Request) {
	dbPath := s.dbPath
	main := fileStat(dbPath)
	wal := fileStat(dbPath + "-wal")

	// 偏大判定：WAL 超过主库体积、且绝对值超过 1 MiB 时提示。
	// 1 MiB 这个阈值是经验值 —— 本项目的写入量极小，正常 checkpoint 后
	// WAL 通常只有几十 KB；涨到 MiB 级说明很久没有过正常关闭。
	walHeavy := wal.Size > main.Size && wal.Size > 1<<20

	writeJSON(w, http.StatusOK, map[string]any{
		"database": map[string]any{
			"path":        dbPath,
			"size_bytes":  main.Size,
			"size_text":   humanBytes(main.Size),
			"modified_at": main.ModifiedAt,
			"exists":      main.Exists,
		},
		"wal": map[string]any{
			"path":        dbPath + "-wal",
			"size_bytes":  wal.Size,
			"size_text":   humanBytes(wal.Size),
			"modified_at": wal.ModifiedAt,
			"exists":      wal.Exists,
			"heavy":       walHeavy,
		},
		"hint": dataStatusHint(walHeavy, wal.Exists),
	})
}

// fileStatResult 是文件探测结果。
type fileStatResult struct {
	Exists     bool
	Size       int64
	ModifiedAt *time.Time
}

func fileStat(path string) fileStatResult {
	info, err := os.Stat(path)
	if err != nil {
		return fileStatResult{}
	}
	mt := info.ModTime()
	return fileStatResult{Exists: true, Size: info.Size(), ModifiedAt: &mt}
}

// humanBytes 把字节数转成便于阅读的形式。
//
// 刻意用 1024 进制并在单位上标 KiB/MiB：与「设置页显示的文件大小」保持
// 和 Windows 资源管理器一致的量纲，避免运维按 1000 进制换算后对不上。
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	units := []string{"KiB", "MiB", "GiB"}
	value := float64(n)
	idx := -1
	for value >= unit && idx < len(units)-1 {
		value /= unit
		idx++
	}
	return strconv.FormatFloat(value, 'f', 1, 64) + " " + units[idx]
}

// dataStatusHint 给出面向运维的一句话解释。
//
// 文案是交付面的一部分：这里说的每一句都要和 README、脚本提示口径一致。
func dataStatusHint(walHeavy, walExists bool) string {
	switch {
	case walHeavy:
		return "预写日志（WAL）明显大于主库，说明平台已很久没有正常关闭过。" +
			"WAL 中尚未回写主库的记录暂无第二副本，遇异常掉电可能丢失。" +
			"建议执行 .\\restart.ps1 触发一次正常关闭与回写。"
	case walExists:
		return "数据库工作正常。预写日志（WAL）中可能含少量尚未回写主库的最新记录，" +
			"属正常现象；正常关闭或停机时平台会自动回写。"
	default:
		return "数据库工作正常，当前无需回写的预写日志。"
	}
}

// handleAudit 返回最近的审计记录（01-prd F16）。
func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}

	items, err := s.st.ListAudit(r.Context(), limit)
	if err != nil {
		s.log.Error("查询审计记录失败", "err", err)
		writeError(w, http.StatusInternalServerError, errKindInternal, "服务内部错误")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}
