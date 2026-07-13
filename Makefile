# media-agent 第三方插件统一构建 / 打包
#
# 单 Go module，多插件：每个插件目录是一个独立的 package main，
# 二进制只链接各自 import 的代码和依赖（可用 `go version -m <二进制>` 验证）。
#
# 打包产物：dist/<插件>-v<版本>-<os>-<arch>.tar.gz，包内结构与宿主
# server/plugins/<id>/ 安装目录一致（plugin.yaml + icon.svg + config.schema.json + bin/）。
# 安装即解压：tar -xzf dist/xxx.tar.gz -C <宿主>/server/plugins/
# 另有 dist/manifests.tar.gz（各插件的 plugin.yaml 汇总），供 cloud 检测仓库时
# 一次下载全部插件元数据。

PLUGINS   ?= drive115 browser-emulator
PLATFORMS ?= darwin-arm64 linux-amd64 linux-arm64

.PHONY: build package package-archives package-manifests test vet clean

# 本机平台快速构建（开发用）
build:
	@for p in $(PLUGINS); do \
		out=$$p/bin/media-agent-plugin-$$p-$$(go env GOOS)-$$(go env GOARCH); \
		go build -o $$out ./$$p && echo "built $$out"; \
	done

# 全平台交叉编译 + 完整插件目录。
package: package-archives package-manifests

package-archives:
	PLATFORMS="$(PLATFORMS)" ./scripts/package.sh $(PLUGINS)

package-manifests:
	./scripts/package-manifests.sh $(PLUGINS)

test:
	go test ./...

vet:
	go vet ./...

clean:
	rm -rf dist
	@for p in $(PLUGINS); do rm -f $$p/bin/media-agent-plugin-$$p-*; done
