#!/usr/bin/env bash
# 生成最新 Release 使用的完整插件目录，不构建任何二进制。

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DIST="$ROOT/dist"

if [ "$#" -eq 0 ]; then
    echo "用法: $0 <插件目录名>..." >&2
    exit 1
fi

mkdir -p "$DIST"
stage="$(mktemp -d)"
trap 'rm -rf "$stage"' EXIT

for plugin in "$@"; do
    src="$ROOT/$plugin"
    if [ ! -f "$src/plugin.yaml" ]; then
        echo "错误: $plugin 缺少 plugin.yaml" >&2
        exit 1
    fi
    mkdir -p "$stage/$plugin"
    cp "$src/plugin.yaml" "$stage/$plugin/"
    for extra in config.schema.json icon.svg README.md; do
        [ -f "$src/$extra" ] && cp "$src/$extra" "$stage/$plugin/"
    done
done

tar -czf "$DIST/manifests.tar.gz" -C "$stage" "$@"
echo "完整插件目录已生成：dist/manifests.tar.gz"
