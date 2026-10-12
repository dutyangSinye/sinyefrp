// Package auth 负责管理平台的身份认证：首次初始化、管理员登录、会话与登录限速。
//
// 对应 01-prd F01/F02：
//   - 管理员密码只保存带盐哈希（bcrypt）；
//   - 错误密码不透露账号是否存在；
//   - 连续失败有限速；
//   - 退出后会话立即失效。
package auth

import (
	"errors"
	"fmt"
	"strings"
	"unicode"

	"golang.org/x/crypto/bcrypt"
)

// bcryptCost 取 12：单次校验约百毫秒量级，可接受，且显著抬高离线爆破成本。
const bcryptCost = 12

// maxPasswordBytes 是 bcrypt 的硬限制。bcrypt 只使用前 72 字节，
// 超过部分会被静默丢弃，因此必须在录入时拒绝，而不是让用户以为长密码更安全。
const maxPasswordBytes = 72

// dummyHash 是一段合法的 bcrypt 哈希（明文为随机串，无实际账号对应）。
//
// 用途：当用户名不存在时仍然执行一次同代价的比对，使「账号不存在」与
// 「密码错误」的耗时一致，避免通过响应时间枚举出有效用户名（01-prd F02）。
var dummyHash = []byte("$2a$12$C6UzMDM.H6dfI/f/IKcEe.oVQ4hC4Rkfr3nJ0jZ7oVQ0hQ0hQ0hQ0")

// 账号与口令的输入约束。
const (
	MinUsernameLen = 3
	MaxUsernameLen = 32
	MinPasswordLen = 10
)

var (
	// ErrWeakUsername 表示用户名不符合格式要求。
	ErrWeakUsername = errors.New("用户名需为 3-32 位，仅可包含字母、数字、点、下划线和连字符")
	// ErrWeakPassword 表示口令长度不符合要求。
	ErrWeakPassword = fmt.Errorf("密码长度需为 %d-%d 字节", MinPasswordLen, maxPasswordBytes)
	// ErrWeakPasswordClasses 表示口令字符种类过于单一。
	ErrWeakPasswordClasses = errors.New("密码需至少包含大写字母、小写字母、数字、符号中的两类")
)

// HashPassword 生成口令哈希。
func HashPassword(plain string) (string, error) {
	if err := ValidatePassword(plain); err != nil {
		return "", err
	}
	h, err := bcrypt.GenerateFromPassword([]byte(plain), bcryptCost)
	if err != nil {
		return "", fmt.Errorf("生成密码哈希失败: %w", err)
	}
	return string(h), nil
}

// VerifyPassword 比对口令与哈希。哈希非法或口令不符都返回 false。
func VerifyPassword(hash, plain string) bool {
	if hash == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}

// burnTime 在账号不存在时消耗与真实校验相当的时间，掩盖账号是否存在。
func burnTime(plain string) {
	_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(plain))
}

// ValidateUsername 校验用户名格式。
//
// 限制为 ASCII 可打印安全字符：用户名会进入审计日志与界面，
// 不做多语言支持（01-prd §10.2），从而避免同形字符导致的冒充。
func ValidateUsername(name string) error {
	name = strings.TrimSpace(name)
	if len(name) < MinUsernameLen || len(name) > MaxUsernameLen {
		return ErrWeakUsername
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '_', r == '-':
			if i == 0 {
				return ErrWeakUsername
			}
		default:
			return ErrWeakUsername
		}
	}
	return nil
}

// ValidatePassword 校验口令强度。
func ValidatePassword(plain string) error {
	n := len([]byte(plain))
	if n < MinPasswordLen || n > maxPasswordBytes {
		return ErrWeakPassword
	}
	// 要求至少包含两类字符，挡住 "aaaaaaaaaa" 这类可预测口令。
	var hasLower, hasUpper, hasDigit, hasSymbol bool
	for _, r := range plain {
		switch {
		case unicode.IsLower(r):
			hasLower = true
		case unicode.IsUpper(r):
			hasUpper = true
		case unicode.IsDigit(r):
			hasDigit = true
		default:
			hasSymbol = true
		}
	}
	classes := 0
	for _, ok := range []bool{hasLower, hasUpper, hasDigit, hasSymbol} {
		if ok {
			classes++
		}
	}
	if classes < 2 {
		return ErrWeakPasswordClasses
	}
	return nil
}
