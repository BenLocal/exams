.PHONY: help build run test vet fmt migrate collect probe sources clean install

BIN := exams
PKG := ./cmd/exams

help: ## 显示可用目标
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

build: ## 编译到 ./exams
	go build -o $(BIN) $(PKG)

install: ## 安装到 $GOBIN
	go install $(PKG)

run: build ## 编译并启动服务
	./$(BIN) serve

test: ## 跑单元测试
	go test ./...

vet: ## go vet
	go vet ./...

fmt: ## 格式化
	gofmt -w .

migrate: build ## 执行数据库迁移
	./$(BIN) migrate

collect: build ## 立即抓取一次（可加 SOURCE=xxx 限定单个源）
	./$(BIN) collect $(SOURCE)

probe: build ## 校验某个数据源的解析（用法：make probe SOURCE=neea）
	@test -n "$(SOURCE)" || { echo "用法: make probe SOURCE=<标识>"; exit 1; }
	./$(BIN) probe $(SOURCE) --with-detail

sources: build ## 列出数据源及其状态
	./$(BIN) sources

clean: ## 清理构建产物
	rm -f $(BIN)
	go clean -testcache
