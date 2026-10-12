# SinyeFRP 管理平台

给自建 FRP 服务器用的 Windows 管理平台。**只读监控** + **表单化出包** + **开机自启动部署**。

它跑在 `frps.exe` 外层，通过 FRP 自带的 Dashboard API 读取状态，不修改、不 fork FRP 内核，也不需要 Server Plugin。管理平台停掉不影响已经建好的穿透。

- 内嵌 FRP **0.71.0**（同时兼容 0.69.x）
- Go 1.27.1 单文件，前端与 FRP 二进制一起 `go:embed`，**部署时不需要装 Go / Node / Python**
- 当前版本 `v0.1.0-dev`

---

## 先看清边界

| 它做 | 它不做 |
|---|---|
| 读 Dashboard API 展示服务器、客户端、代理状态 | 控制 FRP（不启停、不改 frps 配置、不下发指令） |
| 按表单生成客户端 / 服务端 / 平台自身部署包 | 托管多租户、计费、组网、WebSSH |
| 生成带计划任务、可卸载、有退出码的生命周期脚本 | 代替 FRP 官方文档解释协议细节 |
| 记住签发了谁、占了哪些远端端口，撞端口时拦下来 | 保存旧包的密钥（ZIP 不落盘，密钥不留存） |

> **管理平台解耦于 FRP**：它只是 FRP API 凭据的唯一持有方，浏览器不直接请求 Dashboard。管理服务挂了，`frps.exe` / `frpc.exe` 照常转发。

---

## 快速开始

### 方式一：直接用部署包（不用编译）

拿到 `SinyeFRP-platform-<版本>.zip` 后：

1. 解压到目标机器（路径可以有空格）
2. **双击 `一键安装.bat`** —— 它只负责请求管理员权限并转交给 `install.ps1`，不重复实现任何安装逻辑；习惯命令行的照旧 `.\install.ps1`
3. 浏览器打开 **http://127.0.0.1:7800**，进入初始化向导

### 方式二：从源码构建

```powershell
# 1. 拉 FRP 二进制（仓库不含二进制，见下方「为什么」）
.\scripts\fetch-frp.ps1

# 2. 前端构建 + 复制 assets + go build，输出 bin\sinyefrp.exe
.\scripts\build.ps1

# 3. 启动（默认读 .\config.toml，监听 127.0.0.1:7800）
.\bin\sinyefrp.exe
```

要求：Go 1.27+、Node 18+、PowerShell 5.1。

**为什么二进制不在仓库里**：`frp\` 与 `internal\pack\assets\` 下合计约 130 MB，而它们可以从上游 Release 按版本重新下载。把二进制提交进去的代价在第一次升级 FRP（0.69 → 0.71）时就会显现——git 历史里永久多压一份，且每次升级再堆一份。现在改由 `fetch-frp.ps1` 按 `0.71.0` 拉取，实测哈希与既有二进制一致。

---

## 首次配置：三步，别跳第二步

| 步骤 | 位置 | 说明 |
|---|---|---|
| 1 | 初始化向导 | 建管理员账号；填 FRP Dashboard 地址、用户名、密码。凭据用 **DPAPI 加密**存 SQLite，不以明文回前端 |
| 2 | 设置 → **客户端接入点** | 填 frpc 要连的**对外**地址与端口 + auth token |
| 3 | 部署包 → 新建客户端包 | 填设备名、机器人类型、目标平台、代理，生成 ZIP |

**第二步最容易漏，也最不容易被发现**：接入点是「frpc 实际要连的地址」，跟 Dashboard 地址（内网采集用）是两回事。NAT 环境下对外端口 ≠ frps 的 `bindPort`，而 Dashboard **不返回**这个地址，只能显式配置。

漏掉它的症状不是报错——是两端各用一个 token，**包能装上、但连不上**，而单包校验发现不了。所以 auth token 一律以接入点为唯一权威。

---

## 功能一览

| 页面 | 能力 |
|---|---|
| **总览** | FRP 版本、运行状态、客户端数 / 代理数、当前连接数与流量；含采集时间，API 故障时显示「状态未知」与上次成功时间 |
| **客户端** | 名称、主机名、IP、版本、在线状态、首次/最近连接时间；关键词与在线状态筛选；**详情抽屉**展示关联代理与本地端口 |
| **代理** | TCP/UDP/HTTP/HTTPS/STCP/XTCP 六类的状态、端点、流量、连接数；按类型/客户端/状态筛选 |
| **部署包** | 生成客户端包（Windows / Linux）、服务端包、平台自身包；预览冲突、下载、查签发记录 |
| **客户端台账** | 记录签发了谁、占了哪些远端端口、机器人类别、目标平台；可停用/删除以释放端口；**按类型分组并支持筛选**；每行有「生成包」改包入口 |
| **记录** | 谁在何时生成了哪类包与非敏感摘要；不记录明文密码/token，不能重复下载旧密钥包 |
| **设置** | FRP 节点配置与连通性测试（区分连接失败/超时/401/版本不兼容）、客户端接入点 |

状态采集默认 **10 秒**一次，连续失败不覆盖最后一次成功快照。

### 出包时会发生什么

1. **校验**：打包前调用对应 FRP 二进制的 `verify -c`，失败就禁止下载并显示具体错误（F15）
2. **撞端口检查**：同一远端端口被台账里另一条记录占着 → 返回 409，并点名占用方是谁（同 `clientID` 重新出包不算冲突）
3. **审计**：记录签发人、时间、包类型与非敏感摘要
4. **台账更新**：端口占用、显示名称、机器人类别、目标平台一并落库

### 包里有什么

```
frpc.exe / frps.exe      对应平台的 FRP 二进制
frpc.toml / frps.toml    渲染出的配置
install.ps1  uninstall.ps1  start.ps1  stop.ps1  restart.ps1  status.ps1
一键安装.bat              双击即装，自动提权（F21）
README.txt  SHA256SUMS.txt
```

- **自启动**：Windows 用计划任务（`SYSTEM` 身份、系统启动时触发，不登录桌面也能跑）；Linux 用 systemd
- **生命周期脚本**只操作本部署目录对应的实例，**不按进程名误杀**其他 FRP 实例
- **目标平台**：客户端包支持 `windows-amd64`（默认）、`linux-amd64`、`linux-arm64`；服务端包与平台自身包目前只有 Windows（依赖计划任务 + DPAPI + NTFS ACL）
- **文本规范按平台分**：Windows 脚本 `BOM + CRLF`；Linux 脚本**无 BOM + LF**（带 BOM → `No such file or directory`，CRLF → `bad interpreter`，两条报错都不指向真因）

### 改包（不是「编辑旧包」）

ZIP 从不落盘、服务端也不保留签发密钥，所以拿不回旧包。改包是**从台账记录重建出包表单**：台账页每行「生成包」→ 跳到部署包页并自动预填 → 改完生成**新**包，同一 `clientID`，目标机器覆盖安装即可。

表单里的值分两类，界面上也分开标识：

- **台账里有的**（clientID、显示名称、机器人类型、目标平台、每条代理的两端端口）→ 真实还原
- **台账里没有的**（连接 token、日志级别、HTTP 域名与路由、STCP/XTCP 访问密钥、每条代理的本地 IP）→ 由默认值补齐，**逐项列出**而不是笼统提示

连接 token **永远取当前接入点**，不试图还原旧包的。除预填之外，端口冲突、校验、审计、台账更新全照旧。

---

## 端口规划

| 端口 | 用途 | 监听 |
|---|---|---|
| 7000 | frps `bindPort` | 服务端 |
| 7500 | FRP Dashboard / API | `127.0.0.1` |
| **7800** | **SinyeFRP 管理平台 Web** | `127.0.0.1` |

> 生产 frps 主机通常带公网 IP。**管理平台 Web 端口不得绑定到公网网卡**——远程访问请走 SSH 隧道、内网通道，或借助既有 FRP 隧道。

---

## 目录结构

```
cmd\sinyefrp\main.go     程序入口（含 pkg app 子命令）
internal\
  ├─ frpapi\             Dashboard API 只读客户端与错误分类
  ├─ collector\          10 秒状态采集与快照
  ├─ store\              SQLite 连接、迁移、DAO（含客户端台账与端口占用）
  ├─ auth\               管理员认证、会话、登录限速
  ├─ secret\             Windows DPAPI 封装
  ├─ endpoint\           客户端接入点配置
  ├─ pack\               部署包生成器（spec / toml / scripts / verify / builder）
  │   └─ assets\         内嵌 FRP 二进制（构建期注入）
  └─ web\                HTTP 路由与 API 处理器；dist\ 为前端产物
web\                     前端源码（Vue 3 + Vite + Element Plus）
scripts\                 构建、自检、回归脚本
frp\                     FRP 二进制与本机构建来源（二进制不入库）
docs\v1.0\               文档体系
```

---

## 开发

```powershell
.\scripts\regression.ps1              # 全量回归：四套隔离实例 + 一个测试 frps，约 5.5 分钟
.\scripts\regression.ps1 -Only pkg    # 只跑单个套件
.\bin\sinyefrp.exe pkg app -o platform.zip   # 生成平台自身部署包
.\scripts\verify-pack-integrity.ps1 -Zip platform.zip   # 产物级校验
```

回归共 **六套件 542 项**：smoke 41 / pkg 145 / registry 174 / platform 102（需管理员）/ lifecycle 43（需管理员）/ 无头浏览器走查 37。

**改了平台自身代码或 `internal\pack\scripts.go` 之后，必须重出平台包并跑产物级校验**——只跑源码级回归会全绿，而交付物是旧的。

### 脚本

| 脚本 | 用途 |
|---|---|
| `fetch-frp.ps1` | 按版本拉 FRP 二进制（仓库不含二进制） |
| `build.ps1` | 前端构建 + 复制 assets + go build |
| `regression.ps1` | 全量回归驱动 |
| `smoke-test.ps1` / `pkg-test.ps1` / `registry-test.ps1` | 接口冒烟 / 出包端到端 / 台账与端口占用 |
| `platform-test.ps1` / `lifecycle-test.ps1` | 平台包真装真跑真卸 / 部署包安装卸载（均需管理员） |
| `verify-pack-integrity.ps1` | 包产物自检：PS 5.1 语法、行尾与 BOM、SHA256SUMS 自洽 |
| `ui-walkthrough.js` 等 | CDP 驱动无头 Edge 的真实渲染走查与 DOM 断言 |

### 几个踩过的坑

- **脚本编码**：`.ps1` 必须是 UTF-8 **with BOM** + CRLF；Linux 脚本必须无 BOM + LF。行尾由 `.gitattributes` 钉住，BOM 不受它管辖——新建 `.ps1` 要显式写入。
- **数字型可选字段一律声明为指针**：所用的 TOML 库 `omitempty` 对数字不生效，值类型会被照写成 `remotePort = 0`，导致整包被 FRP 拒绝。
- **FRP 不返回代理的本地端口**：只能按 `(clientID, 代理短名)` 从台账关联补齐。键写错的表现是**恒为 0 且 200、零报错**，所以统一显示「未登记」而不是 0。
- **走查环境没有在线 frpc** ⇒ 客户端表没有数据行 ⇒ 行内按钮与它打开的弹窗根本不渲染，走查 0 异常是**假绿**。这类改动要在有真实 frpc 的实例上「先打开再断言」。

---

## 文档

| 文档 | 内容 |
|---|---|
| `docs/v1.0/01-prd.md` | 需求、功能清单 F01–F27、部署包业务规则、验收主路径 |
| `docs/v1.0/02-技术选型.md` | 架构决策与理由、目录结构、端口规划 |
| `docs/v1.0/03-frp-api-contract.md` | FRP Dashboard API 实测契约（字段形状以它为准） |
| `docs/v1.0/00-opensource-research.md` | 选型前的开源方案调研 |

---

## 许可

FRP 沿用其自身的 Apache-2.0 许可，见 `frp\LICENSE`。管理平台本体许可未定。
