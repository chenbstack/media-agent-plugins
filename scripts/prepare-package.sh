#!/usr/bin/env bash
# 计算本次需要重新构建的插件；发布完整快照时，从上一 Release 复用未变化插件资产。
#
# 环境变量：
#   BASE_REF             用于判断变更的 Git ref/commit；不可用时全部重建。
#   FULL_SNAPSHOT        1 表示必须为所有插件准备资产（发布标签场景）。
#   REUSE_RELEASE_TAG    上一个完整 Release 标签；为空时不能复用。
#   PLATFORMS            平台列表。
#
# stdout 只输出空格分隔的待构建插件列表，日志写入 stderr。

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DIST="$ROOT/dist"
BASE_REF="${BASE_REF:-}"
FULL_SNAPSHOT="${FULL_SNAPSHOT:-0}"
REUSE_RELEASE_TAG="${REUSE_RELEASE_TAG:-}"
PLATFORMS="${PLATFORMS:-darwin-arm64 linux-amd64 linux-arm64}"

if [ "$#" -eq 0 ]; then
    echo "用法: $0 <插件目录名>..." >&2
    exit 1
fi

mkdir -p "$DIST"

base_available=0
if [ -n "$BASE_REF" ] && git -C "$ROOT" cat-file -e "$BASE_REF^{commit}" 2>/dev/null; then
    base_available=1
fi

shared_changed=0
shared_paths=(
    go.mod
    go.sum
    Makefile
    scripts/package.sh
    scripts/package-manifests.sh
    scripts/prepare-package.sh
    .github/workflows/package.yml
)
if [ "$base_available" -eq 0 ] || ! git -C "$ROOT" diff --quiet "$BASE_REF" HEAD -- "${shared_paths[@]}"; then
    shared_changed=1
fi

build_plugins=()
for plugin in "$@"; do
    manifest="$ROOT/$plugin/plugin.yaml"
    if [ ! -f "$manifest" ]; then
        echo "错误: $plugin 缺少 plugin.yaml" >&2
        exit 1
    fi
    version="$(awk '$1 == "version:" { gsub(/"/, "", $2); print $2; exit }' "$manifest")"
    if [ -z "$version" ]; then
        echo "错误: 无法从 $plugin/plugin.yaml 解析 version" >&2
        exit 1
    fi

    plugin_changed=0
    if [ "$base_available" -eq 0 ] || ! git -C "$ROOT" diff --quiet "$BASE_REF" HEAD -- "$plugin"; then
        plugin_changed=1
    fi

    if [ "$plugin_changed" -eq 1 ] && [ "$base_available" -eq 1 ]; then
        base_manifest="$(git -C "$ROOT" show "$BASE_REF:$plugin/plugin.yaml" 2>/dev/null || true)"
        if [ -n "$base_manifest" ]; then
            base_version="$(printf '%s\n' "$base_manifest" | awk '$1 == "version:" { gsub(/"/, "", $2); print $2; exit }')"
            if [ -n "$base_version" ] && [ "$base_version" = "$version" ]; then
                echo "错误: $plugin 内容已变化，但 plugin.yaml 版本仍为 $version" >&2
                exit 1
            fi
        fi
    fi

    rebuild=0
    if [ "$shared_changed" -eq 1 ] || [ "$plugin_changed" -eq 1 ]; then
        rebuild=1
    fi

    if [ "$rebuild" -eq 0 ] && [ "$FULL_SNAPSHOT" = "1" ]; then
        if [ -z "$REUSE_RELEASE_TAG" ]; then
            rebuild=1
        else
            reuse_ok=1
            pattern="$plugin-v$version-*.tar.gz"
            if ! gh release download "$REUSE_RELEASE_TAG" --pattern "$pattern" --dir "$DIST" --clobber >/dev/null 2>&1; then
                reuse_ok=0
            fi
            for platform in $PLATFORMS; do
                archive="$plugin-v$version-$platform.tar.gz"
                if [ ! -s "$DIST/$archive" ]; then
                    reuse_ok=0
                fi
            done
            if [ "$reuse_ok" -eq 1 ]; then
                echo "复用 $plugin v${version}：$REUSE_RELEASE_TAG" >&2
            else
                for platform in $PLATFORMS; do
                    rm -f "$DIST/$plugin-v$version-$platform.tar.gz"
                done
                echo "$plugin 的历史资产不完整，改为重新构建" >&2
                rebuild=1
            fi
        fi
    fi

    if [ "$rebuild" -eq 1 ]; then
        build_plugins+=("$plugin")
    fi
done

if [ "${#build_plugins[@]}" -eq 0 ]; then
    echo ""
else
    (IFS=' '; echo "${build_plugins[*]}")
fi
