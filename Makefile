# appkit 框架本体的验证入口。AGENTS.md「验证」一节引用这些目标。
# 域仓库的 Makefile 由脚手架生成，与本文件无关。

GO ?= go

.PHONY: check fmt vet build test test-db test-lint test-rules changelog all

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

# 规则集端到端：生成一个「每个组件都有子包」的域仓库，用钉版本的
# golangci-lint 与 go-arch-lint 真跑一遍，再植入已知违规确认它们会红。
# appkit 只生产规则集、自己不消费，这是唯一的消费点——golden 测试锁的是
# 「模板文本没变」，防不住「规则从写下来那天就是错的」。
# 慢（要拉两个检查器）且需要网络，故 opt-in；CI 每次都跑。
test-rules:
	APPKIT_RULES_E2E=1 $(GO) test -count=1 -v -run TestMaterializedRules ./internal/scaffold/

# 重生成 CHANGELOG.md：内容 = 各版本 annotated tag message 的镜像，按版本倒序。
# tag 是事实源，本文件禁手改——发版三件套之一（顺序见 AGENTS.md「发版」：
# 打 tag → make changelog 并提交 → gh release create）。
changelog:
	@{ echo '# Changelog'; echo; \
	   echo '按版本倒序；每条是其 annotated tag message 的镜像，事实源是 tag，本文件禁手改（发版后跑 `make changelog` 重新生成）。网页版见 [Releases](https://github.com/forgeplex/appkit/releases)。'; echo; \
	   for t in $$(git tag --sort=-v:refname); do \
	     echo "## $$t（$$(git log -1 --format=%cs $$t^{})）"; echo; \
	     git cat-file tag $$t | sed '1,/^$$/d'; echo; \
	   done; \
	 } > CHANGELOG.md

all: check test-lint test-rules
