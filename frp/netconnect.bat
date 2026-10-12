@echo off
:: 结束残留进程
taskkill /f /im frpc.exe >nul 2>nul
:: 启动frpc
frpc.exe -c frpc.toml