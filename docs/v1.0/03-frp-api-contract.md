# FRP 0.69.0 / 0.71.0 Dashboard API 实测契约

> 版本：v1.1  
> 日期：2026-09-21（0.69.0 取证）／2026-09-25（0.71.0 复核）  
> 状态：已实测确认；**0.71.0 已复核，契约无变化**  
> 关联：`00-opensource-research.md` §3.2、`01-prd.md` F04–F08

## 0. 0.71.0 复核结论（2026-09-25）

平台内嵌二进制已由 0.69.0 升级到 0.71.0。升级前做了三项实测，证据在
`.workbuddy/test/_frp071/`（`compare.txt`、`mix.txt`、`shape.txt`）：

| 项 | 结论 |
|---|---|
| 配置校验 `verify -c` | 7 种代理类型中 6 种（tcp/udp/http/https/stcp/xtcp）与 `transport.tls`、`frps.toml` 两版结果一致；**tcpmux 两版都失败**（见下） |
| 混合版本连通性 | frps 0.69 + frpc 0.71、frps 0.71 + frpc 0.69 **两组均 `online:true`**，与同版本组合无异 |
| 响应形状 | `/api/serverinfo`、`/api/proxy/{type}` 两版**逐字节一致**（仅 `version` 与时间戳不同） |

由此得出三条对代码的结论：

1. **`internal/frpapi` 零改动**——字段与 JSON 标签均未变。
2. **F26「FRP 不返回代理本地端口」在 0.71.0 依旧成立**（`conf` 仍只有
   `localIP` + `remotePort`）⇒ 台账回填 `local_port` 的逻辑**不能删**。
3. **升级可以分批做**：先升服务端 frps，再逐台重装客户端，过渡期混跑安全。

### 0.1 顺带确认的两处变更

- **tcpmux 在两版都不可用**（`proxy X: not support multiplexer:`）。这不是
  0.71.0 引入的，而是平台渲染结构 `tomlProxy` 里压根没有 `multiplexer` 字段；
  服务端也没渲染 `tcpmuxHTTPConnectPort`。该类型因此**从支持清单移除**，
  详见 `internal/pack/spec.go` 的 `AllProxyTypes` 注释。
- **0.70.0 起 frpc 拒绝重复代理名**（`proxy name [x] is duplicated`）。
  平台的 `ClientSpec.Validate()` 早有更严格的重名校验，先于 FRP 拦截，
  因此对本平台无影响。

## 1. 实测方法

不依赖任何第三方文档或记忆，直接对本机 FRP 二进制发起真实请求取证。

| 项 | 内容 |
|---|---|
| 二进制 | `frps.exe` / `frpc.exe`，均为 0.69.0 |
| 实测端口 | frps `bindPort = 17000`，Dashboard `127.0.0.1:17500`（刻意避开生产 7000 / 7500） |
| 探测配置 | `.workbuddy/probe/probe-frps.toml`、`.workbuddy/probe/probe-frpc.toml`，不参与交付 |
| 采集状态 | ① 空节点基线 ② 1 个客户端 + 2 个 TCP 代理在线 ③ 客户端离线后 |
| 原始记录 | `.workbuddy/probe/api-dump.txt` 至 `api-dump4.txt` |

## 2. 端点与响应结构

### 2.1 `GET /api/serverinfo`

```json
{"version":"0.69.0","bindPort":17000,"vhostHTTPPort":0,"vhostHTTPSPort":0,"tcpmuxHTTPConnectPort":0,"kcpBindPort":0,"quicBindPort":0,"subdomainHost":"","maxPoolCount":5,"maxPortsPerClient":0,"heartbeatTimeout":-1,"totalTrafficIn":0,"totalTrafficOut":0,"curConns":0,"clientCounts":0,"proxyTypeCount":{}}
```

| 字段 | 类型 | 说明 |
|---|---|---|
| `version` | string | FRP 版本，用于兼容性闸门 |
| `bindPort` | int | 客户端接入端口 |
| `vhostHTTPPort` / `vhostHTTPSPort` / `tcpmuxHTTPConnectPort` / `kcpBindPort` / `quicBindPort` | int | 未启用时为 `0` |
| `subdomainHost` | string | 未配置时为空串 |
| `maxPoolCount` | int | 默认 5 |
| `maxPortsPerClient` | int | `0` 表示不限制 |
| `heartbeatTimeout` | int | `-1` 表示使用内置默认 |
| `totalTrafficIn` / `totalTrafficOut` | int64 | 字节数；**FRP 进程重启后归零** |
| `curConns` | int64 | 当前连接数 |
| `clientCounts` | int64 | **在线客户端数**（不是注册总数），见 §4-⑨ |
| `proxyTypeCount` | map[string]int64 | key 的**出现集合会随历史变化**，值可为 0 |

### 2.2 `GET /api/clients`

返回**裸数组**，不是对象包裹。

```json
[{"key":"probeuser.probe-client-001","user":"probeuser","clientID":"probe-client-001","runID":"ecad9ba22fa15c3b","version":"0.69.0","wireProtocol":"v1","hostname":"dutyangBim","clientIP":"127.0.0.1","firstConnectedAt":1789972554,"lastConnectedAt":1789972554,"online":true}]
```

客户端离线后同一条记录变为：

```json
[{"key":"probeuser.probe-client-001","user":"probeuser","clientID":"probe-client-001","runID":"","version":"0.69.0","wireProtocol":"v1","hostname":"dutyangBim","clientIP":"127.0.0.1","firstConnectedAt":1789972554,"lastConnectedAt":1789972554,"disconnectedAt":1789972599,"online":false}]
```

| 字段 | 类型 | 在线时 | 离线后 | 说明 |
|---|---|---|---|---|
| `key` | string | `user.clientID` | 同 | 详情接口的路径参数 |
| `user` | string | `probeuser` | 同 | 未配置时为空串 |
| `clientID` | string | `probe-client-001` | 同 | **唯一稳定的设备标识** |
| `runID` | string | `ecad9ba2…` | **空串** | 离线即清空，**不可作为持久引用** |
| `version` | string | `0.69.0` | 同 | 客户端版本 |
| `wireProtocol` | string | `v1` | 同 | 通信协议版本，**不是代理类型** |
| `hostname` | string | `dutyangBim` | 同 | 客户端主机名 |
| `clientIP` | string | `127.0.0.1` | 同 | 客户端来源 IP |
| `firstConnectedAt` | int64 | `1789972554` | 同 | **Unix 秒级时间戳** |
| `lastConnectedAt` | int64 | `1789972554` | 同 | **Unix 秒级时间戳** |
| `disconnectedAt` | int64 | **字段不存在** | `1789972599` | 仅在离线时出现 |
| `online` | bool | `true` | `false` | 唯一的在线判定依据 |

### 2.3 `GET /api/clients/{key}`

返回单个客户端对象，结构与 §2.2 的数组元素完全相同。key 不存在时返回 `404`。

### 2.4 `GET /api/proxy/{type}`

返回**对象包裹**（与 `/api/clients` 的裸数组不同）：

```json
{"proxies":[{"name":"probeuser.probe-tcp-1","conf":{"name":"probeuser.probe-tcp-1","type":"tcp","transport":{"bandwidthLimit":"","bandwidthLimitMode":"client"},"loadBalancer":{"group":""},"healthCheck":{"type":"","intervalSeconds":0},"localIP":"127.0.0.1","plugin":null,"remotePort":16001},"user":"probeuser","clientID":"probe-client-001","todayTrafficIn":0,"todayTrafficOut":0,"curConns":0,"lastStartTime":"09-21 14:35:54","lastCloseTime":"","status":"online"}]}
```

代理离线后 `conf` 变为 `null`：

```json
{"name":"probeuser.probe-tcp-1","conf":null,"user":"probeuser","clientID":"probe-client-001","todayTrafficIn":0,"todayTrafficOut":0,"curConns":0,"lastStartTime":"09-21 14:35:54","lastCloseTime":"09-21 14:36:39","status":"offline"}
```

| 字段 | 类型 | 在线时 | 离线后 | 说明 |
|---|---|---|---|---|
| `name` | string | `user.proxyname` | 同 | **完整名**，非用户填写的短名 |
| `conf` | object / **null** | 见下 | **`null`** | 离线即丢失全部配置详情 |
| `user` | string | `probeuser` | 同 | |
| `clientID` | string | `probe-client-001` | 同 | 用于关联客户端 |
| `todayTrafficIn` / `todayTrafficOut` | int64 | `0` | `0` | **今日**流量，非累计 |
| `curConns` | int64 | `0` | `0` | |
| `lastStartTime` / `lastCloseTime` | string | `"09-21 14:35:54"` / `""` | / `"09-21 14:36:39"` | **`MM-DD HH:mm:ss`，无年份** |
| `status` | string | `"online"` | `"offline"` | **字符串，不是 bool** |

`conf` 子结构（仅在线时存在）：`name`、`type`、`transport{bandwidthLimit,bandwidthLimitMode}`、`loadBalancer{group}`、`healthCheck{type,intervalSeconds}`、`localIP`、`plugin`、`remotePort`。

### 2.5 `GET /api/proxy/{type}/{name}`

返回单个代理对象，结构与 §2.4 的数组元素相同。名称不存在时返回 `404`。

### 2.6 `GET /api/traffic/{name}`

```json
{"name":"probeuser.probe-tcp-1","trafficIn":[0,0,0,0,0,0,0],"trafficOut":[0,0,0,0,0,0,0]}
```

- **必须使用完整名**（§2.4 的 `name` 字段）。传用户填写的短名会返回 `404`。
- 数组长度为 **7**，对应最近若干时间单位的入站 / 出站字节数。

## 3. 鉴权与错误行为

| 场景 | HTTP | 响应体 |
|---|---|---|
| 无 `Authorization` 头 | `401` | — |
| Basic Auth 密码错误 | `401` | — |
| 不存在的路由 | `404` | — |
| 不存在的 client key | `404` | — |
| 不存在的 proxy 名 | `404` | — |
| 任意代理类型（含 `bogus`） | **`200`** | `{"proxies":[]}` |
| traffic 用短名 | `404` | `{"Code":404,"Msg":"no proxy info found"}` |

注意：错误响应体使用**大写** `Code` / `Msg`，与正常响应的小写字段风格不一致。

## 4. 与 `00-调研` / `01-PRD` 既有描述的差异

以下 12 条均以实测为准，需回写修订上游文档。

| # | 文档描述 | 实测结果 | 影响 |
|---|---|---|---|
| ① | 调研 §3.2「首次/最近连接时间」 | `firstConnectedAt` / `lastConnectedAt`，**Unix 秒级 int64** | 按字符串解析会直接失败 |
| ② | 调研 §3.2「离线时间」 | `disconnectedAt`，**仅离线时字段存在** | 用 `int64` 会误判为 1970 年，必须用指针 |
| ③ | 调研 §3.2「协议」 | 实为 `wireProtocol`（值 `v1`），与代理类型无关 | 字段语义误解 |
| ④ | 调研 §3.1 用 `runID` 识别设备 | **离线后 `runID` 变空串** | 只有 `clientID` 可作持久标识，印证 PRD §8.1 |
| ⑤ | PRD F07「流量」 | 实为 `todayTrafficIn/Out`，**今日**语义 | 跨日或重启归零，长期趋势必须自采样 |
| ⑥ | PRD F07「状态」 | `status` 为**字符串** `online`/`offline` | 与客户端的 bool `online` 不一致，不能统一处理 |
| ⑦ | PRD F07「端点」 | 端点位于 `conf.remotePort`，**代理离线时 `conf` 为 `null`** | 离线代理无法展示端点，需管理平台自行留存 |
| ⑧ | PRD F07「启停时间」 | `"MM-DD HH:mm:ss"`，**无年份**，且与客户端时间戳格式不同 | 跨年歧义；两套时间表示并存 |
| ⑨ | PRD F04「客户端数」 | `clientCounts` 是**在线数**；离线客户端仍留在 `/api/clients` | 总数 / 离线数必须自行统计 |
| ⑩ | （隐含）可按类型探测 | 任意类型均返回 `200` | 代理类型必须用白名单枚举 |
| ⑪ | PRD F09 用用户填写的代理名 | API 返回的是 `user.proxyname` 完整名 | 重名检查与详情查询都要用完整名 |
| ⑫ | 调研 §3.2 未提及 | `/api/clients` 是裸数组，`/api/proxy/{type}` 是对象包裹 | 两者解码方式不同 |

## 5. 对实现的硬性要求

1. `disconnectedAt` 使用 `*int64`，以区分「字段缺失」与「值为 0」。
2. `firstConnectedAt` / `lastConnectedAt` 按 Unix 秒解析，前端统一转换并按本地时区展示。
3. 代理 `conf` 为 `null` 时必须降级渲染，不得 panic；离线代理的端点信息由管理平台自己的记录补齐。
4. 内部一律以 API 返回的完整名（`user.proxyname`）作为代理主键；用户填写的短名只用于表单。
5. `status` 按字符串处理，`online` 按布尔处理，不要抽象成同一个字段。
6. 首页的客户端总数、在线数、离线数由管理平台对 `/api/clients` 自行统计，`clientCounts` 仅作在线数交叉校验。
7. 代理类型白名单硬编码为 `tcp` / `udp` / `http` / `https` / `tcpmux` / `stcp` / `xtcp`。
8. 所有时间展示以管理平台自己的采集时间为准，不使用 API 返回的无年份时间串。
9. 调用 `/api/traffic/{name}` 前，必须先取得完整名。
10. 代理的**本地端口（`localPort`）不由 Dashboard 返回**（见 §4-⑦：`conf` 只有 `localIP` 与 `remotePort`）。实时视图要展示本地端口，必须按 `(clientID, 代理短名)` 从管理平台自己的台账补齐；短名先用 `SplitQualifiedName` 从返回的完整名里拆出来。补齐不了时标注「未登记」，**不得以 0 代替**（0 是非法端口号）。该字段**不参与端口冲突判定**。

## 变更记录

| 日期 | 原因 | 改动点 | 影响范围 |
|---|---|---|---|
| 2026-09-21 | 初版实测契约 | 对 frps 0.69.0 真实响应取证，记录 12 处与既有文档的差异与 9 条实现约束 | 系统设计、`internal/frpapi` 实现、F04–F08 验收口径 |
| 2026-09-24 | 补记「本地端口不可得」这条约束 | §5 新增第 10 条：代理 `localPort` **不由 Dashboard 返回**（§4-⑦ 只记了 `conf` 的内容与「离线时为 null」，未点明在线也拿不到本地端口）。实时视图要展示它，必须按 `(clientID, 代理短名)` 从管理平台自己的台账补齐；短名先用 `SplitQualifiedName` 从完整名拆出。补不到时标注「未登记」，**不得以 0 代替**。该字段不参与端口冲突判定 | `internal/web/read.go`、`internal/web/registry_handlers.go`、四处端口展示界面、`registry-test.ps1`（+4 断言） |

---

**门禁**：本文档的结论以实测为准。§4 的 12 条差异需回写修订 `00-opensource-research.md` 与 `01-prd.md` 的对应描述。
