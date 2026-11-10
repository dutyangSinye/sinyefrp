// Package ipgeo 提供离线的 IP 归属地查询。
//
// 为什么不用在线查询服务：管理平台部署在内网/环回，网络原则是
// 「不依赖任何外网服务」；更关键的是，把客户端的公网 IP 逐条发给第三方
// 查询接口，等于把运营拓扑交出去 —— 客户端列表本身就是这台服务器最值钱的
// 数据。ip2region（github.com/lionsoul2014/ip2region）完全离线，数据文件
// 由 scripts\fetch-frp.ps1 与 FRP 二进制一起拉取，构建期内嵌进 exe。
//
// 为什么内嵌而不是运行时读磁盘：归属地查询发生在「列表页每一次刷新」，
// 依赖一个磁盘文件意味着部署包要多带一个文件、丢了这个文件就是全列空白
// 而没有任何报错 —— 内嵌则从编译期就焊死，缺数据的症状（列全为「—」）
// 也比「文件路径不对」好排查得多。代价是 exe 增大约 11 MB，可接受。
package ipgeo

import (
	"embed"
	"net"
	"strings"
	"sync"

	"github.com/lionsoul2014/ip2region/binding/golang/xdb"
)

//go:embed data
var dataFS embed.FS

// xdbFile 是数据文件在 data 目录里的固定名字。
//
// data 目录与 internal\pack\assets 同源：go:embed 不接受空目录，
// 占位文件保证没拉取数据时也能编译（fetch-frp.ps1 会把真实数据放进来）。
// 缺数据时 Search 恒返回空串，界面显示「—」—— 归属地是锦上添花的展示列，
// 任何故障都不允许把它放大成页面报错。
const xdbFile = "data/ip2region_v4.xdb"

var (
	once     sync.Once
	searcher *xdb.Searcher
	loadErr  error
	// searchMu 保护 searcher：xdb.Searcher.Search 会写自身的 ioCount
	// 字段（哪怕全量 buffer 模式也写），并发调用是数据竞争，
	// 必须串行。查询本身是纯内存操作（微秒级），串行不构成瓶颈。
	searchMu sync.Mutex
)

func load() (*xdb.Searcher, error) {
	once.Do(func() {
		cBuff, err := xdb.LoadContentFromFS(dataFS, xdbFile)
		if err != nil {
			loadErr = err
			return
		}
		header, err := xdb.LoadHeaderFromBuff(cBuff)
		if err != nil {
			loadErr = err
			return
		}
		version, err := xdb.VersionFromHeader(header)
		if err != nil {
			loadErr = err
			return
		}
		searcher, loadErr = xdb.NewWithBuffer(version, cBuff)
	})
	if loadErr != nil {
		return nil, loadErr
	}
	return searcher, nil
}

// Search 返回 IP 的归属地，形如「中国 广东省 深圳 电信」。
//
// 契约（与 01-prd F26 的本地端口同一套克制原则 —— 查不出来就留白，
// 绝不用一个似是而非的值冒充）：
//   - 归属不明 / 内网 IP 返回「内网」；回环、链路本地同理；
//   - 非法 IP、IPv6（未内嵌 v6 数据）、数据文件缺失或损坏、查询失败，
//     一律返回空串 —— 调用方直接把空串渲染成「—」，不区分原因。
//
// 不做结果缓存：查询是微秒级内存操作，而客户端列表每 10 秒至多几个 IP，
// 缓存省下的时间远小于它引入的状态管理成本。
func Search(ipStr string) string {
	ip := net.ParseIP(strings.TrimSpace(ipStr))
	if ip == nil {
		return ""
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
		return "内网"
	}
	if ip.To4() == nil {
		// IPv6 没有内嵌数据文件：与其硬查一个版本不匹配的表，
		// 不如明确留白。补 v6 数据（data/ip2region_v6.xdb）后此处放开。
		return ""
	}

	s, err := load()
	if err != nil {
		return ""
	}

	searchMu.Lock()
	region, err := s.Search(ipStr)
	searchMu.Unlock()
	if err != nil || region == "" {
		return ""
	}

	// xdb 的输出是「国家|区域|省份|城市|ISP」，缺项以 0 占位；
	// 直接显示会把「0 0 0」漏进界面，这里把占位段滤掉后用空格连接。
	keep := make([]string, 0, 5)
	for _, p := range strings.Split(region, "|") {
		if p != "" && p != "0" {
			keep = append(keep, p)
		}
	}
	return strings.Join(keep, " ")
}
