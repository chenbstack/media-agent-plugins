#!/usr/bin/env bash
# 打包插件为可分发的 tar.gz。
#
# 每个 插件 × 平台 产出一个包：dist/<插件目录名>-v<版本>-<os>-<arch>.tar.gz
# 另外产出 dist/manifests.tar.gz（每个插件一个 <插件目录名>/plugin.yaml），
# 供 cloud 检测仓库时一次下载全部插件元数据，避免逐个调用 contents API。
# 包内是一层以插件目录名命名的顶级目录，内容与宿主安装目录
# （media-agent-lab/server/plugins/<id>/）完全一致：
#   <插件目录名>/
#     plugin.yaml
#     icon.svg              （存在才打包）
#     config.schema.json    （存在才打包）
#     bin/media-agent-plugin-<插件目录名>-<os>-<arch>
#
# 因此安装即解压：tar -xzf xxx.tar.gz -C <宿主>/server/plugins/
#
# 用法：scripts/package.sh <插件目录名>... （由根 Makefile 调用）
# 环境变量：PLATFORMS 覆盖平台列表，默认 "darwin-arm64 linux-amd64 linux-arm64"

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DIST="$ROOT/dist"
PLATFORMS="${PLATFORMS:-darwin-arm64 linux-amd64 linux-arm64}"

if [ "$#" -eq 0 ]; then
    echo "用法: $0 <插件目录名>..." >&2
    exit 1
fi

mkdir -p "$DIST"

for plugin in "$@"; do
    src="$ROOT/$plugin"
    if [ ! -f "$src/plugin.yaml" ]; then
        echo "错误: $plugin 缺少 plugin.yaml" >&2
        exit 1
    fi
    version="$(awk '$1 == "version:" { gsub(/"/, "", $2); print $2; exit }' "$src/plugin.yaml")"
    if [ -z "$version" ]; then
        echo "错误: 无法从 $plugin/plugin.yaml 解析 version" >&2
        exit 1
    fi

    for platform in $PLATFORMS; do
        goos="${platform%-*}"
        goarch="${platform#*-}"
        binary="media-agent-plugin-$plugin-$platform"

        echo "==> 构建 $plugin v$version ($platform)"
        (cd "$ROOT" && CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
            go build -trimpath -ldflags "-s -w" \
            -o "$src/bin/$binary" "./$plugin")

        stage="$(mktemp -d)"
        trap 'rm -rf "$stage"' EXIT
        mkdir -p "$stage/$plugin/bin"
        cp "$src/plugin.yaml" "$stage/$plugin/"
        [ -f "$src/icon.svg" ] && cp "$src/icon.svg" "$stage/$plugin/"
        [ -f "$src/config.schema.json" ] && cp "$src/config.schema.json" "$stage/$plugin/"
        cp "$src/bin/$binary" "$stage/$plugin/bin/"

        out="$DIST/$plugin-v$version-$platform.tar.gz"
        tar -czf "$out" -C "$stage" "$plugin"
        rm -rf "$stage"
        trap - EXIT
        echo "    -> ${out#"$ROOT"/}"
    done
done

echo "==> 打包插件元数据 manifests.tar.gz"
stage="$(mktemp -d)"
trap 'rm -rf "$stage"' EXIT
for plugin in "$@"; do
    mkdir -p "$stage/$plugin"
    cp "$ROOT/$plugin/plugin.yaml" "$stage/$plugin/"
done
tar -czf "$DIST/manifests.tar.gz" -C "$stage" "$@"
rm -rf "$stage"
trap - EXIT
echo "    -> dist/manifests.tar.gz"

echo
echo "全部完成，产物在 dist/："
ls -lh "$DIST"
