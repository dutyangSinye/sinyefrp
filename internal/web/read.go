package web

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"sinyefrp/internal/collector"
	"sinyefrp/internal/endpoint"
	"sinyefrp/internal/frpapi"
	"sinyefrp/internal/pack"
)

// ---------- 响应模型 ----------
//
// 这一组响应都直接来自 FRP Dashboard，天然只有 clientID 这类技术标识 ——
// 管理员看到一个 "9f3a1c7e..." 无法判断是哪台机器。所以凡是列出客户端/代理的
// 地方，都要附上台账里管理员填写的「显示名称」（01-prd F19）。
// 关联由 viewClients / viewProxies 完成，它们只做附加，不改动既有字段。

// clientView 是 FRP 客户端信息 + 本平台台账里的显示名称。
//
// 内嵌 frpapi.ClientInfo 是有意为之：encoding/json 会把匿名结构体的导出字段
// **提升**到外层，于是 JSON 形状与从前完全一致（前端既有字段一个都不用动），
// 也不存在「两份字段清单日后各自漂移」的问题。
//
// 注意 frpapi.ClientInfo 有一个 DisplayName() 方法，本结构又有一个 DisplayName
// 字段。Go 允许**直接字段遮蔽被提升的方法**（字段优先），因此合法；
// 代价是今后不能对 clientView 调用 DisplayName()，需要那个逻辑时
// 先把它还原成 frpapi.ClientInfo。
type clientView struct {
	frpapi.ClientInfo
	// DisplayName 是台账里管理员填写的可读名称（如「财务部-前台机」）。
	// 空串表示台账里这条记录没填名称，界面上退化成展示 clientID。
	DisplayName string `json:"display_name,omitempty"`
	// RobotType 是台账里这台设备的机器人类型（值域见 internal/pack.AllRobotTypes），
	// 界面据它把客户端列表按类型分组。空串表示未分类，也可能是压根没登记 ——
	// 两者靠 Registered 区分，不能只看这一列。
	//
	// 契约与 DisplayName 完全相同：FRP Dashboard 没有这个概念，只能由本平台
	// 按 clientID 关联台账得到。与名称一起在**同一次查询**里取回，否则列表上
	// 可能出现「新类型配旧名称」的短暂错位。
	RobotType string `json:"robot_type,omitempty"`
	// Registered 表示这个 clientID 是否在本平台台账里登记过。
	//
	// **它与 DisplayName 为空不是一回事**，这正是要单独下发的原因：
	// 台账里可能存在「登记过但没填名称」的记录，而 registered=false 说明
	// 这个客户端根本不是本平台签发的包 —— 多半是在别的机器上手工配的 frpc。
	// 界面据此给出完全不同的提示，而不是把两种情况都画成一个空单元格。
	Registered bool `json:"registered"`
}

// proxyView 是 FRP 代理信息 + 本平台台账里才有的几个附加字段。
// 内嵌提升的说明同 clientView。
type proxyView struct {
	frpapi.ProxyInfo
	// DisplayName 是所属客户端在台账里的名称；Registered 的含义同 clientView。
	DisplayName string `json:"display_name,omitempty"`
	Registered  bool   `json:"registered"`
	// LocalPort 是这条代理在**目标机器上**的本地端口。
	//
	// 它不可能来自 FRP：Dashboard 的 conf 只给 localIP 与 remotePort，不含
	// localPort（03-frp-api-contract.md §4-⑦）。唯一来源是本平台台账，按
	// (clientID, 代理短名) 关联 —— 契约与 DisplayName 完全相同。
	LocalPort int `json:"local_port"`
	// LocalPortKnown 表示台账里确实登记了这条代理的本地端口。
	//
	// **必须与 LocalPort 一起下发**，不能让前端用 LocalPort > 0 代替：
	// 「没登记」（手工配的 frpc、包不是本平台签发的）与「本地端口是 0」
	// 是两件事，界面文案也不同（「未登记」vs 数字）。取舍同 clientView.Registered。
	LocalPortKnown bool `json:"local_port_known"`
}

// healthView 是节点健康度的对外表示。
//
// Known 为 false 表示从未采集成功，界面必须显示「状态未知」
// 而不是把所有客户端标记为离线（01-prd §8.4 / KR5）。
//
// 两个时间字段用指针，避免「从未成功」时序列化出 0001-01-01。
type healthView struct {
	Known       bool       `json:"known"`
	Healthy     bool       `json:"healthy"`
	Stale       bool       `json:"stale"`
	LastSuccess *time.Time `json:"last_success,omitempty"`
	LastAttempt *time.Time `json:"last_attempt,omitempty"`
	FailStreak  int        `json:"fail_streak"`
	ErrorKind   string     `json:"error_kind,omitempty"`
	Message     string     `json:"message,omitempty"`
}

type overviewResponse struct {
	Health     healthView            `json:"health"`
	Server     *frpapi.ServerInfo    `json:"server"`
	Clients    collector.ClientStats `json:"clients"`
	ProxyCount int                   `json:"proxy_count"`
	FetchedAt  *time.Time            `json:"fetched_at,omitempty"`
}

type clientsResponse struct {
	Health    healthView            `json:"health"`
	Stats     collector.ClientStats `json:"stats"`
	Clients   []clientView          `json:"clients"`
	FetchedAt *time.Time            `json:"fetched_at,omitempty"`
}

type clientDetailResponse struct {
	Health healthView  `json:"health"`
	Client *clientView `json:"client"`
	// Proxies 用 proxyView 而不是 frpapi.ProxyInfo。
	//
	// 原先刻意用原始结构，理由是「它们全都属于同一个客户端，再给每条代理重复
	// 一遍同一个显示名称没有意义」。本地端口出现后这条理由不再成立：
	// **端口是逐条代理的属性**，而 FRP 不返回 localPort，只能从台账关联 ——
	// 原始结构根本给不出这个字段。proxyView 是 ProxyInfo 的超集（内嵌提升），
	// JSON 形状向后兼容，因此不必为详情页再定义第三个近似类型。
	Proxies []proxyView `json:"proxies"`
}

// clientLinkView 是某条代理「从外面怎么访问」的答案。
//
// 之所以要后端算而不是前端拼：访问地址的拼法取决于代理类型，而类型清单的
// 唯一权威在 internal/pack（AllProxyTypes / NeedsRemotePort）。前端再抄一份
// 类型清单，迟早出现「界面能选、后端不认」这类漂移（同类教训见 tcpmux）。
type clientLinkView struct {
	// ProxyName 是 FRP 的完整名（user.proxyname），ShortName 是管理员填的短名。
	// 界面展示用短名，复制出去的地址也是按短名标注的 —— 完整名带 user. 前缀，
	// 直接贴给现场会让人以为是地址的一部分。
	ProxyName string `json:"proxy_name"`
	ShortName string `json:"short_name"`
	ProxyType string `json:"proxy_type"`
	RemotePort int  `json:"remote_port"`
	Online     bool `json:"online"`
	// LocalPort 是这条代理在**目标机器上**的端口，只用来让现场认出「这是哪个服务」。
	//
	// 它既不在访问地址里，也不是从外面能连的东西 —— 加它纯粹因为只给
	// 「RCS02: 111.55.49.119:12312」时，现场分不清这一条到底对应机器上
	// 哪一个服务。来源与 proxyView 完全一致：FRP 不返回 localPort
	// （03-frp-api-contract §4-⑦），只能按 (clientID, 代理短名) 关联本平台台账。
	LocalPort int `json:"local_port"`
	// LocalPortKnown 表示台账里确实登记了这条代理的本地端口。
	//
	// **必须与 LocalPort 一起下发**，界面不许用 LocalPort > 0 代替：未登记要显示
	// 「未登记」而不是「0 号端口」，后者会被当成真有这条代理且端口是 0。
	// 判据统一走 lookupLocalPort，不在这里另写一套。
	LocalPortKnown bool `json:"local_port_known"`
	// AccessURL 是可直接访问的地址；空串表示这条代理给不出地址，原因见 Reason。
	//
	// 空串与「地址是空」必须分开表示，理由同 LocalPortKnown：
	// 给不出的原因各不相同（见 accessURL），画成空白会被读成「加载失败」。
	AccessURL string `json:"access_url"`
	Reason    string `json:"reason"`
}

type clientLinksResponse struct {
	ClientID string `json:"client_id"`
	// DisplayName 是台账里的可读名称（如 RCS02），用于「XXX 的访问地址为」这句文案。
	DisplayName string `json:"display_name"`
	// Endpoint 是接入点的对外地址（IP 或域名），不是 Dashboard 地址。
	//
	// 这两者不能混用是本项目的一条硬约束：frps 前面有 NAT 时对外端口与
	// bindPort 都不一样，用 Dashboard 地址推出来的链接一定是错的。
	Endpoint string `json:"endpoint"`
	// EndpointConfigured 为 false 时下面所有地址都给不出，界面要提示去设置页补全。
	EndpointConfigured bool `json:"endpoint_configured"`
	Links              []clientLinkView `json:"links"`
}

type proxiesResponse struct {
	Health    healthView  `json:"health"`
	Proxies   []proxyView `json:"proxies"`
	FetchedAt *time.Time  `json:"fetched_at,omitempty"`
}

// ---------- 处理器 ----------

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"time":   time.Now(),
	})
}

// handleNode 返回节点配置（不返回凭据）与采集健康度。
func (s *Server) handleNode(w http.ResponseWriter, _ *http.Request) {
	st := s.coll.State()
	eff := s.nodes.Current()

	writeJSON(w, http.StatusOK, map[string]any{
		"node":   eff.Redacted(),
		"source": eff.Source,
		"health": viewHealth(st.Health),
		"collector": map[string]any{
			"interval_seconds": int(s.coll.Interval() / time.Second),
		},
	})
}

func (s *Server) handleOverview(w http.ResponseWriter, _ *http.Request) {
	st := s.coll.State()

	resp := overviewResponse{
		Health:  viewHealth(st.Health),
		Clients: collector.ClientStats{},
	}
	if st.Snapshot != nil {
		resp.Server = st.Snapshot.ServerInfo
		resp.Clients = st.Snapshot.Stats()
		resp.ProxyCount = len(st.Snapshot.Proxies)
		resp.FetchedAt = timePtr(st.Snapshot.CollectedAt)
	}

	// 从未采集成功时返回 503，避免前端把空数据当成「零客户端」。
	writeJSON(w, statusFor(st), resp)
}

func (s *Server) handleClients(w http.ResponseWriter, r *http.Request) {
	st := s.coll.State()

	resp := clientsResponse{
		Health:  viewHealth(st.Health),
		Stats:   collector.ClientStats{},
		Clients: []clientView{},
	}
	if st.Snapshot != nil {
		resp.Stats = st.Snapshot.Stats()
		resp.Clients = s.viewClients(r.Context(), st.Snapshot.Clients)
		resp.FetchedAt = timePtr(st.Snapshot.CollectedAt)
	}

	writeJSON(w, statusFor(st), resp)
}

func (s *Server) handleClientDetail(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	st := s.coll.State()

	if st.Snapshot == nil {
		writeError(w, http.StatusServiceUnavailable, errKindUnavailable,
			"尚未取得节点数据，无法展示客户端详情")
		return
	}

	var found *frpapi.ClientInfo
	for i := range st.Snapshot.Clients {
		if st.Snapshot.Clients[i].Key == key || st.Snapshot.Clients[i].ClientID == key {
			found = &st.Snapshot.Clients[i]
			break
		}
	}
	if found == nil {
		writeError(w, http.StatusNotFound, errKindNotFound, "未找到该客户端")
		return
	}

	// 按 clientID 关联代理：这是 01-prd F06 要求的关联方式。
	// 选出来之后仍要走 viewProxies：本地端口只存在于台账里
	// （FRP 的 conf 不含 localPort），直接下发原始结构就拿不到它 ——
	// 那正是「客户端详情里有远端端口、没有本地端口」的原因。
	mine := make([]frpapi.ProxyInfo, 0, 8)
	for _, p := range st.Snapshot.Proxies {
		if p.ClientID == found.ClientID {
			mine = append(mine, p)
		}
	}
	proxies := s.viewProxies(r.Context(), mine)

	view := s.viewClients(r.Context(), []frpapi.ClientInfo{*found})[0]

	writeJSON(w, http.StatusOK, clientDetailResponse{
		Health:  viewHealth(st.Health),
		Client:  &view,
		Proxies: proxies,
	})
}

// handleClientLinks 返回某台设备各条代理「从外面怎么访问」的答案。
//
// 需求现场：运维要告诉别人「RCS02 的访问地址是 xxx:12312」，此前只能自己
// 去代理列表里找端口、再手敲接入点 IP 拼起来 —— 手拼的地址错了没人会发现，
// 要等对方说连不上。
func (s *Server) handleClientLinks(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	st := s.coll.State()

	if st.Snapshot == nil {
		writeError(w, http.StatusServiceUnavailable, errKindUnavailable,
			"尚未取得节点数据，无法生成穿透链接")
		return
	}

	var found *frpapi.ClientInfo
	for i := range st.Snapshot.Clients {
		if st.Snapshot.Clients[i].Key == key || st.Snapshot.Clients[i].ClientID == key {
			found = &st.Snapshot.Clients[i]
			break
		}
	}
	if found == nil {
		writeError(w, http.StatusNotFound, errKindNotFound, "未找到该客户端")
		return
	}

	// 接入点是 frpc 真正要连的**对外**地址，与采集用的 Dashboard 地址是两件事
	// （frps 前面有 NAT 时连端口都不同）。读不出来时仍然返回 200：
	// 弹窗要能说清「缺什么」，而不是整个打不开。
	ep, err := endpoint.Load(r.Context(), s.st)
	if err != nil {
		s.log.Warn("读取客户端接入点失败", "err", err)
	}
	host := linkHost(ep.Addr)

	mine := make([]frpapi.ProxyInfo, 0, 8)
	for _, p := range st.Snapshot.Proxies {
		if p.ClientID == found.ClientID {
			mine = append(mine, p)
		}
	}
	// 排序只为稳定：同一台设备的多条代理若每次打开顺序都变，
	// 运维会以为列表在刷新、不敢复制。
	sort.Slice(mine, func(i, j int) bool { return shortProxyName(mine[i]) < shortProxyName(mine[j]) })

	// 本地端口只能从台账关联（FRP 不返回），与代理列表、客户端详情共用同一份索引
	// 和同一个 lookupLocalPort —— 三处判据必须一致，否则会出现「同一条代理
	// 在代理页有本地端口、在链接弹窗里却没有」。
	ports := s.registryPortIndex(r.Context())

	links := make([]clientLinkView, 0, len(mine))
	for _, p := range mine {
		u, reason := accessURL(host, p)
		short := shortProxyName(p)
		local, localKnown := lookupLocalPort(ports, p.ClientID, short)
		links = append(links, clientLinkView{
			ProxyName:      p.Name,
			ShortName:      short,
			ProxyType:      p.Type(),
			RemotePort:     p.RemotePort(),
			Online:         p.IsOnline(),
			LocalPort:      local,
			LocalPortKnown: localKnown,
			AccessURL:      u,
			Reason:         reason,
		})
	}

	view := s.viewClients(r.Context(), []frpapi.ClientInfo{*found})[0]

	writeJSON(w, http.StatusOK, clientLinksResponse{
		ClientID:           found.ClientID,
		DisplayName:        view.DisplayName,
		Endpoint:           ep.Addr,
		EndpointConfigured: ep.Configured(),
		Links:              links,
	})
}

// accessURL 按代理类型拼出可从外部访问的地址。
//
// 只有 tcp / udp 能给出「IP:端口」这种直接可访问的地址，判据用
// pack.NeedsRemotePort —— 与代理表单里「哪些类型要填远端端口」是同一条规则，
// 两边各判一次迟早不一致。
//
// 其余类型一律**不给地址并写明原因**，绝不猜：
//   - http / https 按域名访问，而 FRP 的代理 conf 里根本没有 customDomains /
//     subdomain 字段（03-frp-api-contract §4），猜出来的域名几乎必错，
//     一个错的域名比「不给」更有害 —— 对方会照着它去配 DNS。
//   - stcp / xtcp 要访问者一端另部署 frpc，压根不是「一个地址就能连」的东西。
func accessURL(host string, p frpapi.ProxyInfo) (string, string) {
	if host == "" {
		return "", "未配置客户端接入点，请先到「设置 → 客户端接入点」补全对外地址"
	}
	switch pack.ProxyType(p.Type()) {
	case pack.ProxyTCP, pack.ProxyUDP:
		if p.RemotePort() <= 0 {
			return "", "代理已离线，FRP 不返回离线代理的端口"
		}
		return fmt.Sprintf("%s:%d", host, p.RemotePort()), ""
	case pack.ProxyHTTP, pack.ProxyHTTPS:
		return "", "这类代理按域名访问，而 FRP 的接口不返回域名，请在客户端包内的 frpc.toml 里查看"
	case pack.ProxySTCP, pack.ProxyXTCP:
		return "", "访问这类代理需要在访问者一端另外部署 frpc，没有可直接访问的地址"
	}
	if p.Conf == nil {
		return "", "代理已离线，FRP 不返回离线代理的配置"
	}
	return "", "未知代理类型：" + p.Type()
}

// linkHost 从接入点地址里取出用于拼链接的主机部分。
//
// 接入点地址是「frpc 要连的主机」，管理员完全可能填成 host:port。
// 拼访问链接时冒号后面必须换成本条代理的远端端口，留着原端口就成了
// 两个端口连写（1.2.3.4:7000:12312）。
//
// 只在「恰好一个冒号」时才切：IPv6 字面量本身含多个冒号，不能被误伤。
func linkHost(addr string) string {
	a := strings.TrimSpace(addr)
	if a == "" {
		return ""
	}
	if strings.Count(a, ":") == 1 {
		if h, _, ok := strings.Cut(a, ":"); ok && h != "" {
			return h
		}
	}
	return a
}

func (s *Server) handleProxies(w http.ResponseWriter, r *http.Request) {
	st := s.coll.State()

	wantType := r.URL.Query().Get("type")
	if wantType != "" && !frpapi.IsValidProxyType(wantType) {
		writeError(w, http.StatusBadRequest, errKindBadRequest, "不支持的代理类型: "+wantType)
		return
	}
	wantClient := r.URL.Query().Get("client_id")

	resp := proxiesResponse{
		Health:  viewHealth(st.Health),
		Proxies: []proxyView{},
	}
	if st.Snapshot != nil {
		resp.FetchedAt = timePtr(st.Snapshot.CollectedAt)

		// 先按筛选条件挑出目标代理，再统一附加显示名称：
		// 台账映射只取一次，避免逐条查询。
		picked := make([]frpapi.ProxyInfo, 0, len(st.Snapshot.Proxies))
		for _, p := range st.Snapshot.Proxies {
			if wantType != "" && p.Type() != wantType {
				continue
			}
			if wantClient != "" && p.ClientID != wantClient {
				continue
			}
			picked = append(picked, p)
		}
		resp.Proxies = s.viewProxies(r.Context(), picked)
	}

	writeJSON(w, statusFor(st), resp)
}

// handleRefresh 触发一次立即采集，供界面的「手动刷新」使用。
func (s *Server) handleRefresh(w http.ResponseWriter, _ *http.Request) {
	refreshed := s.coll.Refresh(4 * time.Second)
	st := s.coll.State()

	writeJSON(w, http.StatusOK, map[string]any{
		"refreshed": refreshed,
		"health":    viewHealth(st.Health),
		"known":     st.Snapshot != nil,
	})
}

// viewHealth 把采集健康度转换为对外表示。
func viewHealth(h collector.Health) healthView {
	v := healthView{
		Known:      !h.LastSuccess.IsZero(),
		Healthy:    h.Healthy,
		Stale:      h.Stale,
		FailStreak: h.FailStreak,
		ErrorKind:  string(h.ErrorKind),
		Message:    h.LastError,
	}
	v.LastSuccess = timePtr(h.LastSuccess)
	v.LastAttempt = timePtr(h.LastAttempt)
	return v
}

// ---------- 台账关联（01-prd F19）----------
//
// 显示名称是本平台特有的信息：管理员生成部署包时给这台机器起的名字，
// 存在本地台账里。FRP Dashboard 只返回 clientID 这种设备标识，
// 所以「让界面显示人看得懂的名字」这件事只能在这里做一次关联。

// viewClients 给 FRP 的客户端列表附上台账的显示名称与机器人类型。
//
// 台账读不出来时不阻断、不报错：客户端列表是管理员排查问题的主视图，
// 不能因为一个附加字段让整页失败。此时 registered 全为 false、类型为空串，
// 界面会显示「未登记」并归入「未分类」——措辞上要接受这个降级，因为它与
// 「真的没登记」在界面上不可区分。取舍是刻意的：宁可偶发显示不准，
// 也不要整页打不开。
func (s *Server) viewClients(ctx context.Context, in []frpapi.ClientInfo) []clientView {
	out := make([]clientView, 0, len(in))
	if len(in) == 0 {
		return out
	}
	metas := s.registryMetas(ctx)
	for _, c := range in {
		v := clientView{ClientInfo: c}
		// clientID 为空时无从关联（也无法被登记），保持 registered=false。
		if c.ClientID != "" {
			if m, ok := metas[c.ClientID]; ok {
				v.DisplayName = m.DisplayName
				v.RobotType = m.RobotType
				v.Registered = true
			}
		}
		out = append(out, v)
	}
	return out
}

// viewProxies 给 FRP 的代理列表附上台账里才有的字段：
// 所属客户端的显示名称，以及**这条代理自己的本地端口**。
//
// 本地端口必须逐条关联，不能只按 clientID 取：同一个客户端下可以有多条代理，
// 各自的本地端口不同（现场最常见的就是 RDP 3389 + Web 8080 两条）。
// 关联键是 (clientID, 代理短名) —— 台账里存的是短名，FRP 给的是
// "user.proxyname" 完整名，所以必须先经 shortProxyName 拆开。
func (s *Server) viewProxies(ctx context.Context, in []frpapi.ProxyInfo) []proxyView {
	out := make([]proxyView, 0, len(in))
	if len(in) == 0 {
		return out
	}
	names := s.registryNames(ctx)
	ports := s.registryPortIndex(ctx)
	for _, p := range in {
		v := proxyView{ProxyInfo: p}
		if p.ClientID != "" {
			if name, ok := names[p.ClientID]; ok {
				v.DisplayName = name
				v.Registered = true
			}
			// 查不到就保持 known=false，由界面显示「未登记」——
			// 不要退化成「本地端口 = 0」，那是非法端口号。
			v.LocalPort, v.LocalPortKnown = lookupLocalPort(ports, p.ClientID, shortProxyName(p))
		}
		out = append(out, v)
	}
	return out
}
