package frpapi

import (
	"context"
	"errors"
	"fmt"
	"net"
)

// Kind 是错误分类，供界面区分展示。
//
// 该设计的直接来源是 01-prd KR5：FRP API 不可达、认证失败或配置错误时，
// 界面必须明确显示原因，不能把「未知」错误标记为客户端离线。
// 因此错误必须携带分类，而不是仅有一句文本。
type Kind string

const (
	// KindUnreachable 表示无法建立连接（frps 未运行、端口未监听、地址写错）。
	KindUnreachable Kind = "unreachable"
	// KindTimeout 表示连接或读取超时。
	KindTimeout Kind = "timeout"
	// KindUnauthorized 表示 401，用户名或密码错误。
	KindUnauthorized Kind = "unauthorized"
	// KindNotFound 表示 404，资源不存在。
	KindNotFound Kind = "not_found"
	// KindServerError 表示 5xx。
	KindServerError Kind = "server_error"
	// KindBadResponse 表示响应无法解析，通常意味着接口形态与预期不符。
	KindBadResponse Kind = "bad_response"
	// KindIncompatible 表示 FRP 版本不在正式支持范围内。
	KindIncompatible Kind = "incompatible"
)

// Error 是带分类的 FRP API 错误。
type Error struct {
	Kind       Kind
	StatusCode int
	Message    string
	Err        error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Err)
	}
	return e.Message
}

func (e *Error) Unwrap() error { return e.Err }

// IsKind 判断错误是否属于指定分类。
func IsKind(err error, k Kind) bool {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind == k
	}
	return false
}

// classifyTransportError 把底层网络错误归入可达性或超时两类。
func classifyTransportError(err error) *Error {
	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{
			Kind:    KindTimeout,
			Message: "连接 FRP Dashboard 超时",
			Err:     err,
		}
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return &Error{
			Kind:    KindTimeout,
			Message: "连接 FRP Dashboard 超时",
			Err:     err,
		}
	}

	return &Error{
		Kind:    KindUnreachable,
		Message: "无法连接 FRP Dashboard，请确认 frps 正在运行且已启用 webServer",
		Err:     err,
	}
}
