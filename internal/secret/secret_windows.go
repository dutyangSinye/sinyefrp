//go:build windows

package secret

import (
	"fmt"
	"syscall"
	"unsafe"
)

// DPAPI（CryptProtectData / CryptUnprotectData）相关绑定。
//
// 采用机器范围（CRYPTPROTECT_LOCAL_MACHINE）而非用户范围，原因：
// 01-prd F13 要求管理平台与 frps 以 SYSTEM 身份通过计划任务自启动，
// 而运维排障时又常以管理员身份手工运行同一个 exe。用户范围的数据绑定
// 到创建它的账户，跨身份解密会直接失败，导致「自启动能跑、手工跑报错」
// 这类难排查的问题。机器范围牺牲「防同机其他用户」换成「身份无关」，
// 因此数据库文件必须依赖 NTFS ACL 限制访问（部署脚本会设置）。
const cryptProtectLocalMachine = 0x4

// CRYPTPROTECT_UI_FORBIDDEN：服务进程不得弹窗，必须静默失败。
const cryptProtectUIForbidden = 0x1

var (
	crypt32  = syscall.NewLazyDLL("crypt32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	procCryptProtectData   = crypt32.NewProc("CryptProtectData")
	procCryptUnprotectData = crypt32.NewProc("CryptUnprotectData")
	procLocalFree          = kernel32.NewProc("LocalFree")
)

// dataBlob 对应 Windows 的 DATA_BLOB 结构。
type dataBlob struct {
	cbData uint32
	pbData *byte
}

func newBlob(d []byte) *dataBlob {
	if len(d) == 0 {
		return &dataBlob{}
	}
	return &dataBlob{cbData: uint32(len(d)), pbData: &d[0]}
}

// bytes 复制出 Go 侧切片，调用方随后负责 LocalFree 原内存。
func (b *dataBlob) bytes() []byte {
	if b.pbData == nil || b.cbData == 0 {
		return nil
	}
	out := make([]byte, b.cbData)
	copy(out, unsafe.Slice(b.pbData, b.cbData))
	return out
}

func seal(plain []byte) ([]byte, error) {
	in := newBlob(plain)
	var out dataBlob

	ret, _, err := procCryptProtectData.Call(
		uintptr(unsafe.Pointer(in)), // pDataIn
		0,                           // szDataDescr（不写入描述，避免把 JSON 字段名留在密文里）
		0,                           // pOptionalEntropy
		0,                           // pvReserved
		0,                           // pPromptStruct
		uintptr(cryptProtectLocalMachine|cryptProtectUIForbidden),
		uintptr(unsafe.Pointer(&out)),
	)
	if ret == 0 {
		if err == syscall.Errno(0) {
			err = fmt.Errorf("CryptProtectData 返回失败")
		}
		return nil, err
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))

	return out.bytes(), nil
}

func open(cipher []byte) ([]byte, error) {
	if len(cipher) == 0 {
		return nil, nil
	}

	in := newBlob(cipher)
	var out dataBlob

	ret, _, err := procCryptUnprotectData.Call(
		uintptr(unsafe.Pointer(in)), // pDataIn
		0,                           // ppszDataDescr
		0,                           // pOptionalEntropy
		0,                           // pvReserved
		0,                           // pPromptStruct
		uintptr(cryptProtectUIForbidden),
		uintptr(unsafe.Pointer(&out)),
	)
	if ret == 0 {
		if err == syscall.Errno(0) {
			err = fmt.Errorf("CryptUnprotectData 返回失败")
		}
		return nil, err
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))

	return out.bytes(), nil
}

// Available 报告当前平台是否支持凭据加密。
func Available() bool { return true }
