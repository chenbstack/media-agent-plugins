# 浏览器仿真插件构建 / 测试 / 轻量引擎下载
#
# 纯 Go chromedp 驱动 CDP，无 node / playwright 驱动依赖。
# 轻量引擎二进制（Lightpanda）体积大，不纳入仓库（见 .gitignore）。
# 部署时用 make fetch-lightpanda 拉取到 bin/，或在详情页安装「轻量引擎」组件。
# Chromium 兜底后端使用隐身 Chromium（CloakBrowser，chromedp ExecAllocator）。

BINARY      := media-agent-plugin-browser-emulator
GOOS        ?= $(shell go env GOOS)
GOARCH      ?= $(shell go env GOARCH)
OUT         := bin/$(BINARY)-$(GOOS)-$(GOARCH)

LIGHTPANDA_VERSION ?= nightly

.PHONY: build test vet fetch-lightpanda eval smoke clean

build:
	go build -o $(OUT) .
	@echo "built $(OUT)"

test:
	go test ./...

vet:
	go vet ./...

# 下载 Lightpanda macOS arm64 nightly 到 bin/lightpanda-<os>-<arch>
fetch-lightpanda:
	mkdir -p bin
	curl -fL -o bin/lightpanda-$(GOOS)-$(GOARCH) \
	  https://github.com/lightpanda-io/browser/releases/download/$(LIGHTPANDA_VERSION)/lightpanda-aarch64-macos
	chmod +x bin/lightpanda-$(GOOS)-$(GOARCH)
	@echo "fetched bin/lightpanda-$(GOOS)-$(GOARCH)"

# 后端功能评测（需先起本地测试页与轻量引擎 CDP，见 README）。
eval:
	go test -tags browsereval -run TestEval -v ./...

# 端到端 smoke（走 pluginrpc.ExternalPlugin 拉起已构建二进制）。
smoke: build
	SMOKE_PLUGIN_BIN=$(OUT) go test -tags smoke -run TestSmoke -v ./...

clean:
	rm -rf bin
