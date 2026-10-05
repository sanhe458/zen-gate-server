# Zen Gate Server — 构建与发布
BIN      := zen-gate-server
VERSION  ?= 1.0.0
LDFLAGS  := -s -w -X main.buildVersion=$(VERSION) -X zen-gate-server/internal/server.Version=$(VERSION)
GO       ?= go

.PHONY: all build run test vet fmt clean release docker help

all: vet test build ## 默认：检查 + 测试 + 构建

build: ## 构建当前平台二进制到 dist/
	@mkdir -p dist
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BIN) ./cmd/zen-gate-server
	@echo "→ dist/$(BIN)"

run: ## 前台运行（本机 127.0.0.1:8787 调试用）
	$(GO) run ./cmd/zen-gate-server -port 8787

test: ## 运行单元测试
	$(GO) test ./...

vet: ## 静态检查
	$(GO) vet ./...

fmt: ## 格式化
	$(GO) fmt ./...

clean: ## 清理产物
	rm -rf dist

release: ## 交叉编译 linux/amd64 与 linux/arm64 到 dist/
	@mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BIN)-linux-amd64 ./cmd/zen-gate-server
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BIN)-linux-arm64 ./cmd/zen-gate-server
	@ls -lh dist/

docker: ## 构建 Docker 镜像
	docker build -t $(BIN):$(VERSION) -f deploy/Dockerfile .
	docker tag $(BIN):$(VERSION) $(BIN):latest

help: ## 显示帮助
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'
