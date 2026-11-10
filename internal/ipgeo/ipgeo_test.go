package ipgeo

import (
	"strings"
	"testing"
)

// TestSearchLocalNet 不依赖数据文件：内网与非法输入的契约在任何部署形态下都必须成立。
func TestSearchLocalNet(t *testing.T) {
	cases := map[string]string{
		"127.0.0.1":    "内网",
		"192.168.1.5":  "内网",
		"10.0.0.8":     "内网",
		"169.254.3.2":  "内网",
		"not-an-ip":    "",
		"":             "",
		"999.1.1.1":    "",
		"2408:873c::1": "", // IPv6 数据未内嵌，明确留白
	}
	for in, want := range cases {
		if got := Search(in); got != want {
			t.Errorf("Search(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSearchPublicIP 用页面上真实出现过的公网 IP 验证查询链路与输出格式。
//
// 若数据文件尚未就绪（clone 后没跑 scripts\fetch-frp.ps1），跳过 ——
// 测试不应该因为「没拉数据」而红，那是部署状态而不是代码缺陷。
func TestSearchPublicIP(t *testing.T) {
	if _, err := load(); err != nil {
		t.Skipf("数据文件未就绪（先跑 scripts/fetch-frp.ps1）: %v", err)
	}

	// 一律取公开的公共 DNS 地址：归属地长期稳定，且不把任何客户/生产网络的
	// 出口 IP 写进仓库（本仓库私有，但客户 IP 属于运营信息，不该顺手上传）。
	ips := []string{
		"114.114.114.114", // 114 DNS
		"223.5.5.5",       // 阿里 DNS
		"119.29.29.29",    // 腾讯 DNS
		"8.8.8.8",         // Google DNS
	}
	for _, ip := range ips {
		got := Search(ip)
		if got == "" {
			t.Errorf("Search(%q) 返回空串：查询链路或数据覆盖有问题", ip)
			continue
		}
		// 输出契约：占位段已滤除 ⇒ 不会出现竖线分隔与孤立的 0。
		if strings.ContainsAny(got, "|") {
			t.Errorf("Search(%q) = %q：未滤除的原始分隔符", ip, got)
		}
		for _, seg := range strings.Fields(got) {
			if seg == "0" {
				t.Errorf("Search(%q) = %q：占位段 0 泄漏进输出", ip, got)
			}
		}
	}
}
