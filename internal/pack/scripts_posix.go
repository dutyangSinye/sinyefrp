package pack

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"
)

// Linux 生命周期脚本与 systemd unit 模板。
//
// 与 scripts.go（Windows / PowerShell）是**并列的两套**，刻意不共用模板：
// 两者的运行机制毫无共同点（计划任务 vs systemd、PowerShell 5.1 的 BOM 要求
// vs shell 的 shebang 禁忌），强行抽成一套只会让模板里塞满分支，
// 而分支正是「改一处漏一处」的入口。
//
// 但**语义必须逐条对齐**，这些是运维在两类机器上期待一致的东西：
//
//  1. 只操作本部署目录的实例，绝不按进程名批量击杀（01-prd F14）。
//     Linux 侧靠 /proc/<pid>/exe 的符号链接精确比对可执行文件的绝对路径，
//     等价于 Windows 侧比对 Win32_Process.ExecutablePath。
//     **绝不能用 pkill -f frpc** —— 同一台机器上可能有别的 frp 实例，
//     按命令行片段匹配会直接打断别人的隧道。
//
//  2. 哈希校验两级不对称：主程序不符 = 致命（退出码 12），配置不符 = 只警告。
//     理由见 installBodyTpl 的同名注释：改配置是标准操作，判死会逼运维
//     去开 --skip-hash-check，而那会把主程序校验一起关掉。
//
//  3. 停止走 SIGTERM 并等待，超时才 SIGKILL 且必须告警。
//     强杀永远是最后手段，不能是默认手段。
//
//  4. 自启是「兜底重试，不是守护进程」：Windows 侧是 RestartCount=3/1 分钟，
//     Linux 侧必须显式 StartLimitBurst=3 —— systemd 默认是**无限次**重启，
//     不显式限流就会把一个起不来的服务反复拉起，日志刷屏并掩盖真实故障。
//
//  5. 退出码沿用同一套约定（0/1/2/10/11/12/13），便于监控跨系统用同一套判据。
//
// 脚本用 POSIX sh（#!/bin/sh）而不是 bash：企业环境里 Ubuntu/CentOS/Debian
// 都有 bash，但 Alpine 与部分最小化镜像没有；而本套脚本没用到任何 bash 专有语法
// （无数组、无 [[ ]]、无 ${var//a/b}），写成 POSIX 更耐部署。

// linuxHeaderTpl 是每个 Linux 脚本共用的文件头。
const linuxHeaderTpl = `#!/bin/sh
# SinyeFRP 生命周期脚本 — {{.Vars.Role}}（{{.Action}}）
# {{if .Vars.HasWebUI}}生成器: SinyeFRP 管理平台自身    版本: {{.Vars.Version}}{{else}}生成器: SinyeFRP 管理平台    目标 FRP 版本: {{.Vars.Version}}{{end}}
#
# 本脚本只操作位于其所在目录的实例，不会影响机器上其它已部署的实例。
# 退出码: 0 成功 / 1 一般失败 / 2 未安装 / 10 缺权限 / 11 缺文件 / 12 哈希不符 / 13 服务失败

set -eu
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
EXE="$ROOT/{{.Vars.ExeName}}"
CONF="$ROOT/{{.Vars.ConfName}}"
SERVICE="{{.Vars.TaskName}}"
UNIT_SRC="$ROOT/sinyefrp.service"
UNIT_DEST="/etc/systemd/system/$SERVICE.service"
`

// linuxCommonTpl 是共用的函数定义。
const linuxCommonTpl = `
# 输出错误并退出。与 Windows 版的 Fail 同源约定：所有失败路径都经这里，
# 保证退出码语义一致（监控跨系统用同一套判据）。
fail() {
    code=$1
    shift
    printf '[错误] %s\n' "$*"
    exit "$code"
}

# 要求以 root 运行。
#
# 不做自动 sudo：sudo 需要交互输入密码，脚本里悄悄调起只会让无人值守的
# 安装挂在一个看不见的提示上。明确报错并告诉运维该敲什么，代价最小。
require_root() {
    if [ "$(id -u)" != "0" ]; then
        fail 10 "需要 root 权限。请用 sudo 执行本脚本（例如: sudo ./$0）。"
    fi
}

# 确认 systemd 可用。
#
# 本包只支持 systemd（01-prd 的自启要求在这类系统上由它承担）。
# 检测到没有 systemctl 就直接说清楚，而不是留一个装完不启动的包 ——
# 后者会让运维以为是配置错了，往完全错误的方向查。
require_systemd() {
    if ! command -v systemctl >/dev/null 2>&1; then
        fail 13 "未找到 systemctl：本部署包只支持 systemd。请改用受支持的发行版，或手工启动 $EXE。"
    fi
}

# 列出本部署目录对应的进程 PID，每行一个。
#
# 判据是 /proc/<pid>/exe 这个符号链接指向的目标是否就是本目录的二进制，
# 等价于 Windows 侧比对 Win32_Process.ExecutablePath。
#
# 容忍路径后的 " (deleted)" 后缀：二进制被升级替换后，内核会给已删除文件
# 的链接加上这个标记。不认它的话，升级后会残留一个永远匹配不到的旧进程，
# 表现为「停止无效、安装后两个实例并存」。
#
# 这里刻意**不**用 pgrep -f / pkill -f：按命令行片段匹配会命中同一台机器上
# 其它 frp 部署（含别的项目遗留的实例），直接打断人家的隧道。
instance_pids() {
    for d in /proc/[0-9]*; do
        [ -d "$d" ] || continue
        pid=${d#/proc/}
        target=$(readlink "$d/exe" 2>/dev/null) || continue
        case "$target" in
            "$EXE"|"$EXE (deleted)") printf '%s\n' "$pid" ;;
        esac
    done
}

# 优雅停止本目录实例：先 SIGTERM，等它自己退出，超时才 SIGKILL。
#
# 顺序与 Windows 版一致，理由也一致（只是 Linux 上信号真的能送达，
# 不必退化到「写个文件让进程自己轮询」那种办法）：
# 强杀会让进程来不及收尾；只有超时仍未退出才降级，且必须让运维看见。
stop_instance() {
    pids=$(instance_pids)
    if [ -z "$pids" ]; then
        return 0
    fi

    for pid in $pids; do
        kill -TERM "$pid" 2>/dev/null || true
    done

    i=0
    while [ "$i" -lt 40 ]; do
        left=$(instance_pids)
        [ -n "$left" ] || break
        sleep 0.5
        i=$((i + 1))
    done

    left=$(instance_pids)
    if [ -n "$left" ]; then
        printf '[警告] 进程 %s 在 20 秒内未退出，已强制结束。\n' "$(echo $left | tr '\n' ' ')" >&2
        for pid in $left; do
            kill -KILL "$pid" 2>/dev/null || true
        done
        sleep 1
    fi
    return 0
}

# 哈希校验：主程序不符致命，配置不符只警告。
#
# 与 Windows 版同一套判据，理由见文件头第 2 条。
# 用 sha256sum（coreutils，各发行版都有）；读清单时按行解析，
# 不去适配 sha256sum -c —— 后者对文件名的编码与转义更挑剔。
verify_hash() {
    sum_file="$ROOT/SHA256SUMS.txt"
    if [ ! -f "$sum_file" ]; then
        printf '[{{.Vars.Steps.Hash}}/{{.Vars.Steps.Total}}] 未找到 SHA256SUMS.txt，已跳过哈希校验\n'
        return 0
    fi
    if [ "$SKIP_HASH" = "1" ]; then
        printf '[{{.Vars.Steps.Hash}}/{{.Vars.Steps.Total}}] 已按 --skip-hash-check 跳过哈希校验（主程序与配置都未校验）\n'
        return 0
    fi

    want_exe=""
    want_conf=""
    while IFS= read -r line; do
        [ -n "$line" ] || continue
        h=${line%%  *}
        n=${line##*  }
        case "$n" in
            "{{.Vars.ExeName}}") want_exe="$h" ;;
            "{{.Vars.ConfName}}") want_conf="$h" ;;
        esac
    done < "$sum_file"

    if [ -n "$want_exe" ]; then
        actual=$(sha256sum "$EXE" | cut -d' ' -f1)
        if [ "$actual" != "$want_exe" ]; then
            fail 12 "{{.Vars.ExeName}} 校验失败: 主程序与出厂版本不一致，包不可信。请重新解压一份完整的部署包，不要在这个目录里继续安装。"
        fi
    fi
    printf '[{{.Vars.Steps.Hash}}/{{.Vars.Steps.Total}}] 主程序哈希校验通过\n'

    if [ -n "$want_conf" ]; then
        actual=$(sha256sum "$CONF" | cut -d' ' -f1)
        if [ "$actual" != "$want_conf" ]; then
            printf '[提示] {{.Vars.ConfName}} 与出厂版本不一致（按目标环境改过配置属正常），不阻断安装。\n'
            printf '       若不是你改的，请先核对内容再决定是否继续。\n'
        fi
    fi
}
`

// linuxInstallTpl 实现安装。
const linuxInstallTpl = `
printf '=== SinyeFRP {{.Vars.Role}}安装 ===\n'
printf '部署目录: %s\n' "$ROOT"

SKIP_HASH=0
for arg in "$@"; do
    case "$arg" in
        --skip-hash-check) SKIP_HASH=1 ;;
        *) fail 1 "未知参数: $arg" ;;
    esac
done

require_root
require_systemd

for f in "$EXE" "$CONF" "$UNIT_SRC"; do
    if [ ! -f "$f" ]; then
        fail 11 "缺少必需文件: $f"
    fi
done

verify_hash

# 注册 systemd 服务。
#
# unit 文件在包里是模板（路径用 @ROOT@ 占位），安装时才代入真实部署目录 ——
# systemd 的 ExecStart 与 WorkingDirectory 都必须是绝对路径，而包生成时
# 无从知道运维会解压到哪。模板仍是唯一来源，install.sh 只做代入，
# 不在脚本里另写一份 unit —— 那种「两处各写一半」正是最容易改漏的形状。
#
# 替换值里的 & 和分隔符要转义：sed 的替换串中 & 代表整个匹配，
# 不转义的话部署目录里带 & 就会被展开成错误内容，且不报错。
escaped_root=$(printf '%s' "$ROOT" | sed 's/[\\&|]/\\&/g')
sed "s|@ROOT@|$escaped_root|g" "$UNIT_SRC" > "$UNIT_DEST"
chmod 0644 "$UNIT_DEST"
systemctl daemon-reload
systemctl enable "$SERVICE" >/dev/null 2>&1 || fail 13 "设置开机自启失败: systemctl enable $SERVICE"
printf '[{{.Vars.Steps.Task}}/{{.Vars.Steps.Total}}] {{.Vars.ServiceKind}}已创建或更新: %s\n' "$SERVICE"

# 若已有旧进程在跑，先停掉，避免同目录出现两个实例。
running=$(instance_pids)
if [ -n "$running" ]; then
    stop_instance
    printf '[{{.Vars.Steps.Stop}}/{{.Vars.Steps.Total}}] 已停止旧进程\n'
else
    printf '[{{.Vars.Steps.Stop}}/{{.Vars.Steps.Total}}] 无旧进程需要停止\n'
fi

systemctl restart "$SERVICE" >/dev/null 2>&1 || fail 13 "启动服务失败: systemctl restart $SERVICE"
sleep 3
printf '[{{.Vars.Steps.Start}}/{{.Vars.Steps.Total}}] 服务已启动\n'

now=$(instance_pids)
if [ -n "$now" ]; then
    printf '\n安装完成，进程 PID: %s\n' "$(echo $now | tr '\n' ' ')"
    printf '配置文件: %s\n' "$CONF"
    printf '查看运行状态请执行 sudo ./status.sh\n'
    exit 0
fi

printf '\n服务已注册并启动，但进程尚未出现。请稍候用 sudo ./status.sh 复查；\n' >&2
printf '若持续未启动，请检查同目录下的日志文件，或执行 journalctl -u %s。\n' "$SERVICE" >&2
exit 0
`

// linuxUninstallTpl 实现卸载。
const linuxUninstallTpl = `
printf '=== SinyeFRP {{.Vars.Role}}卸载 ===\n'

REMOVE_DATA=0
for arg in "$@"; do
    case "$arg" in
        --remove-data) REMOVE_DATA=1 ;;
        *) fail 1 "未知参数: $arg" ;;
    esac
done

require_root
require_systemd

# 0) 先停进程，再撤服务 —— 顺序不能反。
#
# 与 Windows 版同一个教训（那边是「卸载时删掉了仍在运行的 data\」）：
# 服务一旦被撤掉，「进程还在跑」这个事实就再也拦不住了。
# 于是第 3 步的「仍在运行」检查看到的永远是空列表，删数据「成功」——
# 而 Linux 上删除被打开的文件同样会成功（目录项摘掉、进程照常读写），
# 症状就是「卸载报成功、数据其实没了」。所以有活进程就必须先收干净。
running=$(instance_pids)
if [ -n "$running" ]; then
    systemctl stop "$SERVICE" >/dev/null 2>&1 || true
    stop_instance
    left=$(instance_pids)
    if [ -n "$left" ]; then
        if [ "$REMOVE_DATA" = "1" ]; then
            fail 1 "检测到进程 $left 仍在运行，已中止删除数据。请先执行 sudo ./stop.sh，确认进程全部退出后再重试；否则会在文件仍被占用时删除数据，导致静默丢失。"
        fi
        fail 1 "仍有进程 $left 未能停止，请手动检查。"
    fi
    printf '[1/3] 已停止 %s 个进程\n' "$(echo $running | wc -w | tr -d ' ')"
else
    printf '[1/3] 无运行中的进程\n'
fi

# 1) 撤销开机自启并删除 unit。
if systemctl is-enabled "$SERVICE" >/dev/null 2>&1; then
    systemctl disable "$SERVICE" >/dev/null 2>&1 || true
fi
if [ -f "$UNIT_DEST" ]; then
    rm -f "$UNIT_DEST" || fail 13 "删除服务文件失败: $UNIT_DEST"
    systemctl daemon-reload
    printf '[2/3] {{.Vars.ServiceKind}}已删除: %s\n' "$SERVICE"
else
    printf '[2/3] {{.Vars.ServiceKind}}不存在，跳过\n'
fi

# 2) 数据保留策略：默认只移除自启动，保留配置与日志。
if [ "$REMOVE_DATA" = "1" ]; then
    for name in "{{.Vars.ConfName}}"{{range .Vars.DataPaths}} "{{.}}"{{end}}; do
        p="$ROOT/$name"
        if [ -e "$p" ]; then
            rm -rf "$p"
        fi
    done
    printf '[3/3] 已按 --remove-data 删除配置与日志\n'
else
    printf '[3/3] 已保留配置与日志；如需一并删除请加 --remove-data\n'
fi

printf '\n卸载完成。\n'
exit 0
`

// linuxStartTpl 启动实例。
const linuxStartTpl = `
printf '=== SinyeFRP {{.Vars.Role}}启动 ===\n'

require_root
require_systemd

running=$(instance_pids)
if [ -n "$running" ]; then
    printf '实例已在运行，PID: %s\n' "$(echo $running | tr '\n' ' ')"
    exit 0
fi

if [ ! -f "$UNIT_DEST" ]; then
    fail 2 "尚未安装，请先执行 sudo ./install.sh"
fi

systemctl start "$SERVICE" >/dev/null 2>&1 || fail 13 "启动服务失败: systemctl start $SERVICE"
sleep 3

now=$(instance_pids)
if [ -z "$now" ]; then
    fail 1 "服务已下发，但进程未出现。请检查同目录日志，或执行 journalctl -u $SERVICE。"
fi
printf '启动成功，PID: %s\n' "$(echo $now | tr '\n' ' ')"
exit 0
`

// linuxStopTpl 停止实例。
const linuxStopTpl = `
printf '=== SinyeFRP {{.Vars.Role}}停止 ===\n'

require_root
require_systemd

# 先让服务不再拉起，否则它会按 Restart 策略把刚停掉的进程再开起来。
systemctl stop "$SERVICE" >/dev/null 2>&1 || true

running=$(instance_pids)
if [ -z "$running" ]; then
    printf '实例未在运行\n'
    exit 0
fi

stop_instance

left=$(instance_pids)
if [ -n "$left" ]; then
    fail 1 "仍有进程 $left 未能停止，请手动检查。"
fi
printf '已停止 %s 个进程\n' "$(echo $running | wc -w | tr -d ' ')"
exit 0
`

// linuxRestartTpl 重启实例。
const linuxRestartTpl = `
printf '=== SinyeFRP {{.Vars.Role}}重启 ===\n'

require_root
require_systemd

if [ ! -f "$UNIT_DEST" ]; then
    fail 2 "尚未安装，请先执行 sudo ./install.sh"
fi

systemctl stop "$SERVICE" >/dev/null 2>&1 || true
running=$(instance_pids)
if [ -n "$running" ]; then
    stop_instance
fi

systemctl start "$SERVICE" >/dev/null 2>&1 || fail 13 "启动服务失败: systemctl restart $SERVICE"
sleep 3

now=$(instance_pids)
if [ -z "$now" ]; then
    fail 1 "重启后进程未出现。请检查同目录日志，或执行 journalctl -u $SERVICE。"
fi
printf '重启完成，PID: %s\n' "$(echo $now | tr '\n' ' ')"
exit 0
`

// linuxStatusTpl 查询状态。
//
// 与 Windows 版同样保持「人可读 + 机器可解析」两用：默认键值对，--json 单行 JSON。
const linuxStatusTpl = `
JSON=0
for arg in "$@"; do
    case "$arg" in
        --json) JSON=1 ;;
        *) fail 1 "未知参数: $arg" ;;
    esac
done

installed=false
if command -v systemctl >/dev/null 2>&1 && [ -f "$UNIT_DEST" ]; then
    installed=true
fi
if [ "$installed" = "true" ] && command -v systemctl >/dev/null 2>&1; then
    if systemctl is-enabled "$SERVICE" >/dev/null 2>&1; then
        autostart="enabled"
    else
        autostart="disabled"
    fi
else
    autostart="none"
fi

pids=$(instance_pids)
count=$(printf '%s' "$pids" | grep -c . || true)
if [ "$count" = "0" ]; then
    running=false
else
    running=true
fi

# JSON 串里的路径要做最小转义：反斜杠与双引号会破坏 JSON 结构，
# 而部署目录由运维决定，含这些字符是可能的。
json_escape() {
    printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g'
}

if [ "$JSON" = "1" ]; then
    printf '{"role":"{{.Vars.Role}}","displayName":"%s","serviceName":"%s","installed":%s,"autostart":"%s","running":%s,"processCount":%s,"pids":"%s","root":"%s","frpVersion":"{{.Vars.Version}}"}\n' \
        "$(json_escape '{{.Vars.DisplayName}}')" "$SERVICE" "$installed" "$autostart" "$running" "$count" \
        "$(echo $pids | tr '\n' ' ' | sed 's/ *$//')" "$(json_escape "$ROOT")"
else
    printf '=== SinyeFRP {{.Vars.Role}}状态 ===\n'
    printf '名称     : {{.Vars.DisplayName}}\n'
    printf '部署目录 : %s\n' "$ROOT"
    printf 'FRP 版本 : {{.Vars.Version}}\n'
    printf '{{.Vars.ServiceKind}} : %s\n' "$SERVICE"
    printf '开机自启 : %s\n' "$autostart"
    if [ "$running" = "true" ]; then
        printf '运行状态 : 运行中 (PID %s)\n' "$(echo $pids | tr '\n' ' ' | sed 's/ *$//')"
    else
        printf '运行状态 : 未运行\n'
    fi
fi

if [ "$installed" != "true" ]; then
    exit 2
fi
if [ "$running" = "true" ]; then
    exit 0
fi
exit 1
`

// linuxUnitTpl 是包内的 systemd unit 模板。
//
// 部署目录用 @ROOT@ 占位，install.sh 安装时代入 —— systemd 要求绝对路径，
// 而包生成时无从知道解压位置。
//
// StartLimitBurst=3 是刻意的，见文件头第 4 条：systemd 默认无限重启，
// 那会把「起不来的服务」变成刷屏的重启循环，与 Windows 侧
// 「RestartCount=3、兜底不是守护」的承诺不一致，也会掩盖真实故障。
const linuxUnitTpl = `[Unit]
Description=SinyeFRP {{.Vars.Role}} ({{.Vars.DisplayName}})
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=@ROOT@
ExecStart=@ROOT@/{{.Vars.ExeName}} {{.Vars.ConfFlag}} @ROOT@/{{.Vars.ConfName}}
Restart=on-failure
RestartSec=10
StartLimitBurst=3
StartLimitIntervalSec=600

[Install]
WantedBy=multi-user.target
`

// linuxScriptDef 描述一个待渲染的 Linux 脚本。
type linuxScriptDef struct {
	File   string
	Action string
	Body   string
}

var linuxScriptDefs = []linuxScriptDef{
	{File: "install.sh", Action: "安装并注册开机自启", Body: linuxInstallTpl},
	{File: "uninstall.sh", Action: "卸载并移除开机自启", Body: linuxUninstallTpl},
	{File: "start.sh", Action: "启动", Body: linuxStartTpl},
	{File: "stop.sh", Action: "停止", Body: linuxStopTpl},
	{File: "restart.sh", Action: "重启", Body: linuxRestartTpl},
	{File: "status.sh", Action: "状态查询", Body: linuxStatusTpl},
}

// RenderLinuxScripts 生成 Linux 生命周期脚本与 systemd unit 模板。
//
// 返回内容一律 LF、无 BOM —— 行尾规范化统一由 assemble 的 normalizeText 做
// （那是默认/预览/下载三条出站路径的唯一汇聚点），这里不重复处理，
// 免得出现「两处都管、结果互相覆盖」的情形。
//
// 目前只支持 frp 客户端/服务端包；管理平台自身包（HasWebUI）尚无 Linux 版，
// 走到这里会明确报错，而不是产出一个缺了数据目录与端口探活的半成品。
func RenderLinuxScripts(v scriptVars) (map[string][]byte, error) {
	if v.HasWebUI {
		return nil, fmt.Errorf("管理平台的 Linux 部署包暂不支持")
	}
	v.Steps = computeSteps(v.HasWebUI)

	out := make(map[string][]byte, len(linuxScriptDefs)+1)
	for _, d := range linuxScriptDefs {
		var buf bytes.Buffer
		t, err := template.New(d.File).Parse(linuxHeaderTpl + linuxCommonTpl + d.Body)
		if err != nil {
			return nil, fmt.Errorf("解析 %s 模板失败: %w", d.File, err)
		}
		if err := t.Execute(&buf, tmplData{Vars: v, Action: d.Action}); err != nil {
			return nil, fmt.Errorf("渲染 %s 失败: %w", d.File, err)
		}
		out[d.File] = withLF(buf.Bytes())
	}

	var buf bytes.Buffer
	t, err := template.New(unitFileName).Parse(linuxUnitTpl)
	if err != nil {
		return nil, fmt.Errorf("解析 %s 模板失败: %w", unitFileName, err)
	}
	if err := t.Execute(&buf, tmplData{Vars: v, Action: "服务定义"}); err != nil {
		return nil, fmt.Errorf("渲染 %s 失败: %w", unitFileName, err)
	}
	out[unitFileName] = withLF(buf.Bytes())
	return out, nil
}

// LinuxScriptExitCodes 是 Linux 脚本的退出码说明。
//
// 数值与 ScriptExitCodes **完全一致**（0/1/2/10/11/12/13），这是刻意的：
// 监控系统对 Windows 与 Linux 两类机器应当用同一套判据，
// 数值一分叉，每个监控项都要写两套阈值、两套告警规则。
//
// 只有文案不同：Linux 上没有「计划任务」，权限也不叫「管理员」，
// 照抄 Windows 那份会让运维对着 Linux 机器去找一个不存在的东西。
var LinuxScriptExitCodes = []struct {
	Code int
	Mean string
}{
	{0, "成功；对 status.sh 而言表示实例正在运行"},
	{1, "一般性失败；对 status.sh 而言表示实例未运行"},
	{2, "尚未安装（仅 status.sh / start.sh）"},
	{10, "缺少 root 权限"},
	{11, "部署目录缺少必需文件（二进制或配置）"},
	{12, "文件哈希校验失败，包可能已损坏或被篡改"},
	{13, "systemd 服务创建、更新或删除失败"},
}

// withLF 把行尾统一为 LF。
//
// 先归一成 LF 再输出，而不是直接把 \r\n 替换掉：后者对已混合的行尾
// 会留下孤立的 \r，而 shell 会把行尾的 CR 当成命令的一部分
// （表现为 `bad interpreter` 或 `: command not found`），且极难看出来。
func withLF(b []byte) []byte {
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return []byte(s)
}
