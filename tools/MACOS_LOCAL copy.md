# macOS 本地启动与 mihomo 联动

## 一次性准备

1. 安装 Go、Node.js、pnpm、PostgreSQL、Redis、`lsof`。
2. 在仓库根目录准备 `config.yaml`，或放到 `backend/config.yaml`。
3. 安装前端依赖。

```bash
pnpm --dir frontend install --frozen-lockfile
```

## 一键启动

在仓库根目录执行：

```bash
make local-start
```

这个命令会：

1. 编译后端到 `backend/bin/server`。
2. 启动后端：http://localhost:9004
3. 启动前端：http://localhost:9005
4. 设置前端开发代理到后端。
5. 持续占用当前终端；按 `Ctrl-C` 会停止前后端。

## 常用命令

| 命令 | 作用 |
| --- | --- |
| `make local-start` | 编译后端并启动前后端 |
| `make local-stop` | 停止本地前后端 |
| `make local-status` | 查看 pid 和端口监听 |
| `make local-logs` | 持续查看前后端日志 |
| `pnpm --dir frontend install --frozen-lockfile` | 依赖变化后重新安装前端依赖 |

也可以临时换端口：

```bash
BACKEND_PORT=9104 FRONTEND_PORT=9105 make local-start
```

## 日志位置

| 文件 | 内容 |
| --- | --- |
| `tmp/sub2api-backend.log` | 后端日志 |
| `tmp/sub2api-frontend.log` | 前端日志 |

## 接入 mihomo

macOS 现有 `make local-start` 只负责前后端，不会直接管理 mihomo。推荐使用一个本地 watcher 脚本实现和 Windows 相同的模式：

1. watcher 检测 sub2api 后端 pid 文件或端口。
2. sub2api 运行时启动 mihomo。
3. sub2api 停止后关闭 mihomo。

示例目录：

```text
~/.local/bin/mihomo
~/.config/mihomo-primary/config.yaml
~/.config/mihomo-primary/logs/
```

创建本地 watcher：

```bash
mkdir -p ~/.config/mihomo-primary/logs
cat > tmp/watch-sub2api-mihomo.sh <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

MIHOMO="${MIHOMO:-$HOME/.local/bin/mihomo}"
CONFIG_DIR="${MIHOMO_CONFIG_DIR:-$HOME/.config/mihomo-primary}"
CONFIG_FILE="${MIHOMO_CONFIG_FILE:-$CONFIG_DIR/config.yaml}"
PID_FILE="${MIHOMO_PID_FILE:-$CONFIG_DIR/mihomo.pid}"
LOG_DIR="$CONFIG_DIR/logs"
STDOUT_LOG="$LOG_DIR/stdout.log"
STDERR_LOG="$LOG_DIR/stderr.log"
SUB2API_ROOT="${SUB2API_ROOT:-$(pwd)}"
SUB2API_PID_FILE="$SUB2API_ROOT/tmp/sub2api-backend.pid"
SUB2API_PORT="${SUB2API_PORT:-9004}"

mkdir -p "$LOG_DIR"

sub2api_running() {
  if [[ -f "$SUB2API_PID_FILE" ]]; then
    local pid
    pid="$(cat "$SUB2API_PID_FILE" 2>/dev/null || true)"
    [[ -n "$pid" ]] && kill -0 "$pid" >/dev/null 2>&1 && return 0
  fi
  lsof -nP -iTCP:"$SUB2API_PORT" -sTCP:LISTEN >/dev/null 2>&1
}

mihomo_running() {
  if [[ -f "$PID_FILE" ]]; then
    local pid
    pid="$(cat "$PID_FILE" 2>/dev/null || true)"
    [[ -n "$pid" ]] && kill -0 "$pid" >/dev/null 2>&1 && return 0
  fi
  return 1
}

start_mihomo() {
  mihomo_running && return 0
  "$MIHOMO" -d "$CONFIG_DIR" -f "$CONFIG_FILE" >> "$STDOUT_LOG" 2>> "$STDERR_LOG" &
  echo $! > "$PID_FILE"
}

stop_mihomo() {
  if [[ -f "$PID_FILE" ]]; then
    local pid
    pid="$(cat "$PID_FILE" 2>/dev/null || true)"
    [[ -n "$pid" ]] && kill "$pid" >/dev/null 2>&1 || true
    rm -f "$PID_FILE"
  fi
}

cleanup() {
  stop_mihomo
  exit 0
}
trap cleanup INT TERM EXIT

while true; do
  if sub2api_running; then
    start_mihomo
  else
    stop_mihomo
  fi
  sleep 2
done
EOF
chmod +x tmp/watch-sub2api-mihomo.sh
```

使用方式：

```bash
# 终端 1
make local-start

# 终端 2
tmp/watch-sub2api-mihomo.sh
```

如果你的 mihomo 或配置不在推荐路径，可以用环境变量覆盖：

```bash
MIHOMO=/opt/homebrew/bin/mihomo \
MIHOMO_CONFIG_DIR="$HOME/.config/mihomo-work" \
MIHOMO_CONFIG_FILE="$HOME/.config/mihomo-work/config.yaml" \
tmp/watch-sub2api-mihomo.sh
```

## macOS 与 Windows 的区别

| 项目 | macOS | Windows |
| --- | --- | --- |
| 前后端一键启动 | `make local-start` | `tools\local-start.cmd` |
| 停止 | `make local-stop` 或 `Ctrl-C` | `tools\local-stop.cmd` 或 `Ctrl-C` |
| 状态 | `make local-status` | `tools\local-status.cmd` |
| 日志 | `make local-logs` | `tools\local-logs.cmd` |
| mihomo 联动 | 本地 watcher 脚本 | `tools\proxy-cores.windows.json` + 自动 watcher |
| 私有代理配置 | 不放进仓库 | `tools\proxy-cores.windows.json` 已忽略 |
