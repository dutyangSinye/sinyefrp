# SinyeFRP 开发环境变量
#
# Go 工具链安装在隔离目录中，不污染系统环境。
# 用法：在命令开头 dot-source 本文件
#     . D:\sinyeFrp\scripts\goenv.ps1
#
# 生产部署不需要 Go，交付物是单个 exe。

$env:GOROOT     = "C:\Users\Admin\.workbuddy\binaries\go\go1.27.1\go"
$env:GOPATH     = "C:\Users\Admin\.workbuddy\binaries\go\gopath"
$env:GOMODCACHE = "C:\Users\Admin\.workbuddy\binaries\go\gopath\pkg\mod"
$env:GOCACHE    = "C:\Users\Admin\.workbuddy\binaries\go\gocache"
$env:GOBIN      = "C:\Users\Admin\.workbuddy\binaries\go\gopath\bin"

# 国内模块代理；direct 兜底。
$env:GOPROXY = "https://goproxy.cn,direct"

# 禁止把 build 产物装进系统目录。
$env:GOFLAGS = "-mod=mod"

$env:PATH = "$env:GOROOT\bin;$env:GOBIN;$env:PATH"
