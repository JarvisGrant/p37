# p37 —— 常用开发命令
#
# 注意 `cross` 目标：曾经出现过「在 macOS 上能编译、在 Linux 上编译不过」
# 的问题（构建标签下重复声明）。发布前务必跑一次 `make cross`。

GO      ?= go
OUT     ?= dist
VERSION ?= 0.4.1
# 用 := 立即求值：否则 p37、p37d、manifest 会各自调用一次 date，
# 同一个发布物里的 buildTime 不一致。
COMMIT  := $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
BUILD_TIME := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
GO_VERSION := $(shell $(GO) env GOVERSION)
LDFLAGS := -s -w \
	-X github.com/chenyiwen/p37/internal/buildinfo.Version=$(VERSION) \
	-X github.com/chenyiwen/p37/internal/buildinfo.Commit=$(COMMIT) \
	-X github.com/chenyiwen/p37/internal/buildinfo.BuildTime=$(BUILD_TIME) \
	-X github.com/chenyiwen/p37/internal/buildinfo.GoVersion=$(GO_VERSION)

VERSION_SLUG := $(subst /,_,$(VERSION))
RELEASE_NAME := p37-linux-amd64-$(VERSION_SLUG)
RELEASE_DIR  := $(OUT)/$(RELEASE_NAME)
RELEASE_NOTES := $(OUT)/RELEASE_NOTES.md

.PHONY: all build test vet fmt cross release release-check p36-bootstrap clean

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

## release-check 拒绝在脏工作区上构建：否则 manifest 会写出“干净 commit
## + 脏二进制”的组合。
release-check:
	@if [ -n "$$(git status --porcelain)" ]; then \
	  echo "release: 工作区不干净，拒绝构建"; \
	  git status --short; \
	  exit 1; \
	fi

## release 产出 linux/amd64 的静态发布物：
##   dist/p37-linux-amd64-<version>.tar.gz
##   dist/p37-linux-amd64-<version>.tar.gz.sha256
##   dist/RELEASE_NOTES.md（给 GitHub Release 用，不进 tar）
## tar 根目录严格只有 p37、p37d、manifest.json。
release: release-check test
	rm -rf $(RELEASE_DIR) $(RELEASE_DIR).tar.gz $(RELEASE_DIR).tar.gz.sha256 $(RELEASE_NOTES)
	mkdir -p $(RELEASE_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -buildvcs=false -ldflags "$(LDFLAGS)" -o $(RELEASE_DIR)/p37  ./cmd/p37
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -buildvcs=false -ldflags "$(LDFLAGS)" -o $(RELEASE_DIR)/p37d ./cmd/p37d
	printf '{\n  "version": "%s",\n  "commit": "%s",\n  "buildTime": "%s",\n  "goVersion": "%s"\n}\n' \
		"$(VERSION)" "$(COMMIT)" "$(BUILD_TIME)" "$(GO_VERSION)" > $(RELEASE_DIR)/manifest.json
	COPYFILE_DISABLE=1 tar -C $(RELEASE_DIR) -czf $(RELEASE_DIR).tar.gz p37 p37d manifest.json
	@if command -v sha256sum >/dev/null 2>&1; then \
	  (cd $(OUT) && sha256sum $(RELEASE_NAME).tar.gz > $(RELEASE_NAME).tar.gz.sha256); \
	else \
	  (cd $(OUT) && shasum -a 256 $(RELEASE_NAME).tar.gz > $(RELEASE_NAME).tar.gz.sha256); \
	fi
	@test "$$(find $(RELEASE_DIR) -maxdepth 1 -type f | wc -l | tr -d ' ')" = 3
	@want="$$(sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' $(RELEASE_DIR)/manifest.json)"; \
	  test "$$want" = "$(VERSION)" || { echo "release: manifest.version=$$want，期望 $(VERSION)"; exit 1; }
	@want="$$(sed -n 's/.*"commit"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' $(RELEASE_DIR)/manifest.json)"; \
	  test "$$want" = "$(COMMIT)" || { echo "release: manifest.commit=$$want，期望 $(COMMIT)"; exit 1; }
	sed -e 's|@VERSION@|$(VERSION)|g' \
	    -e 's|@COMMIT@|$(COMMIT)|g' \
	    -e 's|@BUILD_TIME@|$(BUILD_TIME)|g' \
	    -e 's|@GO_VERSION@|$(GO_VERSION)|g' \
	    packaging/p36/RELEASE_NOTES.md > $(RELEASE_NOTES)
	@echo "release: $(OUT)/$(RELEASE_NAME).tar.gz"
	@echo "release notes: $(RELEASE_NOTES)"
	@cat $(RELEASE_DIR).tar.gz.sha256

## p36-bootstrap 生成平台胶水（P36 使用 L1，不属于发布 tar）：
##   dist/p37-mount-linux-amd64
##   dist/p36-bootstrap/{p37-run,p37-mcp,p37-mount,config.toml,p37d.service,90-p37.conf,tmpfiles-p37.conf}
##   可选 root+overlay：config-overlay.toml、p37d-root-overlay.service
p36-bootstrap: release-check
	mkdir -p $(OUT)/p36-bootstrap
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -buildvcs=false -ldflags "$(LDFLAGS)" -o $(OUT)/p37-mount-linux-amd64 ./cmd/p37-mount
	cp $(OUT)/p37-mount-linux-amd64 $(OUT)/p36-bootstrap/p37-mount
	cp packaging/p36/p37-run packaging/p36/p37-mcp packaging/p36/p37 packaging/p36/config.toml packaging/p36/p37d.service packaging/p36/90-p37.conf packaging/p36/tmpfiles-p37.conf packaging/p36/config-overlay.toml packaging/p36/p37d-root-overlay.service $(OUT)/p36-bootstrap/
	chmod 0755 $(OUT)/p36-bootstrap/p37-run $(OUT)/p36-bootstrap/p37-mcp $(OUT)/p36-bootstrap/p37 $(OUT)/p36-bootstrap/p37-mount
	@echo "p36-bootstrap: $(OUT)/p36-bootstrap"

clean:
	rm -rf $(OUT)
