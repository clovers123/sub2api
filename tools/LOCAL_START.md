# 本地启动入口

本目录提供 macOS 和 Windows 的本地启动说明。

## 平台文档

| 平台 | 文档 |
| --- | --- |
| macOS | `tools/MACOS_LOCAL.md` |
| Windows | `tools/WINDOWS_LOCAL.md` |

## 快速命令

macOS：

```bash
make local-start
make local-stop
make local-status
make local-logs
```

Windows：

```cmd
tools\local-start.cmd
tools\local-stop.cmd
tools\local-status.cmd
tools\local-logs.cmd
```

## mihomo 接入思路

两边都采用同一个模式：

1. 先启动 sub2api 后端。
2. watcher 观察后端 pid 文件或监听端口。
3. 后端运行时启动 mihomo。
4. 后端停止时关闭 mihomo。

Windows 已内置 `tools\watch-sub2api-proxy-cores.ps1` 和 `tools\proxy-cores.windows.example.json`，复制成 `tools\proxy-cores.windows.json` 后即可接入。

macOS 的前后端启动仍由 `make local-start` 管理；mihomo watcher 建议作为本地脚本放在 `tmp/`，示例见 `tools/MACOS_LOCAL.md`。
