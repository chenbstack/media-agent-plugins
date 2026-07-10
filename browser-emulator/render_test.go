package main

import (
	"testing"
	"time"

	"github.com/chenbstack/media-agent-plugin-sdk-go/providers"
)

func TestUnderChallenge(t *testing.T) {
	cases := []struct {
		name string
		html string
		want bool
	}{
		{"just a moment", "<html><head><title>Just a moment...</title></head></html>", true},
		{"中文挑战页", "<title>请稍候…</title>", true},
		{"ddos-guard", "<div>DDoS-Guard protection</div>", true},
		{"cf-challenge-running", `<div class="cf-challenge-running"></div>`, true},
		{"正常页面", "<html><head><title>M-Team</title></head><body>userdetails.php</body></html>", false},
		{"空页面", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := underChallenge(c.html); got != c.want {
				t.Fatalf("underChallenge(%q) = %v, want %v", c.name, got, c.want)
			}
		})
	}
}

func TestNormalizeWaitUntil(t *testing.T) {
	cases := map[string]waitUntilState{
		"":                 waitDomContentLoaded,
		"domcontentloaded": waitDomContentLoaded,
		"load":             waitLoad,
		"networkidle":      waitNetworkIdle,
		"commit":           waitCommit,
		"NETWORKIDLE":      waitNetworkIdle,
		"未知值":              waitDomContentLoaded,
	}
	for in, want := range cases {
		if got := normalizeWaitUntil(in); got != want {
			t.Fatalf("normalizeWaitUntil(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestBuildSetCookieParam(t *testing.T) {
	expires := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)

	// 带 Domain 的 cookie：使用 Domain+Path，不设置 URL。
	c0 := providers.HTTPCookie{Name: "with_domain", Value: "v1", Domain: ".m-team.cc", Path: "/torrents", Secure: true, HTTPOnly: true, Expires: expires}
	p0 := buildSetCookieParam(c0, "https://example.com/page")
	if p0.Name != "with_domain" || p0.Value != "v1" {
		t.Fatalf("name/value 不匹配: %+v", p0)
	}
	if p0.Domain != ".m-team.cc" {
		t.Fatalf("Domain = %q, want .m-team.cc", p0.Domain)
	}
	if p0.Path != "/torrents" {
		t.Fatalf("Path = %q, want /torrents", p0.Path)
	}
	if p0.URL != "" {
		t.Fatalf("URL 应为空（已有 Domain），得到 %q", p0.URL)
	}
	if !p0.Secure {
		t.Fatalf("Secure 应为 true")
	}
	if !p0.HTTPOnly {
		t.Fatalf("HTTPOnly 应为 true")
	}
	if p0.Expires == nil {
		t.Fatalf("Expires 应被设置")
	}

	// 无 Domain 的 cookie：用页面 URL 兜底，Path 默认省略。
	c1 := providers.HTTPCookie{Name: "no_domain", Value: "v2"}
	p1 := buildSetCookieParam(c1, "https://example.com/page")
	if p1.URL != "https://example.com/page" {
		t.Fatalf("URL = %q, want 页面 URL 兜底", p1.URL)
	}
	if p1.Domain != "" {
		t.Fatalf("Domain 应为空，得到 %q", p1.Domain)
	}
}

func TestCookieActionsCount(t *testing.T) {
	if got := cookieActions(nil, "https://x"); got != nil {
		t.Fatalf("空输入应返回 nil")
	}
	acts := cookieActions([]providers.HTTPCookie{{Name: "a", Value: "1"}, {Name: "b", Value: "2"}}, "https://x")
	if len(acts) != 2 {
		t.Fatalf("应有 2 个动作，得到 %d", len(acts))
	}
}

func TestJSString(t *testing.T) {
	if got := jsString(`.quote`); got != `'.quote'` {
		t.Fatalf("jsString = %q", got)
	}
	if got := jsString(`a'b`); got != `'a\'b'` {
		t.Fatalf("jsString 转义单引号失败: %q", got)
	}
}

func TestFirstPositive(t *testing.T) {
	if got := firstPositive(0, 0, 45); got != 45 {
		t.Fatalf("firstPositive = %d, want 45", got)
	}
	if got := firstPositive(10, 45); got != 10 {
		t.Fatalf("firstPositive = %d, want 10", got)
	}
	if got := firstPositive(0, 0, 0); got != 0 {
		t.Fatalf("firstPositive = %d, want 0", got)
	}
}
