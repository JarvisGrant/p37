# p37 —— 常用开发命令
#
# 注意 `cross` 目标：曾经出现过「在 macOS 上能编译、在 Linux 上编译不过」
# 的问题（构建标签下重复声明）。发布前务必跑一次 `make cross`。

GO      ?= go
OUT     ?= dist
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build test vet fmt cross clean

all: fmt vet test build

build:
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(OUT)/p37  ./cmd/p37
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(OUT)/p37d ./cmd/p37d

test:
	$(GO) test ./... -count=1

vet:
	$(GO) vet ./...

fmt:
	gofmt -l -w $(shell $(GO) list -f '{{.Dir}}' ./...)

## cross 在三个目标平台上各编译一遍（不产出发布物，只做编译校验）。
cross:
	@set -e; for t in darwin/amd64 linux/amd64 linux/arm64; do \
	  os=$${t%/*}; arch=$${t#*/}; \
	  printf '%-16s ' "$$t"; \
	  GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 $(GO) build ./... && echo OK; \
	done

## release 产出 linux/amd64 的静态二进制（生产目标）。
release:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(OUT)/p37-linux-amd64      ./cmd/p37
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(OUT)/p37d-linux-amd64     ./cmd/p37d

clean:
	rm -rf $(OUT)
