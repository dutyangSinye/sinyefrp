// Package buildinfo 保存构建期固定的产品标识。
//
// 独立成包而不是让 cmd 与 internal/web 各持一份常量：产品名与版本号会同时
// 出现在三处 —— 启动日志、`-version` 输出、以及「平台自身部署包」的文件名与
// 包内 README。三处对不上时，运维会拿到一个自称版本互相矛盾的交付件，
// 而这恰恰是在排障最需要版本信息的时候。
package buildinfo

// Name 是产品名，用于日志字段、日志文件名与部署包命名。
const Name = "SinyeFRP"

// Version 是管理平台版本号。
//
// 目前仍是开发期版本号，尚未接入构建期注入（-ldflags -X）。
// 等发布流程确定版本号规则后再统一处理，避免现在就把假想的值写进构建脚本。
const Version = "0.1.0-dev"
