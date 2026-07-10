//go:build browsereval

// eval_test.go 是轻量后端 vs Chromium 的功能评测 harness，默认不参与 go test。
// 运行方式（需先启动本地测试页和/或 CDP 引擎，见 Makefile eval 目标）：
//
//	go test -tags browsereval -run TestEval -v ./...
//
// 通过环境变量选择被测后端：
//
//	EVAL_CHROMIUM=1 评测隐身 Chromium（CloakBrowser）后端；默认评测轻量 Lightpanda
//	EVAL_TARGET_URL 被测页面（默认本地 JS 测试页）
package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chenbstack/media-agent-plugin-sdk-go/providers"
)

func TestEval(t *testing.T) {
	target := os.Getenv("EVAL_TARGET_URL")
	if target == "" {
		target = "http://127.0.0.1:8899/testpage.html"
	}

	cfg := Config{
		Backend:               BackendLightweight,
		LightweightEngine:     EngineLightpanda,
		DefaultTimeoutSeconds: 30,
	}
	if os.Getenv("EVAL_CHROMIUM") == "1" {
		cfg.Backend = BackendChromium
	}

	r := &renderer{cfg: cfg}

	waitSel := ".rendered"
	if v := os.Getenv("EVAL_WAIT_SELECTOR"); v != "" {
		waitSel = v
		if v == "none" {
			waitSel = ""
		}
	}
	expect := "JS_RENDERED_OK"
	if v := os.Getenv("EVAL_EXPECT"); v != "" {
		expect = v
	}

	req := providers.RenderRequest{
		URL:            target,
		UserAgent:      "MediaAgentEval/1.0 (eval-ua-probe)",
		Cookies:        []providers.HTTPCookie{{Name: "evalsession", Value: "evalvalue123", Domain: "127.0.0.1", Path: "/"}},
		WaitUntil:      "load",
		WaitSelector:   waitSel,
		TimeoutSeconds: 30,
	}

	start := time.Now()
	res, err := r.Render(context.Background(), req)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("[FAIL] Render 报错: %v", err)
	}

	report := func(name string, ok bool, detail string) {
		status := "PASS"
		if !ok {
			status = "FAIL"
		}
		t.Logf("[%s] %-22s %s", status, name, detail)
	}

	jsOK := strings.Contains(res.HTML, expect)
	report("基本JS渲染", jsOK, "期望 HTML 含 "+expect)

	if strings.Contains(target, "127.0.0.1") {
		lateOK := strings.Contains(res.HTML, "LATE_NODE")
		report("异步DOM改写", lateOK, "期望 HTML 含 setTimeout 追加的 LATE_NODE")
	}

	cookieInjected := strings.Contains(res.HTML, "evalsession=evalvalue123")
	report("cookie注入(document.cookie)", cookieInjected, "期望页面 JS 读到注入的 cookie")

	cookieReadback := false
	for _, c := range res.Cookies {
		if c.Name == "evalsession" && c.Value == "evalvalue123" {
			cookieReadback = true
		}
	}
	report("cookie回读", cookieReadback, "期望渲染后 cookie 列表含 evalsession")

	waitSelOK := jsOK // WaitSelector .rendered 命中才可能拿到 JS_RENDERED_OK
	report("WaitSelector等待", waitSelOK, "等待 .rendered 出现后取快照")

	t.Logf("  backend=%s engine=%s status=%d finalURL=%s elapsed=%s htmlLen=%d cookies=%d",
		cfg.Backend, cfg.LightweightEngine, res.Status, res.FinalURL, elapsed, len(res.HTML), len(res.Cookies))

	if !jsOK {
		t.Logf("  HTML 首 600 字:\n%s", truncate(res.HTML, 600))
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
