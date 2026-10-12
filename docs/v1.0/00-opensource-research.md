# FRP 客户端管理监控软件开源与架构调研

> 版本：v1.0  
> 日期：2026-09-21  
> 状态：已确认

## 1. 调研目标

- 要解决的核心问题：基于现有 FRP 0.69.0，在不修改 FRP 核心代码的前提下，集中查看连接到 frps 的全部客户端、代理与流量状态，并生成可直接在 Windows 上安装/卸载和开机自启动的客户端、服务器端脚本。
- 期望借鉴的内容：FRP 官方管理接口、客户端状态模型、多节点管理方式、服务安装方案、管理台信息架构和安全边界。
- 当前输入基线：`D:\downloads\frp` 包含 `frps.exe`、`frpc.exe`、TOML 配置、启动批处理和 `nssm.exe`；`frps.exe --version` 与 `frpc.exe --version` 均为 `0.69.0`。
- 当前仓库状态：`D:\sinyeFrp` 是空工作区且没有 Git 仓库。本阶段不初始化版本控制。

## 2. 检索记录

| 渠道类型 | 具体渠道 | 关键词 | 结论 |
|---|---|---|---|
| 开源实现 | GitHub | `frp panel`、`frps dashboard`、`frpc desktop` | 找到 FRP-Panel、frps-panel、go-frp-panel、Frpc-Desktop 等候选。 |
| 开源合集 | GitHub 搜索/项目引用 | `awesome frp`、`frp web ui` | 未发现比候选项目更权威、且专门覆盖本需求的活跃合集；以官方项目及活跃面板为主。 |
| 包仓库 | npm / PyPI | `frp admin api` | 本项目技术栈尚未确定，包仓库主要是零散封装，不适合作为产品基座；后续按技术选型选择成熟的 HTTP、TOML 和 SQLite 库。 |
| 架构设计资料 | FRP 官方文档与 v0.69.0 源码 | `Web Interface`、`Server Management`、`/api/clients`、`server plugin` | Dashboard API 已提供客户端、服务器、代理和流量数据；插件 RPC 可扩展登录、心跳和代理事件。 |
| 系统设计资料 | Microsoft Learn、WinSW、NSSM | `Windows service create`、`auto start` | 原生 Windows 服务要求目标程序遵守 Service Control Manager 协议；普通控制台程序更适合使用 WinSW/NSSM 包装或计划任务。 |
| 产品与交互参考 | FRP 内置 Dashboard、FRP-Panel、Frpc-Desktop | `client monitoring UI`、`proxy list`、`logs` | 管理台应以总览、客户端、代理、脚本生成和系统设置为主，不照搬云平台的计费/多租户复杂度。 |

## 3. 现有 FRP 0.69.0 能力验证

### 3.1 当前配置现状

- `frps.toml` 仅设置 `bindPort = 7000`，没有启用 `webServer`，所以当前没有可供管理软件读取的 Dashboard/API。
- `frpc.toml` 配置了两个 TCP 代理，但没有设置稳定的 `user`、`clientID` 或用于展示的 metadata。管理软件仍可从 0.69.0 的客户端注册表获得 `runID`、主机名、IP 和版本；为了让设备长期可识别，后续生成器应写入唯一 `clientID` 和人类可读名称。
- 当前客户端指向的服务器端口与所附 `frps.toml` 的 `bindPort` 不一致。这可能表示两份配置部署在不同机器，或服务端实际配置不在本目录。PRD 阶段需要把“本机管理单节点”与“远程管理节点”的边界写清。

### 3.2 官方 API

对 FRP `v0.69.0` 源码及同版本二进制进行核验：临时在 `127.0.0.1:17500` 启动 Dashboard，Basic Auth 调用成功。

| 接口 | 可获得数据 | 用途 |
|---|---|---|
| `GET /api/serverinfo` | FRP 版本、端口、总流量、当前连接数、客户端数、各代理类型数量 | 总览与服务健康状态 |
| `GET /api/clients` | key、user、clientID、runID、版本、协议、主机名、客户端 IP、首次/最近连接时间、离线时间、在线状态 | 全部客户端列表与在线判定 |
| `GET /api/clients/{key}` | 单个客户端详情 | 客户端详情页 |
| `GET /api/proxy/{type}` | 代理配置、user、clientID、流量、连接数、启停时间、状态 | 代理列表 |
| `GET /api/proxy/{type}/{name}` | 单个代理详情 | 代理详情页 |
| `GET /api/traffic/{name}` | 代理历史入站/出站流量 | 流量趋势图 |
| `GET /metrics` | Prometheus 指标（需 `enablePrometheus = true`） | P1 长期指标采集或接入既有监控体系 |

实测空节点响应为：版本 `0.69.0`、客户端数 `0`、`/api/clients` 返回空数组，HTTP 200。

### 3.3 Server Plugin 扩展能力

FRP 官方插件通过 JSON/HTTP 同步回调提供 `Login`、`NewProxy`、`CloseProxy`、`Ping`、`NewWorkConn` 和 `NewUserConn`。它适合后续实现登录审计、端口策略和多用户鉴权，但不应作为首版只读监控的必需依赖：插件异常会进入 FRP 控制链路，增加部署与可用性风险。

### 3.4 API 的关键限制

- Dashboard API 使用 Basic Auth，官方未承诺把它作为跨版本稳定的公共 SDK；管理软件必须做版本探测、超时、错误隔离和兼容性测试。
- Dashboard 应优先仅监听 `127.0.0.1`。若管理软件与 frps 不同机部署，应通过防火墙白名单、TLS 或反向代理保护，不能直接把弱口令 Dashboard 暴露到公网。
- FRP 内存统计会随进程重启丢失。若要长期趋势、告警历史和离线审计，管理软件需要自己的持久化存储与定时采样。
- 当前 `nssm.exe` 文件时间为 2014 年，SHA-256 为 `F689EE9AF94B00E9E3F0BB072B34CAAF207F32DCB4F5782FC9CA351DF9A06C97`。在未核对可信发布源和签名之前，不应默认分发这份二进制。

## 4. 候选开源项目评估

数据截至 2026-09-21；Star 为调研时近似值。

| 项目 | 地址 | Star | 最近维护 | License | 技术栈/定位 | 评估 |
|---|---|---:|---|---|---|---|
| fatedier/frp | https://github.com/fatedier/frp | 约 110k | 2026-09-09 | Apache-2.0 | Go + Vue，官方 FRP | 直接依赖现有 0.69.0 二进制与管理 API，是事实标准。 |
| VaalaCat/frp-panel | https://github.com/VaalaCat/frp-panel | 约 1.8k | 2026-04-19 | AGPL-3.0 | Master/Server/Client 多节点架构，TypeScript 为主 | 功能最接近，可借鉴集中配置和节点拓扑；AGPL 对分发和网络服务有强约束，不复制其代码。 |
| yhl452493373/frps-panel | https://github.com/yhl452493373/frps-panel | 约 130 | 2024-01-11 | Apache-2.0 | FRP Server Plugin，多用户与流量面板 | 插件接口与权限模型值得参考，但维护已停滞约两年，不作为基座。 |
| xxl6097/go-frp-panel | https://github.com/xxl6097/go-frp-panel | 约 24 | 2026-07-18 | MIT | Go，服务安装、客户端生成、多实例 | 与脚本生成需求高度接近，可借鉴安装流程；社区和测试成熟度信号较弱，需独立验证。 |
| luckjiawei/frpc-desktop | https://github.com/luckjiawei/frpc-desktop | 约 6.9k | 2026-09-16 | MIT | Electron 客户端，配置导入、日志、自启动 | 适合借鉴 Windows 客户端配置体验，但不能解决服务端统一监控。 |
| WinSW | https://github.com/winsw/winsw | 约 14k | 2026-05-30 | MIT | Windows Service Wrapper | 活跃、用途单一明确，可作为普通控制台程序注册 Windows 服务的候选；引入时需固定版本并校验哈希。 |

## 5. 架构设计资料评估

| 资料 | 来源/链接 | 核心方案 | 匹配度 | 可借鉴点 |
|---|---|---|---|---|
| FRP Web Interface | https://gofrp.org/en/docs/features/common/ui/ | frps 内置 Web Server，通过 Basic Auth 暴露 Dashboard 和 API | 高 | 使用官方实时数据源，不修改 FRP 内核。 |
| FRP Server Management | https://gofrp.org/en/docs/features/common/server-manage/ | 外部 HTTP 插件参与登录、心跳、代理和连接事件 | 中 | P1 审计与策略扩展；首版保持只读、避免进入关键控制链路。 |
| FRP v0.69.0 Server Plugin 文档 | https://github.com/fatedier/frp/blob/v0.69.0/doc/server_plugin.md | JSON/HTTP 回调协议与字段定义 | 高 | 作为未来事件采集和鉴权的接口契约。 |
| Microsoft `New-Service` | https://learn.microsoft.com/en-us/powershell/module/microsoft.powershell.management/new-service | 通过 SCM 创建 Windows 服务 | 中 | 生成脚本需管理员权限、幂等检测、明确的安装/卸载/状态查询。 |
| Microsoft `sc.exe create` | https://learn.microsoft.com/en-us/windows-server/administration/windows-commands/sc-create | Windows 原生服务注册命令 | 中 | 可管理服务配置，但不能让任意控制台程序天然响应 SCM；不可直接把 frpc/frps 当作原生服务程序注册。 |
| WinSW | https://github.com/winsw/winsw | 用受维护的 wrapper 托管控制台程序，提供恢复与日志策略 | 高 | 推荐的服务模式候选，优于未经校验的旧 NSSM 文件。 |

## 6. 可借鉴点提炼

| 来源 | 借鉴内容 | 如何应用到本项目 |
|---|---|---|
| FRP 0.69.0 官方 API | 客户端注册表、代理统计、服务器统计 | 管理服务以只读适配器采集，前端不直接持有 Dashboard 凭据。 |
| FRP Server Plugin | 登录、心跳、代理事件 | 作为可选扩展，不阻塞首版；需要独立超时与故障降级设计。 |
| FRP-Panel | Master/节点分层、集中配置 | 首版按单节点实现，但数据模型预留多个 frps 节点，不引入其 AGPL 代码。 |
| frps-panel | 用户、端口与域名限制 | 后续权限/策略模块参考，首版不做复杂多租户。 |
| Frpc-Desktop | TOML 导入、日志、自启动体验 | 生成 Windows 包时同时提供配置、安装、卸载、启动、停止和状态脚本。 |
| WinSW / Microsoft SCM | 服务生命周期与恢复 | 生成幂等 PowerShell 脚本；推荐 WinSW 服务模式，提供计划任务作为无 wrapper 备选。 |

## 7. 调研结论

- [ ] 直接 fork 某个完整面板项目。
- [x] **借鉴设计并自研薄管理层**：保留官方 `frps.exe` / `frpc.exe`，基于 FRP 0.69.0 Dashboard API 构建独立管理服务与 Web 控制台；使用 SQLite 保存节点配置、采样历史、脚本模板和审计记录；生成 Windows 可直接运行的部署包。
- [ ] 完全脱离现有方案重新实现。

选择理由：

1. 官方 0.69.0 已原生提供完整客户端列表，没必要 fork 或修改 FRP。
2. 完整第三方面板普遍比需求更重，且最成熟候选使用 AGPL-3.0；直接复用会引入许可证和升级维护成本。
3. 独立适配层能隐藏 Dashboard 凭据，隔离 FRP API 版本差异，并为历史数据、告警与脚本生成提供稳定接口。
4. 首版聚焦“看得到、识别准、装得上、能自启动”，再按真实使用反馈扩展多用户鉴权和远程控制。

## 8. 建议的首版边界（供 PRD 阶段确认）

### P0 候选

- 管理员登录。
- 单个 frps 节点的服务状态、在线/离线客户端、客户端详情、代理列表、连接数和流量展示。
- 客户端名称、`clientID`、主机名、IP、FRP 版本、首次/最近在线时间展示与搜索筛选。
- 生成客户端包：`frpc.exe`、`frpc.toml`、安装/卸载/启动/停止/状态 PowerShell 脚本。
- 生成服务器端包：`frps.exe`、安全化 `frps.toml`、安装/卸载/启动/停止/状态 PowerShell 脚本。
- 脚本重复执行安全、管理员权限检查、路径转义、日志目录和服务失败恢复。
- Dashboard 仅本机监听，随机强密码由管理服务保存；所有密钥在 UI 默认遮蔽。

### P1 候选

- 多 frps 节点、历史趋势、客户端上下线事件、阈值告警、Prometheus 接入。
- TOML 导入/校验、端口冲突检测、配置版本与回滚。
- 可选 Server Plugin，用于登录审计和代理策略。

### 暂不纳入

- 修改 FRP 协议或 fork FRP 内核。
- 公网 SaaS、多租户计费、自动申请域名/证书、复杂 RBAC。
- 从管理台远程执行任意命令；这会显著扩大安全风险。

## 9. 对后续文档的影响

- 对 PRD：需要明确单节点还是多节点首发、管理软件与 frps 是否同机、首版是否需要告警，以及最终交付偏好（便携 ZIP 或安装程序）。
- 对交互文档：页面建议为总览、客户端、代理、部署包生成、系统设置五个主区域；采用运维工具的紧凑信息布局。
- 对系统设计：管理后端作为唯一 FRP API 调用方；Dashboard 默认仅环回监听；状态采集与业务请求解耦；脚本模板采用结构化配置生成，禁止直接字符串拼接未经校验的用户输入。
- 对测试：必须用真实 `frps 0.69.0 + frpc 0.69.0` 做在线、重连、离线、代理流量和 Windows 服务重启回归；生成脚本需在含空格路径和非 ASCII 用户目录下验证。

## 10. 参考资料

- FRP 官方仓库：https://github.com/fatedier/frp
- FRP Web Interface：https://gofrp.org/en/docs/features/common/ui/
- FRP Server Management：https://gofrp.org/en/docs/features/common/server-manage/
- FRP 0.69.0 Server Plugin：https://github.com/fatedier/frp/blob/v0.69.0/doc/server_plugin.md
- FRP-Panel：https://github.com/VaalaCat/frp-panel
- frps-panel：https://github.com/yhl452493373/frps-panel
- go-frp-panel：https://github.com/xxl6097/go-frp-panel
- Frpc-Desktop：https://github.com/luckjiawei/frpc-desktop
- WinSW：https://github.com/winsw/winsw
- Microsoft New-Service：https://learn.microsoft.com/en-us/powershell/module/microsoft.powershell.management/new-service
- Microsoft sc.exe create：https://learn.microsoft.com/en-us/windows-server/administration/windows-commands/sc-create

## 变更记录

| 日期 | 原因 | 改动点 | 影响范围 |
|---|---|---|---|
| 2026-09-21 | 初始调研 | 完成 FRP 0.69.0 能力实测、候选项目和 Windows 自启动方案评估 | 后续 PRD、交互和系统设计 |
| 2026-09-21 | 用户确认方案方向 | 确认基于 FRP 自带服务封装管理层并生成 Windows 部署包 | 进入 PRD 阶段 |

---

**门禁**：本调研经用户确认后，进入产品文档阶段；确认前不编写业务代码或修改 FRP 配置。
