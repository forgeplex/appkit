# appkit 框架本体的验证入口。AGENTS.md「验证」一节引用这些目标。
# 域仓库的 Makefile 由脚手架生成，与本文件无关。

GO ?= go

.PHONY: check fmt vet build test test-db test-lint all

# 完成的定义（AGENTS.md）：这条全绿，且动了公开 API 时 apidiff 相对最新 tag 零 incompatible。
check: fmt vet build test

# gofmt 只报不改：这里要的是"有没有未格式化的文件"这个答案，而不是顺手改掉。
# 覆盖整棵树（含嵌套的 lint/ module）——gofmt 纯语法，不关心 module 边界。
# 就地格式化用 gofmt -w .（或编辑器保存时自动跑）。
fmt:
	@out=$$(gofmt -l .); \
	if [ -n "$$out" ]; then echo "以下文件未格式化："; echo "$$out"; exit 1; fi

vet:
	$(GO) vet ./...

build:
	$(GO) build ./...

# 不带 TEST_DATABASE_URL 时数据库集成测试自动 skip。
test:
	$(GO) test -count=1 ./...

# 含数据库集成测试。用法：
#   make test-db TEST_DATABASE_URL=postgres://user:pass@127.0.0.1:5432/db?sslmode=disable
test-db:
	@test -n "$(TEST_DATABASE_URL)" || { echo "需要 TEST_DATABASE_URL"; exit 1; }
	TEST_DATABASE_URL='$(TEST_DATABASE_URL)' $(GO) test -race -count=1 ./...

# lint/ 是独立嵌套 module（自带 go.mod），不在 ./... 覆盖范围内。
test-lint:
	cd lint && $(GO) test ./...

all: check test-lint
