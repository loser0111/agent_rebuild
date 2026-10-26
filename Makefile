# agent_rebuild —— 构建与开发入口
#
# 常用：
#   make dev    同时启动后端(:8080)与前端(:5173)，Ctrl-C 一起退出
#   make run    只跑后端（Mock 模式，零配置）
#   make build  构建后端二进制 + 前端静态产物
#   make test   go test -race ./...
#   make smoke  起后端并冒烟验证 SSE 全链路

SHELL := /bin/bash
GO        ?= go
NPM       ?= npm
BIN_DIR   := bin
BIN       := $(BIN_DIR)/agent-server
WEB_DIR   := web
WEB_DIST  := $(WEB_DIR)/dist
PORT      ?= 8080

.DEFAULT_GOAL := help

## help: 显示可用目标
.PHONY: help
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /' | column -t -s ':'

## tidy: 整理 Go 依赖
.PHONY: tidy
tidy:
	$(GO) mod tidy

## build: 构建后端二进制与前端静态产物
.PHONY: build
build: build-server build-web

## build-server: 只构建后端二进制到 bin/agent-server
.PHONY: build-server
build-server:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w -X agent_rebuild/internal/api.Version=0.1.0" -o $(BIN) ./cmd/server
	@echo "✅ 后端已构建: $(BIN)"

## build-binary: 构建「包含前端产物」的单一可执行文件（可拷到任意目录直接跑）
.PHONY: build-binary
build-binary: build-web
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w -X agent_rebuild/internal/api.Version=0.1.0 -X main.defaultWebDir=$(WEB_DIST)" -o $(BIN) ./cmd/server
	@echo "✅ 单文件二进制已构建: $(BIN)（默认托管 $(WEB_DIST)）"

## build-web: 安装前端依赖（幂等）并构建静态产物
.PHONY: build-web
build-web:
	cd $(WEB_DIR) && (test -d node_modules || $(NPM) install) && $(NPM) run build
	@echo "✅ 前端已构建: $(WEB_DIST)"

## run: 前台运行后端（Mock 模式，零配置）
.PHONY: run
run:
	PORT=$(PORT) $(GO) run ./cmd/server

## serve: 单端口运行 Demo（构建前端产物后由 Go 直接托管，:8080 一个端口搞定）
.PHONY: serve
serve: build-web
	@echo "▶ 单端口 Demo: http://localhost:$(PORT)"
	WEB_DIR=$(WEB_DIST) PORT=$(PORT) $(GO) run ./cmd/server

## dev: 同时启动后端与前端开发服务器
.PHONY: dev
dev: dev-backend dev-frontend

## dev-backend: 只启动后端（后台日志写 /tmp/agent-server.log）
.PHONY: dev-backend
dev-backend:
	@echo "▶ 后端 http://localhost:$(PORT)  (日志: /tmp/agent-server.log)"
	@PORT=$(PORT) $(GO) run ./cmd/server > /tmp/agent-server.log 2>&1 & echo $$! > /tmp/agent-server.pid

## dev-frontend: 只启动前端 Vite 开发服务器
.PHONY: dev-frontend
dev-frontend:
	cd $(WEB_DIR) && (test -d node_modules || $(NPM) install) && $(NPM) run dev

## test: 运行全部 Go 测试（含竞态检测）
.PHONY: test
test:
	$(GO) test -race -count=1 ./...

## test-web: 前端逻辑与组件渲染检查（无需浏览器）
.PHONY: test-web
test-web:
	node --experimental-strip-types scripts/web-smoke.mjs
	cd $(WEB_DIR) && node --experimental-strip-types scripts/verify-tool-restore.mjs
	cd $(WEB_DIR) && node --experimental-strip-types scripts/verify-ask-user-fixes.mjs
	cd $(WEB_DIR) && node scripts/verify-ask-answer.mjs
	cd $(WEB_DIR) && node scripts/verify-multi-question.mjs
	cd $(WEB_DIR) && node --experimental-strip-types scripts/verify-bubble-merge.mjs
	cd $(WEB_DIR) && node scripts/verify-tool-events.mjs
	cd $(WEB_DIR) && node scripts/verify-tool-summary.mjs
	cd $(WEB_DIR) && node scripts/verify-ask-user.mjs
	cd $(WEB_DIR) && node scripts/run-render-check.mjs

## test-all: Go + 前端全部检查
.PHONY: test-all
test-all: test test-web

## vet: 静态检查 + 格式检查
.PHONY: vet
vet:
	$(GO) vet ./...
	@test -z "$$(gofmt -l ./cmd ./internal)" || (echo "需要 gofmt:"; gofmt -l ./cmd ./internal; exit 1)

## smoke: 启动后端并用 curl 校验 SSE 全链路（需后端已在跑或由本目标启动）
.PHONY: smoke
smoke:
	@bash scripts/smoke.sh

## e2e: 端到端联调验收（新建/流式/刷新/切换/停止/单端口，24 项检查）
.PHONY: e2e
e2e: build-web
	@START_SERVER=1 PORT=$(PORT) bash scripts/e2e.sh

## e2e-ask-user: ask_user 跨进程恢复验证（提问→kill -9→重启→resume）
.PHONY: e2e-ask-user
e2e-ask-user:
	@bash scripts/e2e-ask-user.sh

## e2e-multi-question: 一次 ask_user 问多个问题（减少往返）
.PHONY: e2e-multi-question
e2e-multi-question:
	@bash scripts/e2e-multi-question.sh

## e2e-approval: 人工审批（manual 模式）验证：批准后真执行、拒绝后不执行
.PHONY: e2e-approval
e2e-approval:
	@bash scripts/e2e-approval.sh

## demo: 构建并启动单端口 Demo（浏览器访问 http://localhost:8080）
.PHONY: demo
demo: build-binary
	@echo "▶ Demo: http://localhost:$(PORT)"
	@PORT=$(PORT) $(BIN)

# ---- 桌面客户端（Electron）----

DESKTOP_DIR := desktop

## desktop-deps: 安装桌面端依赖（Electron / electron-builder）
.PHONY: desktop-deps
desktop-deps:
	cd $(DESKTOP_DIR) && (test -d node_modules || $(NPM) install)

## desktop: 开发态启动桌面客户端（会自动拉起 Go sidecar）
.PHONY: desktop
desktop: desktop-deps
	cd $(DESKTOP_DIR) && $(NPM) run prepare-sidecar && $(NPM) start

## desktop-smoke: 桌面壳冒烟（启动 sidecar → 校验 API/SSE → 验证进程回收）
.PHONY: desktop-smoke
desktop-smoke: desktop-deps
	cd $(DESKTOP_DIR) && $(NPM) run prepare-sidecar && node scripts/smoke-electron.mjs

## desktop-pack: 打包桌面应用（未签名，产物在 desktop/release）
.PHONY: desktop-pack
desktop-pack: desktop-deps
	cd $(DESKTOP_DIR) && $(NPM) run pack
	@echo "✅ 打包完成：$(DESKTOP_DIR)/release/"

## desktop-verify: 验证打包后的应用能真正运行（asar 解包 / API / SSE / 进程回收）
.PHONY: desktop-verify
desktop-verify: desktop-pack
	cd $(DESKTOP_DIR) && node scripts/verify-package.mjs

## desktop-test: 桌面壳回归（关窗不弹框 / 关窗即退出 / 孤儿进程守护 / 配置注入 / 持久化 / UI 修复）
.PHONY: desktop-test
desktop-test: desktop-deps
	cd $(DESKTOP_DIR) && $(NPM) run prepare-sidecar \
		&& node scripts/test-config-loader.mjs \
		&& node scripts/test-config-e2e.mjs \
		&& node scripts/test-close-no-dialog.mjs \
		&& node scripts/test-orphan-guard.mjs \
		&& node scripts/test-desktop-persistence.mjs \
		&& node scripts/test-ui-fixes.mjs

## desktop-ui-check: 在真实应用内验证 UI（user-select / app-region / 窗口控制）
.PHONY: desktop-ui-check
desktop-ui-check: desktop-deps
	cd $(DESKTOP_DIR) && $(NPM) run prepare-sidecar && node scripts/verify-ui-runtime.mjs

## desktop-config-dir: 显示桌面版配置文件应放置的目录
.PHONY: desktop-config-dir
desktop-config-dir:
	@echo "桌面版配置目录（macOS）: ~/Library/Application Support/agent-rebuild-desktop/config.env"
	@echo "命令行方式（make run/demo）: 仓库根目录的 .env"
	@echo "详见 docs/06-model-setup.md"

## desktop-test-packaged: 验证打包产物（关窗即退出 / 配置加载）
.PHONY: desktop-test-packaged
desktop-test-packaged: desktop-pack
	cd $(DESKTOP_DIR) && node scripts/verify-package.mjs \
		&& node scripts/test-packaged-config.mjs \
		&& node scripts/test-packaged-close.mjs

## clean: 清理构建产物
.PHONY: clean
clean:
	rm -rf $(BIN_DIR) $(WEB_DIST)
	@test ! -f /tmp/agent-server.pid || (kill $$(cat /tmp/agent-server.pid) 2>/dev/null; rm -f /tmp/agent-server.pid)
	@echo "✅ 已清理"

## stop: 停止 make dev-backend 启动的后端进程
.PHONY: stop
stop:
	@if test -f /tmp/agent-server.pid; then kill $$(cat /tmp/agent-server.pid) 2>/dev/null; rm -f /tmp/agent-server.pid; echo "✅ 后端已停止"; else echo "没有运行中的后端（找不到 pid 文件）"; fi

.PHONY: all
all: build
