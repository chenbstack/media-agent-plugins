package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/storage"
	"github.com/chromedp/chromedp"

	"github.com/chenbstack/media-agent-plugin-sdk-go/providers"
)

// challengeMarkers 对齐主仓 server/internal/plugins/official/site/provider.go 的 underChallenge：
// 页面标题或正文命中这些标记说明仍停留在 Cloudflare / DDoS-GUARD 挑战页，
// 需要继续等待浏览器完成挑战后再取快照。
var challengeMarkers = []string{
	"<title>just a moment...</title>", "<title>请稍候…</title>", "ddos-guard",
	"cf-challenge-running", "cf-please-wait", "challenge-spinner", "trk_jschal_js",
}

// underChallenge 判断当前 HTML 是否仍是反爬挑战页。
func underChallenge(html string) bool {
	lower := strings.ToLower(html)
	for _, marker := range challengeMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// renderWithCDP 是所有后端共用的渲染主逻辑：拿到一个 CDP allocator context
// （由各后端准备：lightweight 接 Lightpanda 的远端 CDP、chromium 由 chromedp
// 启动隐身 Chromium/CloakBrowser）后，注入 cookie/UA/headers 打开页面，按 WaitUntil/WaitSelector 等待，
// 对挑战页做启发式重试，最终取回渲染后 HTML、最终 URL、状态码和全量 cookie。
//
// 纯 Go 实现，不依赖 node/playwright 驱动进程。
func renderWithCDP(allocCtx context.Context, cfg Config, req providers.RenderRequest) (providers.RenderResult, error) {
	if strings.TrimSpace(req.URL) == "" {
		return providers.RenderResult{}, fmt.Errorf("渲染请求缺少 URL")
	}

	timeout := time.Duration(firstPositive(req.TimeoutSeconds, cfg.DefaultTimeoutSeconds, 45)) * time.Second

	ctx, cancel := chromedp.NewContext(allocCtx)
	defer cancel()
	ctx, cancelT := context.WithTimeout(ctx, timeout)
	defer cancelT()

	// 捕获主文档响应状态码与生命周期事件。
	// 注意兼容性差异：Lightpanda 目前不发 Network.responseReceived 事件（拿不到状态码），
	// domContentEventFired / loadEventFired 均正常。
	var status int
	domFired := make(chan struct{}, 1)
	loadFired := make(chan struct{}, 1)
	chromedp.ListenTarget(ctx, func(ev interface{}) {
		switch e := ev.(type) {
		case *network.EventResponseReceived:
			if e.Type == network.ResourceTypeDocument && status == 0 {
				status = int(e.Response.Status)
			}
		case *page.EventDomContentEventFired:
			trySignal(domFired)
		case *page.EventLoadEventFired:
			trySignal(loadFired)
		}
	})

	// 启用域 + 注入 UA / headers / cookie，然后发起导航。
	setup := []chromedp.Action{
		network.Enable(),
		page.Enable(),
	}
	if ua := strings.TrimSpace(req.UserAgent); ua != "" {
		// Lightpanda 会让页面 navigator.userAgent 生效。
		setup = append(setup, emulation.SetUserAgentOverride(ua))
	}
	if len(req.Headers) > 0 {
		headers := network.Headers{}
		for k, v := range req.Headers {
			headers[k] = v
		}
		setup = append(setup, network.SetExtraHTTPHeaders(headers))
	}
	setup = append(setup, cookieActions(req.Cookies, req.URL)...)
	if err := chromedp.Run(ctx, setup...); err != nil {
		return providers.RenderResult{}, fmt.Errorf("初始化渲染会话失败: %w", err)
	}

	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		_, _, _, _, err := page.Navigate(req.URL).Do(ctx)
		return err
	})); err != nil {
		return providers.RenderResult{}, fmt.Errorf("打开页面失败: %w", err)
	}

	// 按 WaitUntil 等待页面生命周期事件。
	if err := waitLifecycle(ctx, req.WaitUntil, domFired, loadFired, timeout); err != nil {
		return providers.RenderResult{}, err
	}

	// 显式等待选择器出现后再取快照。
	if sel := strings.TrimSpace(req.WaitSelector); sel != "" {
		if err := waitSelector(ctx, sel, timeout); err != nil {
			return providers.RenderResult{}, fmt.Errorf("等待选择器 %q 超时: %w", sel, err)
		}
	}

	html, err := outerHTML(ctx)
	if err != nil {
		return providers.RenderResult{}, fmt.Errorf("读取页面 HTML 失败: %w", err)
	}
	// Cloudflare / DDoS-GUARD 挑战页启发式：仍是挑战页则轮询等待挑战清除。
	if underChallenge(html) {
		html = waitForChallengeCleared(ctx, timeout, html)
	}

	finalURL := req.URL
	if href, err := locationHref(ctx); err == nil && href != "" {
		finalURL = href
	}

	outCookies, err := collectCookies(ctx)
	if err != nil {
		return providers.RenderResult{}, fmt.Errorf("读取渲染后 cookie 失败: %w", err)
	}

	return providers.RenderResult{
		HTML:     html,
		FinalURL: finalURL,
		Status:   status,
		Cookies:  outCookies,
	}, nil
}

// waitLifecycle 按 WaitUntil 语义等待页面事件。
func waitLifecycle(ctx context.Context, waitUntil string, domFired, loadFired chan struct{}, timeout time.Duration) error {
	switch normalizeWaitUntil(waitUntil) {
	case waitCommit:
		// 导航发起即视为满足，不等事件。
		return nil
	case waitLoad:
		return waitSignal(ctx, loadFired, timeout, "load")
	case waitNetworkIdle:
		// 轻量引擎无稳定 networkidle 语义，用 load + 短暂静默近似。
		if err := waitSignal(ctx, loadFired, timeout, "load"); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
		return nil
	default: // domcontentloaded
		return waitSignal(ctx, domFired, timeout, "domcontentloaded")
	}
}

func waitSignal(ctx context.Context, ch chan struct{}, timeout time.Duration, name string) error {
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("等待 %s 事件被取消: %w", name, ctx.Err())
	case <-time.After(timeout):
		return fmt.Errorf("等待 %s 事件超时", name)
	}
}

// waitSelector 轮询 querySelector 直到元素出现或超时。
// 不用 chromedp.WaitVisible（其依赖 DOM.getDocument + 节点可见性判断，
// 轻量引擎兼容性弱），改用 Runtime.evaluate 轮询，兼容性最好。
func waitSelector(ctx context.Context, selector string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	expr := fmt.Sprintf("document.querySelector(%s) !== null", jsString(selector))
	for time.Now().Before(deadline) {
		var found bool
		if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &found)); err != nil {
			return err
		}
		if found {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return fmt.Errorf("超时")
}

// waitForChallengeCleared 轮询等待挑战页消失，返回最新 HTML。
func waitForChallengeCleared(ctx context.Context, timeout time.Duration, lastHTML string) string {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return lastHTML
		case <-time.After(1500 * time.Millisecond):
		}
		html, err := outerHTML(ctx)
		if err != nil {
			continue
		}
		lastHTML = html
		if !underChallenge(html) {
			return html
		}
	}
	return lastHTML
}

func outerHTML(ctx context.Context) (string, error) {
	var html string
	err := chromedp.Run(ctx, chromedp.Evaluate(`document.documentElement.outerHTML`, &html))
	return html, err
}

func locationHref(ctx context.Context) (string, error) {
	var href string
	err := chromedp.Run(ctx, chromedp.Evaluate(`location.href`, &href))
	return href, err
}

// collectCookies 读取渲染后的全量 cookie，优先 Storage.getCookies，
// 失败再退回 Network.getCookies，转换为 SDK 契约类型。
func collectCookies(ctx context.Context) ([]providers.HTTPCookie, error) {
	var raw []*network.Cookie
	err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		cookies, e := storage.GetCookies().Do(ctx)
		if e != nil {
			cookies, e = network.GetCookies().Do(ctx)
		}
		raw = cookies
		return e
	}))
	if err != nil {
		return nil, err
	}
	out := make([]providers.HTTPCookie, 0, len(raw))
	for _, c := range raw {
		cookie := providers.HTTPCookie{
			Name:     c.Name,
			Value:    c.Value,
			Domain:   c.Domain,
			Path:     c.Path,
			Secure:   c.Secure,
			HTTPOnly: c.HTTPOnly,
		}
		if c.Expires > 0 {
			cookie.Expires = time.Unix(int64(c.Expires), 0).UTC()
		}
		out = append(out, cookie)
	}
	return out, nil
}

// cookieActions 把 SDK cookie 转换为 Network.setCookie 动作。
// 缺少 Domain 的 cookie 用请求 URL 兜底。
func cookieActions(cookies []providers.HTTPCookie, pageURL string) []chromedp.Action {
	if len(cookies) == 0 {
		return nil
	}
	actions := make([]chromedp.Action, 0, len(cookies))
	for _, c := range cookies {
		param := buildSetCookieParam(c, pageURL)
		name := c.Name
		actions = append(actions, chromedp.ActionFunc(func(ctx context.Context) error {
			if err := param.Do(ctx); err != nil {
				return fmt.Errorf("注入 cookie %q 失败: %w", name, err)
			}
			return nil
		}))
	}
	return actions
}

// buildSetCookieParam 把单个 SDK cookie 转成 Network.setCookie 参数（纯函数，便于单测）。
func buildSetCookieParam(c providers.HTTPCookie, pageURL string) *network.SetCookieParams {
	param := network.SetCookie(c.Name, c.Value)
	if strings.TrimSpace(c.Domain) != "" {
		param = param.WithDomain(c.Domain)
		path := c.Path
		if path == "" {
			path = "/"
		}
		param = param.WithPath(path)
	} else {
		param = param.WithURL(pageURL)
	}
	if c.Secure {
		param = param.WithSecure(true)
	}
	if c.HTTPOnly {
		param = param.WithHTTPOnly(true)
	}
	if !c.Expires.IsZero() {
		exp := cdp.TimeSinceEpoch(c.Expires)
		param = param.WithExpires(&exp)
	}
	return param
}

func trySignal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// waitUntilState 是内部归一化后的等待语义。
type waitUntilState int

const (
	waitDomContentLoaded waitUntilState = iota
	waitLoad
	waitNetworkIdle
	waitCommit
)

// normalizeWaitUntil 把 SDK 的 WaitUntil 字段映射为内部枚举，默认 domcontentloaded。
func normalizeWaitUntil(value string) waitUntilState {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "load":
		return waitLoad
	case "networkidle":
		return waitNetworkIdle
	case "commit":
		return waitCommit
	case "domcontentloaded", "":
		return waitDomContentLoaded
	default:
		return waitDomContentLoaded
	}
}

// jsString 把字符串安全地转成 JS 字面量（用于 querySelector 表达式）。
func jsString(s string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `'`, `\'`, "\n", `\n`, "\r", `\r`)
	return "'" + replacer.Replace(s) + "'"
}

func firstPositive(values ...int) int {
	for _, v := range values {
		if v > 0 {
			return v
		}
	}
	return 0
}
