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

---

## ⚠️ Windows 常见问题与解决方案

### 问题1：502 Bad Gateway - 时区错误

**错误信息**:
```
Failed to initialize application: invalid timezone "Asia/Shanghai": unknown time zone Asia/Shanghai
```

**原因**: Windows 不支持 Linux 风格的时区名称 `Asia/Shanghai`

**解决方案**:

1. **设置环境变量**（推荐在启动脚本中设置）:
   ```powershell
   $env:TZ = "UTC"
   $env:TIMEZONE = "UTC"
   ```

2. **或在 `backend/config.yaml` 末尾添加**:
   ```yaml
   # Timezone Configuration
   timezone: "UTC"
   ```

3. **重新启动后端**:
   ```powershell
   tools\local-stop.cmd
   $env:TZ = "UTC"
   tools\local-start.cmd
   ```

---

### 问题2：502 Bad Gateway - TOTP 加密密钥为空

**错误信息**:
```
TOTP encryption key auto-generated. Consider setting a fixed key for production.
channel_monitor: decrypt api key failed - cipher: message authentication failed
```

**原因**: `backend/config.yaml` 中 `totp.encryption_key` 为空，每次启动都生成新密钥，导致已加密的密钥无法解密

**解决方案**:

1. **生成固定的加密密钥** (PowerShell):
   ```powershell
   $bytes = [byte[]]::new(32)
   $rng = [System.Security.Cryptography.RNGCryptoServiceProvider]::new()
   $rng.GetBytes($bytes)
   $encryptionKey = -join ($bytes | ForEach-Object { "{0:x2}" -f $_ })
   Write-Host "Generated key: $encryptionKey"
   ```

2. **添加到 `backend/config.yaml`**:
   ```yaml
   totp:
     encryption_key: "YOUR_GENERATED_KEY_HERE"
   ```

3. **重新启动后端**

---

### 问题3：502 Bad Gateway - 无有效上游账户

**错误信息**:
```
account_select_failed: context canceled
upstream_status: 403
Upstream request failed
```

**原因**: Sub2API 后端没有配置有效的 OpenAI API 密钥或其他上游账户

**解决方案**:

1. **启动前端和后端**:
   ```powershell
   $env:TZ = "UTC"
   tools\local-start.cmd
   ```

2. **访问管理界面**:
   - 浏览器打开: `http://localhost:3000` 或 `http://localhost:9005`

3. **登录管理员账户**:
   - 邮箱: `cloverszaq@gmail.com`
   - 密码: `cloverszaq@wuguohe`

4. **添加上游账户**:
   - 进入 Dashboard → Accounts/Channels 菜单
   - 点击 "Add New Account" 或 "Add Channel"
   - 选择 "OpenAI API Key" 类型
   - 输入您的有效 OpenAI API 密钥 (`sk-...`)
   - 保存并确认测试通过

5. **重新测试**:
   ```powershell
   # 用 PowerShell 测试 API
   $headers = @{
       "Authorization" = "Bearer sk-your-test-key"
       "Content-Type" = "application/json"
   }
   Invoke-RestMethod -Uri "http://localhost:9005/api/v1/auth/me" -Headers $headers
   ```

---

### 问题4：CORS 跨域问题

**错误信息**:
```
Warning: CORS allowed_origins not configured; cross-origin requests will be rejected.
```

**解决方案** (如果需要从其他域名访问):

在 `backend/config.yaml` 中配置:
```yaml
cors:
  allowed_origins:
    - "http://localhost:3000"
    - "http://localhost:9005"
    - "http://127.0.0.1"
    - "*"  # 开发环境可允许所有来源
  allow_credentials: true
```

---

### 快速启动（完整步骤）

```powershell
cd E:\sub2api\sub2api

# 1. 设置时区
$env:TZ = "UTC"
$env:TIMEZONE = "UTC"

# 2. 停止旧进程
.\tools\local-stop.cmd

# 3. 清空旧日志（可选）
Remove-Item .\tmp\*.log -Force -ErrorAction SilentlyContinue

# 4. 启动服务
.\tools\local-start.cmd

# 5. 验证后端响应
Start-Sleep -Seconds 5
Invoke-WebRequest -Uri "http://localhost:9005/api/v1/settings/public" -UseBasicParsing

# 6. 打开管理界面
Start-Process "http://localhost:3000"
```

---

### 查看日志

```powershell
# 实时查看后端日志
Get-Content .\tmp\sub2api-backend.err.log -Wait

# 查看最近的错误
Get-Content .\tmp\sub2api-backend.err.log -Tail 50

# 搜索特定错误
Select-String "error|failed|502|403" .\tmp\sub2api-backend.err.log
```

---

## 参考文档

- 详细故障排除: `tools/WINDOWS_LOCAL.md`
- 后端配置说明: `backend/config.yaml`
- API 使用示例: `frontend/src/views/auth/README.md`
