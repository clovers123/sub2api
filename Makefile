.PHONY: build build-backend build-frontend build-datamanagementd local-start local-stop local-status local-logs test test-backend test-frontend test-frontend-critical test-datamanagementd secret-scan

FRONTEND_CRITICAL_VITEST := \
	src/views/auth/__tests__/LinuxDoCallbackView.spec.ts \
	src/views/auth/__tests__/WechatCallbackView.spec.ts \
	src/views/user/__tests__/PaymentView.spec.ts \
	src/views/user/__tests__/PaymentResultView.spec.ts \
	src/components/user/profile/__tests__/ProfileInfoCard.spec.ts \
	src/views/admin/__tests__/SettingsView.spec.ts

BACKEND_PORT ?= 9004
FRONTEND_PORT ?= 9005

# 一键编译前后端
build: build-backend build-frontend

# 编译后端（复用 backend/Makefile）
build-backend:
	@$(MAKE) -C backend build

# 编译前端（需要已安装依赖）
build-frontend:
	@pnpm --dir frontend run build

# 编译 datamanagementd（宿主机数据管理进程）
build-datamanagementd:
	@cd datamanagement && go build -o datamanagementd ./cmd/datamanagementd

# 编译后端并启动本地前后端服务（后端 9004，前端 9005）
local-start:
	@./tools/local-start.sh

# 停止本地前后端服务
local-stop:
	@./tools/local-stop.sh

# 查看本地服务状态
local-status:
	@test -f tmp/sub2api-backend.pid && printf 'backend pid: ' && cat tmp/sub2api-backend.pid || true
	@test -f tmp/sub2api-frontend.pid && printf 'frontend pid: ' && cat tmp/sub2api-frontend.pid || true
	@lsof -nP -iTCP:$(BACKEND_PORT) -sTCP:LISTEN || true
	@lsof -nP -iTCP:$(FRONTEND_PORT) -sTCP:LISTEN || true

# 查看本地服务日志
local-logs:
	@tail -f tmp/sub2api-backend.log tmp/sub2api-frontend.log

# 运行测试（后端 + 前端）
test: test-backend test-frontend

test-backend:
	@$(MAKE) -C backend test

test-frontend:
	@pnpm --dir frontend run lint:check
	@pnpm --dir frontend run typecheck
	@$(MAKE) test-frontend-critical

test-frontend-critical:
	@pnpm --dir frontend exec vitest run $(FRONTEND_CRITICAL_VITEST)

test-datamanagementd:
	@cd datamanagement && go test ./...

secret-scan:
	@python3 tools/secret_scan.py
