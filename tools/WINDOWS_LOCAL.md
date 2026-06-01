# Windows 本地启动

## 一次性准备

1. 安装 Go、Node.js、pnpm、PostgreSQL、Redis。
2. 在仓库根目录准备 `config.yaml`，或放到 `backend\config.yaml`。
3. 安装前端依赖：

```powershell
pnpm --dir frontend install --frozen-lockfile
```

## 启动 / 停止 / 状态 / 日志

```powershell
powershell -ExecutionPolicy Bypass -File .\tools\local-start.ps1
powershell -ExecutionPolicy Bypass -File .\tools\local-stop.ps1
powershell -ExecutionPolicy Bypass -File .\tools\local-status.ps1
powershell -ExecutionPolicy Bypass -File .\tools\local-logs.ps1
```

也可以直接使用 CMD 包装器：

```cmd
tools\local-start.cmd
tools\local-stop.cmd
tools\local-status.cmd
tools\local-logs.cmd
```

默认端口：

- 后端：http://localhost:9004
- 前端：http://localhost:9005

也可以临时换端口：

```powershell
$env:BACKEND_PORT = "9104"
$env:FRONTEND_PORT = "9105"
powershell -ExecutionPolicy Bypass -File .\tools\local-start.ps1
```

## 多代理核心联动

Windows 原生版本使用 `tools\watch-sub2api-proxy-cores.ps1` 管理多个代理核心。复制示例配置后改成本机路径：

```powershell
Copy-Item .\tools\proxy-cores.windows.example.json .\tools\proxy-cores.windows.json
notepad .\tools\proxy-cores.windows.json
```

只要 `tools\proxy-cores.windows.json` 存在，`local-start.ps1` 会在后端启动后自动启动 watcher。watcher 会在 sub2api 运行时启动所有 `enabled=true` 的核心，在 sub2api 停止后关闭它们。

单独调试代理核心：

```powershell
powershell -ExecutionPolicy Bypass -File .\tools\watch-sub2api-proxy-cores.ps1 -Config .\tools\proxy-cores.windows.json -Status
powershell -ExecutionPolicy Bypass -File .\tools\watch-sub2api-proxy-cores.ps1 -Config .\tools\proxy-cores.windows.json -Stop
```

不想启动代理核心时：

```powershell
powershell -ExecutionPolicy Bypass -File .\tools\local-start.ps1 -NoProxyCores
```
