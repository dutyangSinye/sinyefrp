# internal/ipgeo/data 是 IP 归属地数据目录

本目录存放 ip2region 的 xdb 数据文件，也是 `ipgeo.go` 中
`//go:embed data` 的来源。**仓库里只有这一个占位文件**，
真实的 `ip2region_v4.xdb`（约 11 MB）由 `scripts\fetch-frp.ps1` 下载，
已在 `.gitignore` 中排除。

## 为什么不入库

与 FRP 二进制同理：它是第三方的**数据文件**，上游每天更新修正，
入库的代价是每次更新都在 git 历史里永久压一份 11 MB。
拉取脚本与 FRP 二进制共用同一个入口（`fetch-frp.ps1`），clone 之后
跑一次即可，构建期自动内嵌进 exe。

## 没有这个文件会怎样

`go build` **照常通过**（本占位文件保证了 `//go:embed data` 不为空），
运行时所有归属地查询返回空串，前端显示「—」，其余功能不受影响。
归地属是展示列，任何故障都不允许放大成页面报错。

## 补充 IPv6 支持

当前只内嵌了 IPv4 数据（frpc 场景全是 v4）。需要 v6 时：

1. `fetch-frp.ps1` 增加 `ip2region_v6.xdb` 的下载；
2. `ipgeo.go` 放开 `Search` 里对 IPv6 的留白，按地址族选择对应 Searcher。

这个设计与 `internal/pack/assets/PLACEHOLDER.txt` 完全同源。
