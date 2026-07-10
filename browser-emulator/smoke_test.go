//go:build smoke

// smoke_test.go 用主仓 pluginrpc.ExternalPlugin 拉起已构建的插件二进制，
// 通过 go-plugin net/rpc 全链路验证 manifest / config-schema / RendererRender。
// 默认不参与 go test，需要真实浏览器后端，运行方式见 Makefile smoke 目标：
//
//	SMOKE_PLUGIN_BIN=./bin/media-agent-plugin-browser-emulator-darwin-arm64 \
//	SMOKE_TARGET_URL=http://127.0.0.1:8899/testpage.html \
//	go test -tags smoke -run TestSmoke -v ./...
package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chenbstack/media-agent-plugin-sdk-go"
	"github.com/chenbstack/media-agent-plugin-sdk-go/pluginrpc"
	"github.com/chenbstack/media-agent-plugin-sdk-go/providers"
)

func TestSmoke(t *testing.T) {
	pluginBin := os.Getenv("SMOKE_PLUGIN_BIN")
	if pluginBin == "" {
		t.Skip("未设置 SMOKE_PLUGIN_BIN，跳过端到端 smoke 测试")
	}
	target := os.Getenv("SMOKE_TARGET_URL")
	if target == "" {
		target = "http://127.0.0.1:8899/testpage.html"
	}

	external := pluginrpc.ExternalPlugin{
		Manifest: pluginsdk.Manifest{
			ID:           "browser-emulator",
			Name:         "浏览器仿真",
			Version:      "0.1.0",
			Type:         "cli",
			Capabilities: []string{"renderer.render", "renderer.test"},
			Resources:    pluginsdk.Resources{MemoryLimitMB: 512, IdleTimeoutSeconds: 60},
		},
		Command: pluginBin,
		Stderr:  os.Stderr,
	}
	plugin := external.Plugin()

	if plugin.NewRenderer == nil {
		t.Fatal("ExternalPlugin 未暴露 NewRenderer（capability 未识别为 renderer）")
	}

	// 1) manifest / config-schema 走 RPC 拉起子进程读取。
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	cfg := map[string]any{
		"backend": "lightweight",
	}

	inst := pluginsdk.Instance{ID: "smoke", Name: "smoke", Config: cfg}
	provider, err := plugin.NewRenderer(ctx, inst, nil)
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}

	// 2) RendererRender 全链路。
	res, err := provider.Render(ctx, providers.RenderRequest{
		URL:            target,
		UserAgent:      "MediaAgentSmoke/1.0",
		Cookies:        []providers.HTTPCookie{{Name: "smoke", Value: "ok123", Domain: "127.0.0.1", Path: "/"}},
		WaitUntil:      "load",
		WaitSelector:   os.Getenv("SMOKE_WAIT_SELECTOR"),
		TimeoutSeconds: 45,
	})
	if err != nil {
		t.Fatalf("RendererRender: %v", err)
	}

	expect := os.Getenv("SMOKE_EXPECT")
	if expect == "" {
		expect = "JS_RENDERED_OK"
	}
	if !strings.Contains(res.HTML, expect) {
		t.Fatalf("渲染结果未包含期望内容 %q；HTML 前 300 字: %s", expect, first(res.HTML, 300))
	}
	t.Logf("[PASS] 全链路 RendererRender 成功 status=%d finalURL=%s htmlLen=%d cookies=%d",
		res.Status, res.FinalURL, len(res.HTML), len(res.Cookies))

	// 3) RendererTest（自检 about:blank/data 页）。
	if err := provider.TestConnection(ctx); err != nil {
		t.Fatalf("RendererTest: %v", err)
	}
	t.Logf("[PASS] RendererTest 自检通过")
}

func first(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
