// Package secret 负责管理平台内部敏感值的加密与解密。
//
// 对应 01-prd §8.5：SQLite 中保存的外部凭据（FRP Dashboard 密码、FRP token）
// 必须加密保存，管理员密码只保存带盐哈希。
//
// 存储格式为带前缀的字符串，便于区分「已加密」与「历史明文」：
//
//	enc:v1:<base64(密文)>
//
// 解密时若发现没有前缀，则按明文原样返回并置 Legacy 标记，
// 由调用方决定是否提示运维重新保存，而不是直接报错导致服务不可用。
package secret

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// 存储前缀。v1 对应当前实现，将来更换算法时递增，便于平滑迁移。
const (
	prefix   = "enc:v1:"
	prefixV0 = "enc:"
)

// ErrUnsupported 表示当前平台不支持本地凭据加密。
var ErrUnsupported = errors.New("当前平台不支持凭据加密")

// Result 是解密结果。
type Result struct {
	// Plain 是解密后的明文。
	Plain string
	// Legacy 为 true 表示存储值没有加密前缀，是历史明文。
	Legacy bool
}

// Encrypt 加密明文并返回带前缀的存储值。
//
// 空字符串直接返回空串：不加密空值，也避免把「未配置」写成一段密文。
func Encrypt(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	sealed, err := seal([]byte(plain))
	if err != nil {
		return "", fmt.Errorf("加密敏感值失败: %w", err)
	}
	return prefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt 解密存储值。
//
// 传入空串返回空串；无前缀视为历史明文（Legacy=true）。
func Decrypt(stored string) (Result, error) {
	stored = strings.TrimSpace(stored)
	if stored == "" {
		return Result{}, nil
	}

	if !strings.HasPrefix(stored, prefix) {
		// enc:<...> 是更早的预留格式，尚未启用过；与无前缀一并按历史值处理。
		if strings.HasPrefix(stored, prefixV0) {
			return Result{}, fmt.Errorf("不支持的凭据存储格式: %q", prefixV0)
		}
		return Result{Plain: stored, Legacy: true}, nil
	}

	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, prefix))
	if err != nil {
		return Result{}, fmt.Errorf("凭据存储值不是合法 base64: %w", err)
	}
	plain, err := open(raw)
	if err != nil {
		return Result{}, fmt.Errorf("解密敏感值失败: %w", err)
	}
	return Result{Plain: string(plain)}, nil
}

// IsEncrypted 判断存储值是否已加密。
func IsEncrypted(stored string) bool {
	return strings.HasPrefix(strings.TrimSpace(stored), prefix)
}
