# 默认编译两端；首次构建由 Go 自动下载缺少的模块，无需 Python 或 vendor。
.DEFAULT_GOAL := build
.PHONY: all help check deps build client server _build-client _build-server install install-files test framework-test vendor-sync stress weaknet vet clean

ROOT := $(abspath $(dir $(lastword $(MAKEFILE_LIST))))
GO ?= go
PYTHON ?= python3
BINDIR ?= bin
BUILD_FLAGS ?= -trimpath
LDFLAGS ?=
TEST_FLAGS ?=
PREFIX ?= /opt/soft/sgate
DESTDIR ?=

# 显式使用模块模式，避免旧 vendor 副本覆盖 third_party 中的框架扩展。
# 离线构建可先 make vendor-sync，再设置 MOD=vendor。
MOD ?= mod
GO_CMD = $(GO) -C "$(ROOT)"
OUTPUT_DIR := $(if $(filter /%,$(BINDIR)),$(BINDIR),$(ROOT)/$(BINDIR))
SGATE_STRESS_REPORT ?= $(ROOT)/artifacts/stress.json
# install 后的 client/server 是安装选择项，不运行同名编译目标。
INSTALL_ROLE := $(if $(filter client,$(MAKECMDGOALS)),$(if $(filter server,$(MAKECMDGOALS)),all,client),$(if $(filter server,$(MAKECMDGOALS)),server,all))

all: build

help:
	@printf '%s\n' \
	  'make / make build    编译客户端和服务端到 bin/' \
	  'make client          仅编译客户端' \
	  'make server          仅编译服务端' \
	  'make install         安装已编译程序到 /opt/soft/sgate（先以普通用户 make build）' \
	  'make install server  只安装服务端（先以普通用户 make server）' \
	  'make install client  只安装客户端（先以普通用户 make client）' \
	  'make install-files   make install 的兼容别名，同样不调用 Go' \
	  '正式配置为 sgate-client.cf / sgate-server.cf；专用示例安装到 samples/' \
	  'make deps            预下载 go.mod 锁定的模块' \
	  'make test            框架及项目竞态测试（需要 C 编译器）' \
	  'make framework-test  测试两份本地框架扩展' \
	  'make vet             Go 静态检查' \
	  'make vendor-sync     下载依赖并同步 vendor（需要 Python 3）' \
	  'make stress          重连与资源压力测试' \
	  'make weaknet         弱网竞态测试' \
	  'make clean           删除本 Makefile 生成的两个程序' \
	  '可覆盖：GO、BINDIR、PREFIX、DESTDIR、BUILD_FLAGS、LDFLAGS、TEST_FLAGS、MOD；交叉编译使用 GOOS/GOARCH/CGO_ENABLED。'

check:
	@command -v "$(GO)" >/dev/null 2>&1 || { printf '%s\n' '错误：找不到 Go，请安装 Go 1.23 或以上版本，或设置 GO。' >&2; exit 1; }
	@for module in gin-service go-service; do \
	  if [ ! -f "$(ROOT)/third_party/$$module/go.mod" ]; then \
	    printf '%s\n' "错误：缺少 third_party/$${module}，请完整拉取包含本地框架扩展的项目；不能用上游版本替代。" >&2; \
	    exit 1; \
	  fi; \
	done

deps: check
	$(GO_CMD) mod download

build: _build-client _build-server

ifneq ($(filter install install-files,$(MAKECMDGOALS)),)
client server:
	@:
else
client: _build-client
server: _build-server
endif

_build-client: check
	mkdir -p "$(OUTPUT_DIR)"
	$(GO_CMD) build -mod=$(MOD) $(BUILD_FLAGS) -ldflags "$(LDFLAGS)" -o "$(OUTPUT_DIR)/sgate-client" ./cmd/sgate-client

_build-server: check
	mkdir -p "$(OUTPUT_DIR)"
	$(GO_CMD) build -mod=$(MOD) $(BUILD_FLAGS) -ldflags "$(LDFLAGS)" -o "$(OUTPUT_DIR)/sgate-server" ./cmd/sgate-server

# 普通用户先 make build，再 sudo make install；sudo 环境无需 Go。
# DESTDIR 仅用于暂存打包；写入配置的运行路径始终使用 PREFIX。
install install-files:
	sh "$(ROOT)/scripts/install.sh" "$(ROOT)" "$(OUTPUT_DIR)" "$(PREFIX)" "$(DESTDIR)" "$(INSTALL_ROLE)"

# vendor 不包含依赖自身的测试文件，因此框架测试始终读取 third_party。
framework-test: check
	$(GO_CMD) test -mod=mod -race $(TEST_FLAGS) github.com/acl-dev/gin-service github.com/acl-dev/go-service

vendor-sync: deps
	cd "$(ROOT)" && GO="$(GO)" $(PYTHON) scripts/sync-go-vendor.py

test: framework-test
	$(GO_CMD) test -mod=$(MOD) -race $(TEST_FLAGS) ./...

stress: check
	SGATE_STRESS=1 SGATE_STRESS_REPORT="$(SGATE_STRESS_REPORT)" $(GO_CMD) test -mod=$(MOD) -run '^TestStressReconnectResources$$' -v -timeout 10m ./internal/tunnel

weaknet: check
	$(GO_CMD) test -mod=$(MOD) -race -count=3 -run '^(TestWeakNetwork|TestNoiseWeakNetwork|TestHeartbeatNotStarved)' -v -timeout 8m ./internal/tunnel

vet: check
	$(GO_CMD) vet -mod=$(MOD) ./...

# 保留 bin/ 内其他工具、诊断程序；不清理配置、密钥和运行数据。
clean:
	rm -f "$(OUTPUT_DIR)/sgate-client" "$(OUTPUT_DIR)/sgate-server"
	@rmdir "$(OUTPUT_DIR)" 2>/dev/null || true
