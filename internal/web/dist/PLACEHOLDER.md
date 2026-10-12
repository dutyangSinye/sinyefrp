# internal/web/dist 是构建产物目录

本目录是前端构建的输出目标，也是 `internal/web/embed.go` 中
`//go:embed all:dist` 的来源。

**仓库里只有这一个占位文件**，真实的 `index.html` 与 `assets/` 由前端构建生成，
已在 `.gitignore` 中排除 —— 它们是每次构建都会变的带哈希指纹产物，
入库只会让每次构建后 `git status` 都变脏。

## 为什么必须保留一个文件

`go:embed` 不支持空目录：目录为空时 `go build` 直接失败，且报错信息
不会指向「忘了构建前端」。留一个占位文件，代价是零，换来的是
「clone 下来立刻能 `go build`」—— 运行时才会给出明确的缺件提示
（`serveIndex` 返回 500「前端资源缺失，请先执行前端构建」）。

这个设计与 `internal/pack/assets/PLACEHOLDER.txt` 完全同源，改一处时另一处同理。

## 构建

完整构建走 `scripts\build.ps1`（前端构建 + 复制 FRP 二进制 + go build）。
只重建前端见 `web\package.json` 的 `build` 脚本。
