.PHONY: help build run test test-store test-db test-db-stop vet fmt \
        migrate collect probe sources clean install

BIN := exams
PKG := ./cmd/exams

# Throwaway PostgreSQL for the store tests. It is deliberately named
# exams_test: the tests TRUNCATE every table and refuse to run against a
# database whose name does not contain "test".
TEST_DB_PORT  ?= 55433
TEST_DB_NAME  ?= exams_test
TEST_DATABASE_URL ?= postgres://exams:exams@127.0.0.1:$(TEST_DB_PORT)/$(TEST_DB_NAME)?sslmode=disable

help: ## 显示可用目标
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

build: ## 编译到 ./exams
	go build -o $(BIN) $(PKG)

install: ## 安装到 $GOBIN
	go install $(PKG)

run: build ## 编译并启动服务
	./$(BIN) serve

test: ## 跑全部测试（store 层无数据库时自动跳过）
	go test ./...

test-db: ## 起一个临时 postgres 供 store 测试用
	docker run -d --rm --name exams-test-pg \
		-e POSTGRES_PASSWORD=exams -e POSTGRES_USER=exams \
		-e POSTGRES_DB=$(TEST_DB_NAME) \
		-p 127.0.0.1:$(TEST_DB_PORT):5432 postgres:17-alpine
	@echo "等待就绪…"
	@for i in $$(seq 1 30); do \
		docker exec exams-test-pg pg_isready -U exams -q && break; sleep 1; done
	@echo "就绪：$(TEST_DATABASE_URL)"

test-db-stop: ## 停掉临时 postgres
	-docker rm -f exams-test-pg

test-store: ## 跑 store 层集成测试（会清空目标库！）
	TEST_DATABASE_URL="$(TEST_DATABASE_URL)" go test ./internal/store/ -count=1

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
