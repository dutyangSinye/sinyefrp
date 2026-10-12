package web

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"sinyefrp/internal/collector"
	"sinyefrp/internal/endpoint"
	"sinyefrp/internal/frpapi"
	"sinyefrp/internal/pack"
	"sinyefrp/internal/store"
)

// 本文件实现客户端台账与端口占用（01-prd F19）。
//
// 要解决的问题：远端端口在同一个 frps 上是**全局资源**。两个客户端占同一个
// 端口时，后注册的那个代理会被 FRP 拒绝，而失败发生在目标机器上 ——
// 管理员在管理台里看不到任何征兆，只能等现场反馈。
//
// FRP Dashboard 只在代理**已在线**时才能看到它占的端口，于是
// 「包已生成、还没装到目标机器」这段时间完全无据可查。这段时间恰恰是最容易
// 重复分配端口的窗口（管理员连续给几台机器出包）。所以必须自己记账。
//
// 两个口径刻意分开呈现，不合并：
//   - registry：本平台签发过什么，包含尚未安装的；
//   - live：FRP 此刻真实在线的代理，包含别人手工配的。
// 二者都可能单独存在。合并成一个列表会同时掩盖「签了但没装」与
// 「装了但没在台账里」两类问题 —— 而这两类问题的处理方式完全不同。

// ---------- 响应模型 ----------

// registryPortView 是台账里的一条端口占用明细。
type registryPortView struct {
	ID         int64  `json:"id"`
	ProxyName  string `json:"proxy_name"`
	ProxyType  string `json:"proxy_type"`
	RemotePort int    `json:"remote_port"`
	LocalPort  int    `json:"local_port"`
}

// portsToView 把 store 层的端口记录转成对外表示，并保证空集是 []。
//
// 单独抽出来是为了让「切片永不为 nil」这条约定只有一处实现：
// 前端读 ports.length，收到 null 会直接抛异常崩掉整页。
func portsToView(ports []store.ClientPort) []registryPortView {
	out := make([]registryPortView, 0, len(ports))
	for _, p := range ports {
		out = append(out, registryPortView{
			ID:         p.ID,
			ProxyName:  p.ProxyName,
			ProxyType:  p.ProxyType,
			RemotePort: p.RemotePort,
			LocalPort:  p.LocalPort,
		})
	}
	return out
}

// newRegistryClientView 是台账记录的**唯一**视图构造函数。
//
// 原先列表、补录、编辑三处各自用字面量拼一遍 registryClientView，注释里还写着
// 「与列表接口同口径」—— 而新增 platform 字段时，编辑那一处就被漏掉了：
// 更新确实生效了（审计、数据库都对），只是响应里少了一个字段。于是
// 「停用一次台账之后平台还在不在」这件事在界面上查不出来，而它要几周后
// 给那台设备改包时才被用到。
//
// 这类缺陷只能靠结构消除，靠记性没用，它有三个特征：
//   - 零报错零警告，编译器完全沉默（结构体少个字段是合法的）；
//   - 症状是「响应里少一个数」，要等有人用到它才浮现；
//   - 回归也照样绿 —— 断言往往只覆盖到先写的那一处。
// 所以三个调用点一律走这里，将来再加字段只改这一处。
func newRegistryClientView(
	c store.ClientRegistry,
	ports []registryPortView,
	seen, online map[string]bool,
) registryClientView {
	if ports == nil {
		// 契约：切片一律是 []，不是 null。前端读 ports.length，收到 null 直接崩页。
		ports = []registryPortView{}
	}
	return registryClientView{
		ID:          c.ID,
		ClientID:    c.ClientID,
		DisplayName: c.DisplayName,
		RobotType:   c.RobotType,
		Platform:    c.Platform,
		ServerAddr:  c.ServerAddr,
		ServerPort:  c.ServerPort,
		Status:      c.Status,
		Note:        c.Note,
		// GenerateCount 是重新生成的累计次数；补录得来的记录这里是 0。
		GenerateCount:    c.GenerateCount,
		SeenInFRP:        seen[c.ClientID],
		Online:           online[c.ClientID],
		FirstGeneratedAt: timePtr(c.FirstGeneratedAt),
		LastGeneratedAt:  timePtr(c.LastGeneratedAt),
		Ports:            ports,
	}
}

// registryClientView 是台账记录的对外表示。
type registryClientView struct {
	ID          int64  `json:"id"`
	ClientID    string `json:"client_id"`
	DisplayName string `json:"display_name"`
	// RobotType 取 pack 的机器人类型值（spray/rebar/transport/welding/other），
	// 空串表示未分类。这是纯业务分类，不参与任何判定。
	RobotType  string `json:"robot_type"`
	ServerAddr string `json:"server_addr"`
	ServerPort int    `json:"server_port"`
	// Platform 是这台设备的部署包**目标运行平台**（windows-amd64 / linux-amd64 /
	// linux-arm64），决定下一次改包产出什么样的内容。
	//
	// 空串表示「未记录」——v4 迁移之前签发的包没有这一信息，界面按 Windows
	// 展示但必须让管理员确认，不能替他默认（给 Linux 盒子生成 Windows 包
	// 时安装脚本不会报错，要等上线才发现）。
	Platform string `json:"platform"`
	Status   string `json:"status"`
	Note     string `json:"note"`
	// GenerateCount 是重新生成的累计次数，首次生成为 1。
	GenerateCount int `json:"generate_count"`
	// SeenInFRP 表示该 clientID 是否出现在 FRP 当前快照的客户端列表里。
	//
	// **只在 health.known 为 true 时才有意义。** 从未采集成功时它是 false，
	// 但那是「不知道」而不是「不在线」，界面必须显示「—」而不是「未见」，
	// 否则会把「Dashboard 连不上」误报成「客户端全都没上线」（同 §8.4 的语义）。
	SeenInFRP bool `json:"seen_in_frp"`
	// Online 表示该 clientID **此刻**在 FRP 里是否在线。
	//
	// 与 SeenInFRP 是两件事，不能互相替代：
	//   - SeenInFRP 连**已断开但尚未被 FRP 清理**的客户端也算，回答「装过没有」；
	//   - Online 只看 /api/clients 的 online 字段，回答「现在通不通」。
	//
	// 只下发 SeenInFRP 时界面只能表达「装过 / 没装过」，而「连过但现在断了」
	// 这种最常见的情况会被显示成「没装」，把排查方向带偏。
	// 与 SeenInFRP 同样只在 health.known 为 true 时才有意义。
	Online           bool               `json:"online"`
	FirstGeneratedAt *time.Time         `json:"first_generated_at"`
	LastGeneratedAt  *time.Time         `json:"last_generated_at"`
	Ports            []registryPortView `json:"ports"`
}

// portOwnerView 是某个远端端口的占用者。
type portOwnerView struct {
	// ID 是台账端口占用记录的编号，用于「只释放这一条」；
	// live 口径的行没有台账记录，恒为 0。
	ID int64 `json:"id"`
	// Port 是**远端端口**（frps 上的那个）。本列表就是按它组织的：
	// 远端端口在同一个 frps 上是全局资源，冲突判定只看它。
	Port       int    `json:"port"`
	ClientID   string `json:"client_id"`
	ClientName string `json:"client_name"`
	ProxyName  string `json:"proxy_name"`
	ProxyType  string `json:"proxy_type"`
	// LocalPort / LocalPortKnown 是这条代理在目标机器上的本地端口，
	// 语义与来源同 proxyView：只能由台账关联得到（FRP 不返回 localPort）。
	// 它**不参与冲突判定** —— 本地端口属于目标机器自己的地址空间，
	// 不同客户端重复是正常的，界面上也要保留这个说法。
	LocalPort      int  `json:"local_port"`
	LocalPortKnown bool `json:"local_port_known"`
	// Status 为 registry 口径时取 active/retired；live 口径时取 online/offline。
	Status string `json:"status"`
	// Source 取 store.PortSourceRegistry / PortSourceLive。
	Source string `json:"source"`
}

// registryClientsResponse 是台账列表的响应。
type registryClientsResponse struct {
	Clients []registryClientView `json:"clients"`
	Health  healthView           `json:"health"`
}

// registryPortsResponse 是端口占用总览的响应。
type registryPortsResponse struct {
	Registry []portOwnerView `json:"registry"`
	Live     []portOwnerView `json:"live"`
	Health   healthView      `json:"health"`
}

// ---------- 处理器 ----------

// handleRegistryClients 返回客户端台账（01-prd F19）。
func (s *Server) handleRegistryClients(w http.ResponseWriter, r *http.Request) {
	clients, err := s.st.ListClients(r.Context(), 500)
	if err != nil {
		s.log.Error("查询客户端台账失败", "err", err)
		writeError(w, http.StatusInternalServerError, errKindInternal, "查询客户端台账失败")
		return
	}
	ports, err := s.st.ListClientPorts(r.Context())
	if err != nil {
		s.log.Error("查询端口占用失败", "err", err)
		writeError(w, http.StatusInternalServerError, errKindInternal, "查询端口占用失败")
		return
	}

	byClient := map[string][]registryPortView{}
	for _, p := range ports {
		byClient[p.ClientID] = append(byClient[p.ClientID], registryPortView{
			ID: p.ID, ProxyName: p.ProxyName, ProxyType: p.ProxyType,
			RemotePort: p.RemotePort, LocalPort: p.LocalPort,
		})
	}

	// 台账页要同时回答两个问题：「这个包装上没有」（seen）与「现在通不通」（online）。
	// 两者都只在采集成功时有意义，因此同时下发健康度，由界面决定怎么显示。
	seen, online := s.clientPresence(r.Context())

	state := s.coll.State()
	out := make([]registryClientView, 0, len(clients))
	for _, c := range clients {
		ps := byClient[c.ClientID]
		if ps == nil {
			ps = []registryPortView{} // 契约：切片一律是 []，不是 null
		}
		out = append(out, newRegistryClientView(c, ps, seen, online))
	}

	writeJSON(w, http.StatusOK, registryClientsResponse{
		Clients: out,
		Health:  viewHealth(state.Health),
	})
}

// handleRegistryPorts 返回端口占用总览：台账口径 + FRP 实时口径。
func (s *Server) handleRegistryPorts(w http.ResponseWriter, r *http.Request) {
	state := s.coll.State()

	regOwners, err := s.st.ActivePortOwners(r.Context())
	if err != nil {
		s.log.Error("查询端口占用者失败", "err", err)
		writeError(w, http.StatusInternalServerError, errKindInternal, "查询端口占用失败")
		return
	}

	// 本地端口只能从台账关联得到（FRP 不返回 localPort），两个口径共用同一份索引。
	// 只查一次：逐条查会让端口总览这种几十行的列表变成几十次数据库往返。
	ports := s.registryPortIndex(r.Context())

	reg := make([]portOwnerView, 0, len(regOwners))
	for _, o := range regOwners {
		local, known := lookupLocalPort(ports, o.ClientID, o.ProxyName)
		reg = append(reg, portOwnerView{
			ID:   o.ID,
			Port: o.Port, ClientID: o.ClientID, ClientName: o.ClientName,
			ProxyName: o.ProxyName, ProxyType: o.ProxyType,
			LocalPort: local, LocalPortKnown: known,
			Status: o.Status, Source: o.Source,
		})
	}

	names := s.registryNames(r.Context())
	live := livePortOwners(state.Snapshot, names)
	liveViews := make([]portOwnerView, 0, len(live))
	for _, o := range live {
		local, known := lookupLocalPort(ports, o.ClientID, o.ProxyName)
		liveViews = append(liveViews, portOwnerView{
			Port: o.Port, ClientID: o.ClientID, ClientName: o.ClientName,
			ProxyName: o.ProxyName, ProxyType: o.ProxyType,
			LocalPort: local, LocalPortKnown: known,
			Status: o.Status, Source: o.Source,
		})
	}

	writeJSON(w, http.StatusOK, registryPortsResponse{
		Registry: reg, Live: liveViews, Health: viewHealth(state.Health),
	})
}

// registryUpdateReq 是修改台账记录的请求体。
//
// 四个字段都必填（DisallowUnknownFields 之外，Go 的解码不会因为缺字段报错），
// 因此前端一次提交完整四件套，不做「只传变化字段」的局部更新。
// robot_type 传空串是合法的，表示清空为「未分类」。
type registryUpdateReq struct {
	DisplayName string `json:"display_name"`
	RobotType   string `json:"robot_type"`
	// Platform 是这台设备的部署包目标平台。
	//
	// 空串表示「未记录」—— 它不是一个可选平台，而是历史数据状态（v4 迁移之前
	// 签发的包没有这一列）。这里**刻意允许**空串，好让管理员能把一条记录退回
	// 初始状态；但前端必须始终带上当前值，否则「改个备注」会把平台悄悄清掉。
	Platform string `json:"platform"`
	Note     string `json:"note"`
	Status   string `json:"status"`
}

// registryCreateReq 是手工新增台账记录的请求体。
//
// 存在的理由（来自现场事故）：设备已经部署并在运行，但对应的台账记录丢了。
// 旧版本唯一能创建台账的入口是「生成客户端包」，而管理员并不想再出一个包 ——
// 于是这台设备在台账里永久缺席：界面显示不出名称、端口也不受冲突保护。
// 这个接口就是补这条路。
//
// Ports 允许为空：管理员可能只知道有这台机器、暂时不清楚代理端口。
// 与签发路径一致，只有需要远端端口的类型才计入冲突判定。
type registryCreateReq struct {
	ClientID    string `json:"client_id"`
	DisplayName string `json:"display_name"`
	RobotType   string `json:"robot_type"`
	// Platform 见 registryUpdateReq 的同名字段：空串是「未记录」，可以显式传，
	// 但前端必须始终带上当前值，避免顺手改个别字段时把它清空。
	Platform   string                    `json:"platform"`
	Note       string                    `json:"note"`
	ServerAddr string                    `json:"server_addr"`
	ServerPort int                       `json:"server_port"`
	Ports      []registryCreatePortEntry `json:"ports"`
}

type registryCreatePortEntry struct {
	ProxyName  string `json:"proxy_name"`
	ProxyType  string `json:"proxy_type"`
	RemotePort int    `json:"remote_port"`
	LocalPort  int    `json:"local_port"`
}

// handleRegistryCreate 手工新增一条台账记录（POST /api/registry/clients）。
//
// 与签发路径的两点差异，都是有意的：
//   - generate_count 记 0（不是 1）：这条记录没有对应的签发历史，
//     界面据此可以区分「发出去的」与「补录的」；
//   - 端口冲突在这里**硬拦**（409），而不是像 preview 那样只告警。
//     因为登记是显式动作，管理员此刻就在填端口，直接告诉他换一个成本最低；
//     等他填完再报错反而要重来。
func (s *Server) handleRegistryCreate(w http.ResponseWriter, r *http.Request) {
	var req registryCreateReq
	if !s.decodeJSON(w, r, &req) {
		return
	}

	clientID := strings.TrimSpace(req.ClientID)
	name := strings.TrimSpace(req.DisplayName)
	note := strings.TrimSpace(req.Note)
	robotType := strings.TrimSpace(req.RobotType)
	platform := strings.TrimSpace(req.Platform)
	serverAddr := strings.TrimSpace(req.ServerAddr)

	if clientID == "" {
		writeError(w, http.StatusBadRequest, errKindBadRequest, "clientID 不能为空")
		return
	}
	// clientID 是 FRP 侧的主键，格式由 frp 生成（sf-xxxx 形式）。
	// 这里只做长度与字符白名单校验：补录时管理员是从现场 FRP 界面抄下来的，
	// 抄错一个字符就会与真实客户端对不上，还不如早点拦住。
	if len([]rune(clientID)) > 64 {
		writeError(w, http.StatusBadRequest, errKindBadRequest, "clientID 不能超过 64 字")
		return
	}
	for _, ch := range clientID {
		okChar := (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') ||
			(ch >= '0' && ch <= '9') || ch == '-' || ch == '_'
		if !okChar {
			writeError(w, http.StatusBadRequest, errKindBadRequest,
				"clientID 只能包含字母、数字、连字符与下划线，请核对是否抄错")
			return
		}
	}
	if len([]rune(name)) > 100 {
		writeError(w, http.StatusBadRequest, errKindBadRequest, "显示名称不能超过 100 字")
		return
	}
	if len([]rune(note)) > 500 {
		writeError(w, http.StatusBadRequest, errKindBadRequest, "备注不能超过 500 字")
		return
	}
	if req.ServerPort < 0 || req.ServerPort > 65535 {
		writeError(w, http.StatusBadRequest, errKindBadRequest, "接入点端口需在 0-65535 之间")
		return
	}
	// 值域以 pack 的枚举为唯一权威，不在这里另抄一份清单。
	if !pack.ValidRobotType(robotType) {
		writeError(w, http.StatusBadRequest, errKindBadRequest,
			"机器人类型 "+strconv.Quote(robotType)+" 不受支持，仅支持："+pack.RobotTypeChoices())
		return
	}
	// 补录的设备往往已经在现场跑着，平台是管理员**看得见的事实**（那台盒子是什么系统），
	// 不是要猜的东西 —— 所以同样按 pack 的枚举校验，不在接口里另抄一份清单。
	if platform != "" && !pack.Platform(platform).Valid() {
		writeError(w, http.StatusBadRequest, errKindBadRequest,
			"目标平台 "+strconv.Quote(platform)+" 不受支持，仅支持："+pack.PlatformChoices())
		return
	}

	ports := make([]store.ClientPort, 0, len(req.Ports))
	wanted := map[int]string{}
	for _, p := range req.Ports {
		proxyName := strings.TrimSpace(p.ProxyName)
		if proxyName == "" {
			writeError(w, http.StatusBadRequest, errKindBadRequest, "端口记录缺少代理名称")
			return
		}
		proxyType := strings.TrimSpace(p.ProxyType)
		if proxyType == "" {
			proxyType = string(pack.ProxyTCP)
		}
		// 归一成小写：pack 的类型枚举是小写（"tcp"/"udp"...）。
		// 不归一化的后果很隐蔽 —— 传 "TCP" 时 pt.Valid() 为假，
		// 于是这条代理既不进冲突判定集合、也不报错，端口被静默放行；
		// 实测过：两条记录都登记 12311 却都创建成功（见 .workbuddy/test/api-test.txt）。
		// 大小写不该由调用方承担，这里统一收口。
		pt := pack.ProxyType(strings.ToLower(proxyType))
		proxyType = string(pt)
		if p.RemotePort < 0 || p.RemotePort > 65535 || p.LocalPort < 1 || p.LocalPort > 65535 {
			writeError(w, http.StatusBadRequest, errKindBadRequest,
				"代理「"+proxyName+"」的端口需在 1-65535 之间")
			return
		}
		// 只有需要远端端口的类型才参与冲突判定；其余类型（HTTP/HTTPS/STCP/XTCP/TCPMUX）
		// 不占用 frps 上的全局端口。
		//
		// 类型不认识时**必须报错而不是跳过**：跳过等于放行一个可能冲突的端口。
		if !pt.Valid() {
			writeError(w, http.StatusBadRequest, errKindBadRequest,
				"代理「"+proxyName+"」的类型 "+strconv.Quote(p.ProxyType)+" 不受支持，仅支持："+
					pack.ProxyTypeChoices())
			return
		}
		if pt.NeedsRemotePort() && p.RemotePort > 0 {
			wanted[p.RemotePort] = proxyName
		}
		ports = append(ports, store.ClientPort{
			ProxyName:  proxyName,
			ProxyType:  proxyType,
			RemotePort: p.RemotePort,
			LocalPort:  p.LocalPort,
		})
	}

	// ownerClientID 传空：新记录还不存在，任何既有占用都算冲突。
	// 传 clientID 也不对 —— 那等于假设「同 ID 的记录已存在」，与后面的重复检查语义冲突。
	if conflicts := s.conflictsFor(r.Context(), wanted, ""); len(conflicts) > 0 {
		msgs := conflictMessages(conflicts)
		summary := "登记失败：填写的端口中有 " + strconv.Itoa(len(msgs)) +
			" 个已被占用。请改用其他端口，或先释放占用方。"
		writeErrorDetails(w, http.StatusConflict, errKindConflict, summary, msgs)
		return
	}

	created, err := s.st.CreateClient(r.Context(), store.ClientRegistry{
		ClientID:    clientID,
		DisplayName: name,
		RobotType:   robotType,
		Platform:    platform,
		ServerAddr:  serverAddr,
		ServerPort:  req.ServerPort,
		Status:      store.ClientStatusActive,
		Note:        note,
	}, ports)
	if err != nil {
		if errors.Is(err, store.ErrClientExists) {
			writeError(w, http.StatusConflict, errKindConflict,
				"clientID "+strconv.Quote(clientID)+" 已在台账中。若这是同一台设备，请直接编辑既有记录。")
			return
		}
		s.writeRegistryErr(w, err, "新增台账记录失败")
		return
	}

	// 补录必须在审计里写清楚：这类记录没有签发历史，
	// 事后看到一条 generate_count=0 的记录时，要能在这里找到它是谁什么时候加的。
	s.audit(r, "registry.create", created.ClientID, "success",
		"手工补录台账记录："+created.DisplayName+
			"（机器人类型 "+describeRobotType(created.RobotType)+
			"，登记端口 "+strconv.Itoa(len(ports))+" 条）")

	// 补录的典型场景恰恰是「设备已经在跑、台账里没有」，所以这条记录大概率
	// 在 FRP 里可见、甚至此刻在线。回显时把这两个状态一起带上，界面立刻能看到，
	// 不必等下一次刷新列表。
	cSeen, cOnline := s.clientPresence(r.Context())
	view := newRegistryClientView(*created, portsToView(ports), cSeen, cOnline)
	writeJSON(w, http.StatusOK, map[string]any{"client": view, "message": "台账记录已补录"})
}

// handleRegistryUpdate 修改台账记录的显示名、备注与状态（POST /api/registry/clients/{id}）。
func (s *Server) handleRegistryUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r, "id")
	if !ok {
		return
	}
	var req registryUpdateReq
	if !s.decodeJSON(w, r, &req) {
		return
	}

	name := strings.TrimSpace(req.DisplayName)
	note := strings.TrimSpace(req.Note)
	status := strings.TrimSpace(req.Status)
	robotType := strings.TrimSpace(req.RobotType)
	platform := strings.TrimSpace(req.Platform)

	if status != store.ClientStatusActive && status != store.ClientStatusRetired {
		writeError(w, http.StatusBadRequest, errKindBadRequest,
			"状态只能是 active（启用）或 retired（停用）")
		return
	}
	// 值域以 pack 的枚举为唯一权威（01-prd F19 扩展）：这里若自己再写一份清单，
	// 就会在新增机器人类型时出现「打包放行、台账编辑拒绝」这种自相矛盾的状态。
	if !pack.ValidRobotType(robotType) {
		writeError(w, http.StatusBadRequest, errKindBadRequest,
			"机器人类型 "+strconv.Quote(robotType)+" 不受支持，仅支持："+pack.RobotTypeChoices())
		return
	}
	if len([]rune(name)) > 100 {
		writeError(w, http.StatusBadRequest, errKindBadRequest, "显示名称不能超过 100 字")
		return
	}
	if len([]rune(note)) > 500 {
		writeError(w, http.StatusBadRequest, errKindBadRequest, "备注不能超过 500 字")
		return
	}
	// 平台的值域同样以 pack 的枚举为唯一权威（理由同上面的机器人类型）。
	// 空串单独放行 —— 它是「未记录」这种历史状态，用于把记录退回 v4 迁移前的样子，
	// 不是一个可选的第四种平台。
	if platform != "" && !pack.Platform(platform).Valid() {
		writeError(w, http.StatusBadRequest, errKindBadRequest,
			"目标平台 "+strconv.Quote(platform)+" 不受支持，仅支持："+pack.PlatformChoices())
		return
	}

	before, err := s.st.ClientByID(r.Context(), id)
	if err != nil {
		s.writeRegistryErr(w, err, "台账记录不存在")
		return
	}

	// 「停用 → 启用」是一次重新主张端口所有权的动作，必须先确认端口还空着：
	// 停用期间它完全可能已经被分配给了别人（见 checkActivationConflicts）。
	// 而「本来就是启用」的重复保存不该被自己的占用绊倒，所以只在这一跃迁上校验。
	if status == store.ClientStatusActive && before.Status != store.ClientStatusActive {
		conflicts := s.checkActivationConflicts(r.Context(), before.ClientID)
		if len(conflicts) > 0 {
			msgs := conflictMessages(conflicts)
			summary := "无法启用：该客户端占用的端口中有 " + strconv.Itoa(len(msgs)) +
				" 个已被其他客户端占用。请先释放这些端口（停用或删除占用方、或到客户端表里改端口后重新生成），再启用。"
			writeErrorDetails(w, http.StatusConflict, errKindConflict, summary, msgs)
			return
		}
	}

	if err := s.st.UpdateClient(r.Context(), id, name, robotType, platform, note, status); err != nil {
		s.writeRegistryErr(w, err, "台账记录不存在")
		return
	}

	after, err := s.st.ClientByID(r.Context(), id)
	if err != nil {
		s.log.Error("回读台账记录失败", "err", err)
		writeError(w, http.StatusInternalServerError, errKindInternal, "更新已生效但回读失败")
		return
	}

	// 停用/启用会改变端口是否受保护，必须留痕：这是审计里唯一能解释
	// 「某个端口为什么突然能被别人用了」的记录。
	if before.Status != after.Status {
		s.audit(r, "registry."+after.Status, after.ClientID, "success",
			"状态 "+before.Status+" → "+after.Status)
	} else {
		// 其余字段的修改不影响端口，逐项列出便于事后追溯到底改了什么
		// （「更新台账名称或备注」这种笼统描述在追责时等于没写）。
		var changes []string
		if before.DisplayName != after.DisplayName {
			changes = append(changes, "名称 "+before.DisplayName+" → "+after.DisplayName)
		}
		if before.RobotType != after.RobotType {
			changes = append(changes, "机器人类型 "+describeRobotType(before.RobotType)+
				" → "+describeRobotType(after.RobotType))
		}
		// 平台改动必须留痕：它决定下一次改包产出的是 Windows 还是 Linux 包，
		// 是事后判断「为什么给 Linux 盒子生成了装不上的包」的唯一线索。
		// 用 Label 输出中文，审计是给人看的。
		if before.Platform != after.Platform {
			changes = append(changes, "目标平台 "+describePlatform(before.Platform)+
				" → "+describePlatform(after.Platform))
		}
		if before.Note != after.Note {
			changes = append(changes, "备注已修改")
		}
		if len(changes) == 0 {
			changes = append(changes, "无变化")
		}
		s.audit(r, "registry.update", after.ClientID, "success", strings.Join(changes, "；"))
	}

	// 与列表接口同口径：更新的返回值也被前端当成一条 RegistryClient 用，
	// 两个状态字段不能在这里是零值（否则「编辑完立刻变成没装过」）。
	uSeen, uOnline := s.clientPresence(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		// Ports 传 nil：newRegistryClientView 会把它补成 []。更新接口不碰端口，
		// 这里没必要为了填它再查一次库。
		"client":  newRegistryClientView(*after, nil, uSeen, uOnline),
		"message": "已更新",
	})
}

// describeRobotType 把机器人类型转成中文描述，空串显示为「未分类」。
//
// 只在审计文案里用：空串本身是合法取值，但审计里写一个空字符串
// 会让人分不清「清空了」还是「这行日志没记」。
func describeRobotType(v string) string {
	if v == "" {
		return "未分类"
	}
	return pack.RobotTypeLabel(v)
}

// describePlatform 把台账里的平台值变成审计里给人看的中文。
//
// 空串是「未记录」而不是 Windows —— 两者在审计里必须分得开：
// 写成「Windows」会让人误以为当初明确选过 Windows，而实际可能是一台
// v4 迁移前签发的 Linux 盒子，这条区别正是排查「包装不上」的关键。
func describePlatform(v string) string {
	if v == "" {
		return "未记录"
	}
	return pack.Platform(v).Label()
}

// handleRegistryDelete 删除台账记录并释放其端口（DELETE /api/registry/clients/{id}）。
func (s *Server) handleRegistryDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r, "id")
	if !ok {
		return
	}
	c, err := s.st.ClientByID(r.Context(), id)
	if err != nil {
		s.writeRegistryErr(w, err, "台账记录不存在")
		return
	}

	// 删除**之前**先把该客户端占用的端口查出来写进审计。
	//
	// 为什么必须在删除前查：client_port 是 ON DELETE CASCADE，删除台账记录后
	// 这些端口就被级联清掉了，事后再想回答「刚才到底释放了哪几个端口」只能全库反推。
	// 而这条信息有实际价值 —— FRP 不返回代理的本地端口（03-frp-api-contract §4-⑦），
	// 台账是它的唯一副本；一次误删就等于这些数字永久消失，只能靠现场人工复填。
	// 之前审计只写了「释放其占用的端口」却没写是哪些端口，正是这次事故里
	// 两条台账记录无法复原的原因（见 .workbuddy/memory 的 2026-09-24 记录）。
	released, portErr := s.portsOfClient(r.Context(), c.ClientID)
	if portErr != nil {
		// 查不到端口不该阻断删除（用户明确要删），但要在日志里留痕。
		s.log.Warn("删除台账前查询端口占用失败，审计将不含端口明细",
			"client_id", c.ClientID, "err", portErr)
	}

	detail := "删除台账记录并释放其占用的端口：" + c.DisplayName
	if len(released) == 0 {
		detail += "；该记录未登记端口"
	} else {
		detail += "；释放的远端端口：" + strings.Join(released, ", ")
	}

	if err := s.st.DeleteClient(r.Context(), id); err != nil {
		s.writeRegistryErr(w, err, "台账记录不存在")
		return
	}
	// 端口随台账记录级联删除，审计里要明确写出来：
	// 「删掉一条记录」在管理员眼里可能只是清理界面，实际却释放了端口。
	s.audit(r, "registry.delete", c.ClientID, "success", detail)

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "已删除，端口已释放"})
}

// portsOfClient 返回某客户端占用的**远端端口**字符串列表（升序、去重）。
//
// 用远端端口而不是本地端口：冲突判定看的、别人要抢的都是远端端口，
// 本地端口属于目标机器自己的地址空间、可以重复，写进审计反而干扰判断。
//
// 实现上用 ListClientPorts 再按 client_id 过滤，而不是新加一条带 WHERE 的查询：
// 端口总量是「客户端数 × 每客户端代理数」量级（现场几十到几百），
// 全量拉一次的开销可忽略；而且这个删除接口本身是低频操作。
// 为此新增一条几乎相同的 SQL 只会多一处要同步维护的地方。
func (s *Server) portsOfClient(ctx context.Context, clientID string) ([]string, error) {
	all, err := s.st.ListClientPorts(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[int]bool{}
	out := []int{}
	for _, p := range all {
		if p.ClientID != clientID || seen[p.RemotePort] {
			continue
		}
		seen[p.RemotePort] = true
		out = append(out, p.RemotePort)
	}
	sort.Ints(out)

	// 远端端口是数字，转成字符串只为拼审计文案。
	// 统一加 remote 前缀其实更严谨，但审计是给人读的，
	// 这里保持纯数字、由前面的「释放的远端端口：」说明语义。
	strs := make([]string, 0, len(out))
	for _, v := range out {
		strs = append(strs, strconv.Itoa(v))
	}
	return strs, nil
}

// handleRegistryPortRelease 只释放一条端口占用，保留客户端记录
// （DELETE /api/registry/ports/{id}）。
func (s *Server) handleRegistryPortRelease(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r, "id")
	if !ok {
		return
	}
	if err := s.st.DeleteClientPort(r.Context(), id); err != nil {
		s.writeRegistryErr(w, err, "端口占用记录不存在")
		return
	}
	s.audit(r, "registry.port.release", "", "success", "手工释放端口占用记录 #"+r.PathValue("id"))

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "端口已释放"})
}

// ---------- 端口冲突判定 ----------

// portConflict 是一条被拦下的端口冲突。
type portConflict struct {
	Port      int
	ProxyName string
	Owner     store.PortOwner
}

// Message 生成给管理员看的冲突说明。
//
// 两种来源的后果不同，文案必须分开：台账冲突是「我们自己签发过」，
// 管理员能一键释放；实时冲突多半来自不在本平台管理范围内的机器，只能改端口。
func (c portConflict) Message() string {
	owner := c.Owner.ClientName
	if owner == "" {
		owner = c.Owner.ClientID
	}
	if owner == "" {
		owner = "未知客户端"
	}
	where := "代理 " + c.Owner.ProxyName
	if c.Owner.ProxyName == "" {
		where = "一条代理"
	}

	if c.Owner.Source == store.PortSourceRegistry {
		return "远端端口 " + strconv.Itoa(c.Port) + "（代理 " + c.ProxyName + "）已被客户端「" + owner +
			"」的" + where + "占用（clientID " + c.Owner.ClientID + "）。" +
			"请改用其他端口；若那台设备确实不再使用该端口，可到「客户端台账」把它停用或删除以释放。"
	}
	return "远端端口 " + strconv.Itoa(c.Port) + "（代理 " + c.ProxyName + "）当前已被 FRP 上的客户端「" + owner +
		"」的" + where + "占用。该占用不在本平台台账里，可能是在其他机器上手工配置的，" +
		"请改用其他端口，否则新客户端的代理无法注册。"
}

// Messages 把冲突列表转成文案列表。
func conflictMessages(cs []portConflict) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Message())
	}
	return out
}

// checkClientPortConflicts 检查本次客户端配置里的远端端口是否已被占用。
//
// 判定规则：
//   - 只看需要远端端口的类型（TCP/UDP），其余类型不占全局端口；
//   - 台账里属于**同一个 clientID** 的占用不算冲突 —— 那正是「重新生成自己的包」，
//     这是必须允许的路径（改一条代理后重新出包）；
//   - 停用（retired）的台账记录不参与判定，其端口视为已释放。
//
// 返回按端口升序的冲突列表。
func (s *Server) checkClientPortConflicts(ctx context.Context, spec *pack.ClientSpec) []portConflict {
	wanted := map[int]string{} // port → 本次配置里的代理名
	for _, p := range spec.Proxies {
		if p.Type.NeedsRemotePort() && p.RemotePort > 0 {
			wanted[p.RemotePort] = p.Name
		}
	}
	return s.conflictsFor(ctx, wanted, spec.ClientID)
}

// checkActivationConflicts 检查「把某个客户端重新启用」是否会与别人撞端口。
//
// 为什么这个动作也要查：停用表示设备已下线、端口释放，停用期间端口完全可能
// 被分配给了别人。此时直接启用会造出一个「两个启用客户端占同一端口」的状态 ——
// 而这正是本功能要防的事。发现的时机只能在这里，因为设备一旦上线，
// frpc 注册失败的现象离管理台已经很远。
func (s *Server) checkActivationConflicts(ctx context.Context, clientID string) []portConflict {
	rows, err := s.st.ListClientPorts(ctx)
	if err != nil {
		s.log.Error("读取待启用客户端的端口占用失败", "err", err)
		return nil
	}
	wanted := map[int]string{}
	for _, p := range rows {
		if p.ClientID == clientID && p.RemotePort > 0 {
			wanted[p.RemotePort] = p.ProxyName
		}
	}
	return s.conflictsFor(ctx, wanted, clientID)
}

// conflictsFor 在给定的「端口 → 代理名」期望集合上判定冲突。
//
// 所有冲突判定都走这里，避免出现「生成时用一种口径、启用时用另一种」
// 这种只有读代码才发现得了的分叉。ownerClientID 是提出主张的一方，
// 它自己占用的端口不算冲突。
func (s *Server) conflictsFor(ctx context.Context, wanted map[int]string, ownerClientID string) []portConflict {
	if len(wanted) == 0 {
		return nil
	}
	index := s.portOwnerIndex(ctx)

	out := make([]portConflict, 0, len(wanted))
	for port, proxyName := range wanted {
		o, ok := index[port]
		if !ok || o.ClientID == ownerClientID {
			continue
		}
		out = append(out, portConflict{Port: port, ProxyName: proxyName, Owner: o})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Port < out[j].Port })
	return out
}

// portOwnerIndex 汇总当前全部端口占用，按端口索引。
//
// 两个口径合并成一张表用于**判定**（判定只关心「这个端口现在有没有主」），
// 但保留每条记录的 Source，因为两种来源的处理方式完全不同：
// 台账里的占用可以去停用/删除释放，FRP 上的在线占用只能改端口。
//
// 台账优先：同一个端口两边都出现时（客户端已装好并在线）取台账那条，
// 它是管理员真正可操作的那条。
func (s *Server) portOwnerIndex(ctx context.Context) map[int]store.PortOwner {
	index := map[int]store.PortOwner{}

	owners, err := s.st.ActivePortOwners(ctx)
	if err != nil {
		// 台账读不到时不能当成「没有占用」：那等于在一个信息不全的
		// 状态下放行，正好会在最不该放行的时候放行。
		// 这里降级为「只按 FRP 实时占用判定」，并由调用方的日志暴露问题。
		s.log.Error("读取台账端口占用失败，本次判定将只依据 FRP 实时状态", "err", err)
	}
	for _, o := range owners {
		index[o.Port] = o
	}

	state := s.coll.State()
	for _, o := range livePortOwners(state.Snapshot, s.registryNames(ctx)) {
		if _, exists := index[o.Port]; exists {
			continue
		}
		index[o.Port] = o
	}
	return index
}

// ---------- 内部辅助 ----------

// livePortOwners 从 FRP 快照里整理出当前真实在线的端口占用。
//
// 只统计能拿到 remotePort 的代理：离线代理的 conf 为 null，此时端口信息完全
// 不可用（03-frp-api-contract.md §4）。这里既不能报 0，也不能猜 ——
// 把一个未知端口当成占用会让管理员永远改不出可用的配置。
//
// names 是 clientID → 台账显示名的映射，用于让实时列表尽量显示人看得懂的名字；
// 不在台账里的客户端回退到 clientID。
func livePortOwners(snap *collector.Snapshot, names map[string]string) []store.PortOwner {
	if snap == nil {
		return nil
	}
	out := make([]store.PortOwner, 0, len(snap.Proxies))
	for _, p := range snap.Proxies {
		port := p.RemotePort()
		if port <= 0 {
			continue
		}
		short := shortProxyName(p)
		name := names[p.ClientID]
		if name == "" {
			name = p.ClientID
		}
		out = append(out, store.PortOwner{
			Port: port, ClientID: p.ClientID, ClientName: name,
			ProxyName: short, ProxyType: p.Type(),
			Status: p.Status, Source: store.PortSourceLive,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Port != out[j].Port {
			return out[i].Port < out[j].Port
		}
		return out[i].ProxyName < out[j].ProxyName
	})
	return out
}

// shortProxyName 取代理短名。
//
// 代理的完整名是 "user.proxyname"，但短名的权威来源始终是 API 返回的
// Conf.Name，不是自行按点号切分的猜测（03-frp-api-contract.md §4-⑪）。
func shortProxyName(p frpapi.ProxyInfo) string {
	if p.Conf != nil && p.Conf.Name != "" {
		return p.Conf.Name
	}
	_, short := frpapi.SplitQualifiedName(p.Name)
	return short
}

// registryMetas 返回 clientID → 台账元数据（显示名 + 机器人类型）的映射，
// 用于给 FRP 实时数据打上人看得懂的名称与设备分类。
//
// 数据源是 store.ClientMetaIndex 而不是 ListClients：后者有条数上限，
// 超出部分会被当成「未登记」，也就是让显示名称在台账变大之后**悄悄消失**。
//
// 映射包含名称为空、类型为空的记录（值存在但为空）。调用方据此区分
// 「登记过但没填」与「根本没登记」——两者的界面提示不一样。
func (s *Server) registryMetas(ctx context.Context) map[string]store.ClientMeta {
	metas, err := s.st.ClientMetaIndex(ctx)
	if err != nil {
		// 显示名称与设备分类都是附加信息：读不出来就不显示，但不阻断列表本身。
		// 客户端/代理列表是管理员排查问题的主视图，不能因为一个锦上添花的
		// 字段让整页报错。
		s.log.Warn("读取台账元数据失败，本次不展示显示名称与机器人类型", "err", err)
		return nil
	}
	return metas
}

// registryNames 只取名称，供「只需要显示名」的调用方使用（代理列表、端口占用）。
func (s *Server) registryNames(ctx context.Context) map[string]string {
	metas := s.registryMetas(ctx)
	if len(metas) == 0 {
		// 与「读不出来」保持同样的降级语义：nil 映射取值为零值 + ok=false，
		// 调用方一律判定为未登记。两条路径的后果一致，不必分开处理。
		return nil
	}
	out := make(map[string]string, len(metas))
	for id, m := range metas {
		out[id] = m.DisplayName
	}
	return out
}

// registryPortIndex 返回 clientID → (代理短名 → 台账端口信息) 的映射，
// 用于给 FRP 的实时代理补上本地端口。
//
// 降级语义与 registryMetas 完全一致：读不出来就不补，**不阻断列表**。
// 于是「台账读失败」与「这条代理没登记」在界面上都显示成「未登记」——
// 这是刻意的取舍（宁可偶发显示不准，也不要整页打不开），但必须留下日志，
// 否则排查时无从区分这两种情况。
func (s *Server) registryPortIndex(ctx context.Context) map[string]map[string]store.ClientPortMeta {
	idx, err := s.st.ClientPortIndex(ctx)
	if err != nil {
		s.log.Warn("读取台账端口明细失败，本次不展示代理的本地端口", "err", err)
		return nil
	}
	return idx
}

// lookupLocalPort 按 (clientID, 代理短名) 从台账端口索引取本地端口。
//
// 封装成函数是为了让三个调用点（代理列表、客户端详情、端口占用两个口径）
// 用**同一套判据**，尤其别让某一处退化成 `LocalPort > 0` —— 那会把脏数据
// 当成未登记，而且这种退化不会有任何报错。对 nil 映射取下标是安全的。
func lookupLocalPort(idx map[string]map[string]store.ClientPortMeta, clientID, proxyName string) (int, bool) {
	m, ok := idx[clientID][proxyName]
	if !ok {
		return 0, false
	}
	return m.LocalPort, true
}

// clientPresence 整理 FRP 快照里每个 clientID 的两种出现情况。
//
// 两个集合刻意分开，不合并成一个：
//   - seen：clientID 出现在 /api/clients 里。**包含已断开但尚未被 FRP 清理的
//     客户端**（FRP 会保留一段时间），所以它回答的是「这个包到底装上并连过没有」。
//   - online：只看 ClientInfo.Online，回答「现在通不通」。
//
// 只给 seen 的后果是「连过但现在断了」被显示成「没装过」—— 这两者的处置方式
// 完全不同（前者查网络与 frps，后者去现场装包），界面必须能区分。
func (s *Server) clientPresence(ctx context.Context) (seen, online map[string]bool) {
	snap := s.coll.State().Snapshot
	seen, online = map[string]bool{}, map[string]bool{}
	if snap == nil {
		return seen, online
	}
	for _, c := range snap.Clients {
		if c.ClientID == "" {
			continue
		}
		seen[c.ClientID] = true
		if c.Online {
			online[c.ClientID] = true
		}
	}
	return seen, online
}

// pathID 解析路径里的数字 ID，失败时已写出 400。
func (s *Server) pathID(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	raw := r.PathValue(name)
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, errKindBadRequest, "路径参数 "+name+" 不是合法编号")
		return 0, false
	}
	return id, true
}

// registrySpecResponse 是「拿台账记录重开一张打包表单」的响应。
type registrySpecResponse struct {
	Spec pack.ClientSpec `json:"spec"`
	// DefaultsApplied 列出按默认值补齐的**顶层**字段。
	//
	// 台账没有地方存它们（也不该存：token 从不留存、日志级别与 TLS 开关
	// 属于管理员当下的选择），所以重开的这张表单里这些项必然是默认值。
	// 列出它们是为了让界面说清「这些不是你当初填的」——
	// 否则管理员会以为表单每一项都是原样还原，扫一眼就提交。
	DefaultsApplied []string `json:"defaults_applied"`
	// ProxyDefaults 逐条代理列出没能还原的字段。
	//
	// 只列**这条代理用得上**的：给一条 tcp 代理列 custom_domains 是噪音，
	// 而给一条 http 代理不列它就是漏了 —— 那个域名不在任何数据库里，
	// 填不出来，而缺它的包会把服务暴露在错误的虚拟主机上。
	ProxyDefaults []proxyDefaultView `json:"proxy_defaults"`
	// Warnings 是本条记录需要管理员先确认的事项，例如平台未曾记录。
	Warnings []string `json:"warnings"`
}

type proxyDefaultView struct {
	ProxyName string   `json:"proxy_name"`
	Fields    []string `json:"fields"`
}

// handleRegistrySpec 把一条台账记录翻译成一份可直接编辑的打包草稿
// （GET /api/registry/clients/{id}/spec）。
//
// 为什么要这个接口：部署包 ZIP 从来不落盘，服务端也不保留签发时的密钥
// （01-prd §8.5 的刻意取舍），所以「编辑已有部署包」物理上不可能实现为
// 「把旧包解出来改」。能改的只有**当初的输入**，而那份输入的唯一去处就是台账 ——
// 台账记了 clientID、名称、类型、接入点与每条代理的端口。
//
// 于是「改包」的正确形态是：用台账重建一张表单，管理员在上面改，
// 走原有的签发路径产出一个**新**包。新包与旧包共用同一个 clientID，
// 目标机器上覆盖安装即可，不需要重新登记、也不会丢历史。
func (s *Server) handleRegistrySpec(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r, "id")
	if !ok {
		return
	}
	c, err := s.st.ClientByID(r.Context(), id)
	if err != nil {
		s.writeRegistryErr(w, err, "台账记录不存在")
		return
	}
	ports, err := s.st.ListClientPortsByClient(r.Context(), c.ClientID)
	if err != nil {
		s.log.Error("查询台账端口明细失败", "client_id", c.ClientID, "err", err)
		writeError(w, http.StatusInternalServerError, errKindInternal, "查询台账端口明细失败")
		return
	}

	// token 与连接目标取接入点，绝不试图还原旧包的：
	// ① 旧包的 token 从不留存（§8.5）；② 就算留着也必须与接入点一致 ——
	// 两端 token 不一致时 frpc 连不上，而这个故障只出现在目标机器上，
	// 管理台这一侧看不到任何痕迹。
	ep, epErr := endpoint.Load(r.Context(), s.st)
	if epErr != nil {
		// 降级而不是报错：接入点读不出来（典型是 DPAPI 密文随 SQLite 换机器）时
		// 仍要让表单打开，让管理员手工填 —— 否则整台设备的改包入口就废了。
		s.log.Warn("读取客户端接入点失败，改包草稿回退为手工填写", "err", epErr)
	}

	var warnings []string
	var defaults []string

	spec := pack.ClientSpec{
		ServerAddr: ep.Addr,
		ServerPort: ep.PortOrDefault(),
		ClientID:   c.ClientID,
		Name:       c.DisplayName,
		RobotType:  c.RobotType,
		Token:      ep.Token,
		LogLevel:   "info",
		LogMaxDays: 7,
		Proxies:    make([]pack.ProxySpec, 0, len(ports)),
	}
	defaults = append(defaults, "log_level", "log_max_days")
	if ep.Token == "" {
		defaults = append(defaults, "token")
	}

	// 平台：台账记了就用台账的，没记（v4 之前签发的包）按 Windows 处理，
	// 但必须告警让管理员确认 —— 这是唯一能让生成的包**根本装不上**的字段，
	// 猜错的代价远高于让他在下拉里点一下。
	switch c.Platform {
	case "":
		spec.Platform = pack.DefaultPlatform
		warnings = append(warnings,
			"这条记录没有登记目标平台（说明它的包是在本平台支持「记录目标平台」之前签发的），"+
				"已按 "+pack.DefaultPlatform.Label()+" 预填。若这台设备实际运行在别的系统上，"+
				"请先在表单里改对再生成 —— 平台选错时安装脚本不会报错，要等上线才发现。")
	default:
		p := pack.Platform(c.Platform)
		if !p.Valid() {
			// 不可能走到这里（写入路径已校验），但真走到时不能产出一个坏值。
			s.log.Error("台账记录了非法平台值，按默认平台处理",
				"client_id", c.ClientID, "platform", c.Platform)
			spec.Platform = pack.DefaultPlatform
			warnings = append(warnings, "台账里的目标平台值无法识别，已按 "+pack.DefaultPlatform.Label()+" 预填，请核对。")
		} else {
			spec.Platform = p
		}
	}

	var proxyDefaults []proxyDefaultView
	for _, p := range ports {
		pt := pack.ProxyType(p.ProxyType)
		if pt == "" {
			pt = pack.ProxyTCP // 补录入口允许省略类型，默认 tcp
		}
		spec.Proxies = append(spec.Proxies, pack.ProxySpec{
			Name:        p.ProxyName,
			Type:        pt,
			LocalIP:     "127.0.0.1",
			LocalPort:   p.LocalPort,
			RemotePort:  p.RemotePort,
			CustomHosts: []string{},
			Locations:   []string{},
		})

		// 只报这条代理真正缺的东西，不给用不上的也列出来 ——
		// 「每一行都列七八个字段」的结果是没有人会看，等于没提示。
		var fields []string
		fields = append(fields, "local_ip")
		switch pt {
		case pack.ProxyHTTP, pack.ProxyHTTPS:
			fields = append(fields, "custom_domains", "subdomain", "locations")
		case pack.ProxySTCP, pack.ProxyXTCP:
			fields = append(fields, "secret_key")
		}
		proxyDefaults = append(proxyDefaults, proxyDefaultView{
			ProxyName: p.ProxyName,
			Fields:    fields,
		})
	}
	if len(ports) == 0 {
		warnings = append(warnings,
			"这条台账记录没有登记任何代理，表单里是空的一组。签发后它只会装上一个不带代理的 frpc。")
	}

	writeJSON(w, http.StatusOK, registrySpecResponse{
		Spec:            spec,
		DefaultsApplied: defaults,
		ProxyDefaults:   proxyDefaults,
		Warnings:        warnings,
	})
}

// writeRegistryErr 把仓储错误映射为状态码。
func (s *Server) writeRegistryErr(w http.ResponseWriter, err error, notFoundMsg string) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, errKindNotFound, notFoundMsg)
		return
	}
	s.log.Error("台账操作失败", "err", err)
	writeError(w, http.StatusInternalServerError, errKindInternal, "台账操作失败")
}
