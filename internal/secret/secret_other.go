//go:build !windows

// 非 Windows 平台不提供凭据加密实现。
//
// 本项目目标平台是 Windows（Windows 10/11 与 Windows Server 2019+），
// 此文件只用于保证 `go vet ./...` 与交叉编译在开发机上仍可通过，
// 真跑到非 Windows 上会在保存凭据时明确报错，而不是静默降级成明文。

package secret

func seal([]byte) ([]byte, error) { return nil, ErrUnsupported }

func open([]byte) ([]byte, error) { return nil, ErrUnsupported }

// Available 报告当前平台是否支持凭据加密。
func Available() bool { return false }
