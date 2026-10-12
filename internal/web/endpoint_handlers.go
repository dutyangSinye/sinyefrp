package web

import (
	"errors"
	"fmt"
	"net/http"

	"sinyefrp/internal/endpoint"
)

// endpointRequest 是设置页提交的客户端接入点配置。
//
// Token 为空表示「保持原 token」：界面只拿得到 token_configured，
// 管理员改地址或改端口时不该被迫重新输入 token。
// 需要真正清空（例如 frps 侧没有配 auth.token）时传 clear_token。
type endpointRequest struct {
	ClientAddr string `json:"client_addr"`
	ClientPort int    `json:"client_port"`
	Token      string `json:"token"`
	ClearToken bool   `json:"clear_token"`
}

// handleGetEndpoint 返回客户端接入点配置，token 只暴露「是否已配置」。
func (s *Server) handleGetEndpoint(w http.ResponseWriter, r *http.Request) {
	ep, err := endpoint.Load(r.Context(), s.st)

	decryptFailed := false
	if err != nil {
		var de *endpoint.DecryptError
		if !errors.As(err, &de) {
			s.log.Error("读取客户端接入点失败", "err", err)
			writeError(w, http.StatusInternalServerError, errKindInternal, "服务内部错误")
			return
		}
		// 地址与端口仍然可用，只是 token 解不开。照常返回内容，
		// 由前端提示「token 需要重新填写」，而不是把整个区块变成错误页。
		decryptFailed = true
		s.log.Warn("接入点 token 无法解密，需要重新填写", "err", err)
	}

	body := ep.Redacted()
	body["token_decrypt_failed"] = decryptFailed
	writeJSON(w, http.StatusOK, body)
}

// handleSaveEndpoint 保存客户端接入点配置。
//
// 该配置只影响之后生成的部署包，不涉及采集链路，因此没有 needs_restart 一说。
func (s *Server) handleSaveEndpoint(w http.ResponseWriter, r *http.Request) {
	var req endpointRequest
	if !s.decodeJSON(w, r, &req) {
		return
	}

	user := currentUser(r.Context())
	actor := ""
	if user != nil {
		actor = user.Username
	}

	ep, err := endpoint.Save(r.Context(), s.st, endpoint.SaveInput{
		Addr:       req.ClientAddr,
		Port:       req.ClientPort,
		Token:      req.Token,
		ClearToken: req.ClearToken,
	})
	if err != nil {
		// 审计只记动作与结论，不记 token 明文（01-prd F17）。
		s.audit(r, "endpoint.save", req.ClientAddr, "fail", err.Error())
		writeError(w, http.StatusBadRequest, errKindBadRequest, err.Error())
		return
	}

	target := "未配置"
	if ep.Configured() {
		target = fmt.Sprintf("%s:%d", ep.Addr, ep.PortOrDefault())
	}
	s.audit(r, "endpoint.save", target, "ok", "")
	s.log.Info("客户端接入点已更新",
		"addr", ep.Addr, "port", ep.Port, "token_set", ep.Token != "", "actor", actor)

	writeJSON(w, http.StatusOK, map[string]any{
		"endpoint": ep.Redacted(),
		"message":  "接入点已保存。之后生成的客户端包会默认使用该地址、端口与 token。",
	})
}
