# appkit 框架本体的验证入口。AGENTS.md「验证」一节引用这些目标。
# 域仓库的 Makefile 由脚手架生成，与本文件无关。

GO ?= go

.PHONY: check fmt vet build test test-db test-db-local test-downstream-local test-acceptance test-lint test-rules changelog tag all

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

# 隔离的临时 PostgreSQL 集群；不读取现有 TEST_DATABASE_URL，也不启动系统服务。
# 需要完整的本地 server 安装：APPKIT_POSTGRES_BIN=/path/to/bin make test-db-local
test-db-local:
	bash scripts/test-db-local.sh

# 只在复制的现有项目源码中验收，不修改业务仓库。项目测试须先人工审查，非执行沙箱。
# APPKIT_APPS_ROOT 默认 ../apps；TOOLS 及依赖须已就绪；失败保留副本和日志。
test-downstream-local:
	bash scripts/test-downstream-local.sh

# 四个真实 Go module 的跨项目复用/兼容升级验收（包括子进程 race）。
test-acceptance:
	$(GO) test -race -count=1 -v ./internal/acceptance

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

# 发版三件套第 1 步：打 tag。主 tag 是 annotated，正文即事实源；
# lint/ 是嵌套 module，Go 要求 lint/vX.Y.Z 前缀 tag 才能被 @version 解析
# （缺了它，域仓库 CI 按主版本号装 appkit-lint 会报 unknown revision）——
# 两个 tag 绑在同一目标里，物理上杜绝漏打。lint tag 是轻量 tag：
# 事实源仍是主 tag 的 message，它只是给 Go 模块解析器的路标。
# 用法：make tag VERSION=vX.Y.Z MSG=/tmp/msg
tag:
	@test -n "$(VERSION)" || { echo "用法：make tag VERSION=vX.Y.Z MSG=/tmp/msg"; exit 1; }
	@test -n "$(MSG)" || { echo "用法：make tag VERSION=vX.Y.Z MSG=/tmp/msg"; exit 1; }
	@case "$(VERSION)" in v[0-9]*.[0-9]*.[0-9]*) ;; *) echo "VERSION 必须形如 vX.Y.Z"; exit 1 ;; esac
	@! git rev-parse -q --verify "refs/tags/$(VERSION)" >/dev/null || { echo "tag $(VERSION) 已存在"; exit 1; }
	@! git rev-parse -q --verify "refs/tags/lint/$(VERSION)" >/dev/null || { echo "tag lint/$(VERSION) 已存在"; exit 1; }
	git tag -a $(VERSION) -F '$(MSG)'
	git tag "lint/$(VERSION)"
	@echo "已打 $(VERSION) + lint/$(VERSION)；推送：git push origin main $(VERSION) lint/$(VERSION)"

# 重生成 CHANGELOG.md：内容 = 各版本 annotated tag message 的镜像，按版本倒序。
# tag 是事实源，本文件禁手改——发版三件套之一（顺序见 AGENTS.md「发版」：
# 打 tag → make changelog 并提交 → gh release create）。
# --list 'v[0-9]*' 排除 lint/vX.Y.Z 路标 tag——它们不是版本叙事，且是轻量 tag，
# git cat-file tag 读不出 message。
changelog:
	@{ echo '# Changelog'; echo; \
	   echo '按版本倒序；每条是其 annotated tag message 的镜像，事实源是 tag，本文件禁手改（发版后跑 `make changelog` 重新生成）。网页版见 [Releases](https://github.com/forgeplex/appkit/releases)。'; echo; \
	   for t in $$(git tag --list 'v[0-9]*' --sort=-v:refname); do \
	     echo "## $$t（$$(git log -1 --format=%cs $$t^{})）"; echo; \
	     git cat-file tag $$t | sed '1,/^$$/d'; echo; \
	   done; \
	 } > CHANGELOG.md

all: check test-lint test-rules
