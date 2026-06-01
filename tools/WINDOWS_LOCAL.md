# Windows 本地启动与 mihomo 联动

## 一次性准备

1. 安装 Go、Node.js、pnpm、PostgreSQL、Redis。
2. 在仓库根目录准备 `config.yaml`，或放到 `backend\config.yaml`。
3. 安装前端依赖。
4. 如需联动 mihomo，准备 `mihomo.exe` 和一份可用的 mihomo 配置文件。

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

## 命令说明

| 命令 | 作用 |
| --- | --- |
| `tools\local-start.cmd` | 编译后端为 `backend\bin\server.exe`，启动后端、前端；如果存在代理核心配置，也会启动 watcher |
| `tools\local-stop.cmd` | 停止前端、后端、代理核心 watcher，并清理监听端口 |
| `tools\local-status.cmd` | 查看前端、后端、代理核心 watcher 的 pid 和端口监听情况 |
| `tools\local-logs.cmd` | 持续查看后端、前端、代理核心 watcher 日志 |

## 接入 mihomo

Windows 原生版本使用 `tools\watch-sub2api-proxy-cores.ps1` 管理一个或多个代理核心。工作模式是：

1. `local-start.ps1` 先启动 sub2api 后端。
2. 如果存在 `tools\proxy-cores.windows.json`，脚本会自动启动代理核心 watcher。
3. watcher 检测到 sub2api 后端运行时，启动所有 `enabled=true` 的核心。
4. sub2api 停止后，watcher 自动关闭这些核心。

先复制示例配置：

```powershell
Copy-Item .\tools\proxy-cores.windows.example.json .\tools\proxy-cores.windows.json
notepad .\tools\proxy-cores.windows.json
```

推荐目录结构：

```text
C:\Users\<你>\.local\bin\mihomo.exe
C:\Users\<你>\.config\mihomo-primary\config.yaml
```

对应配置：

```json
{
  "watch": {
    "backendPidFile": "tmp\\sub2api-backend.pid",
    "backendPort": 9004,
    "intervalSeconds": 2
  },
  "cores": [
    {
      "name": "mihomo-primary",
      "enabled": true,
      "exe": "%USERPROFILE%\\.local\\bin\\mihomo.exe",
      "args": [
        "-d",
        "%USERPROFILE%\\.config\\mihomo-primary",
        "-f",
        "%USERPROFILE%\\.config\\mihomo-primary\\config.yaml"
      ],
      "workingDirectory": "%USERPROFILE%\\.config\\mihomo-primary",
      "pidFile": "tmp\\proxy-cores\\mihomo-primary.pid",
      "stdoutLog": "tmp\\proxy-cores\\mihomo-primary.stdout.log",
      "stderrLog": "tmp\\proxy-cores\\mihomo-primary.stderr.log"
    }
  ]
}
```

如果你的 `mihomo.exe` 在别的位置，只需要改 `exe`。如果配置文件在别的位置，改 `-d` 后面的目录和 `-f` 后面的文件。

`tools\proxy-cores.windows.json` 已经被 `.gitignore` 忽略，可以放心写本机路径，但不要把真实节点配置文件放进仓库。

## 多核心模式

同一个配置文件里可以写多个核心。比如一个常用核心、一个备用核心：

```json
{
  "cores": [
    {
      "name": "mihomo-primary",
      "enabled": true,
      "exe": "%USERPROFILE%\\.local\\bin\\mihomo.exe",
      "args": ["-d", "%USERPROFILE%\\.config\\mihomo-primary", "-f", "%USERPROFILE%\\.config\\mihomo-primary\\config.yaml"]
    },
    {
      "name": "mihomo-backup",
      "enabled": false,
      "exe": "C:\\tools\\mihomo\\mihomo.exe",
      "args": ["-d", "C:\\tools\\mihomo\\backup", "-f", "C:\\tools\\mihomo\\backup\\config.yaml"]
    }
  ]
}
```

把备用核心的 `enabled` 改成 `true` 后，下次启动就会一起拉起。

## 单独调试代理核心

```powershell
powershell -ExecutionPolicy Bypass -File .\tools\watch-sub2api-proxy-cores.ps1 -Config .\tools\proxy-cores.windows.json -Status
powershell -ExecutionPolicy Bypass -File .\tools\watch-sub2api-proxy-cores.ps1 -Config .\tools\proxy-cores.windows.json -Stop
```

不想启动代理核心时：

```powershell
powershell -ExecutionPolicy Bypass -File .\tools\local-start.ps1 -NoProxyCores
```

## 日志位置

| 文件 | 内容 |
| --- | --- |
| `tmp\sub2api-backend.log` | 后端标准输出 |
| `tmp\sub2api-backend.err.log` | 后端错误输出 |
| `tmp\sub2api-frontend.log` | 前端标准输出 |
| `tmp\sub2api-frontend.err.log` | 前端错误输出 |
| `tmp\sub2api-proxy-cores-watcher.log` | 代理核心 watcher 输出 |
| `tmp\proxy-cores\*.stdout.log` | 代理核心标准输出 |
| `tmp\proxy-cores\*.stderr.log` | 代理核心错误输出 |
