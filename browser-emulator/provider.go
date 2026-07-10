package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/chromedp/chromedp"

	"github.com/chenbstack/media-agent-plugin-sdk-go"
	"github.com/chenbstack/media-agent-plugin-sdk-go/providers"
	runtimesdk "github.com/chenbstack/media-agent-plugin-sdk-go/runtime"
)

// renderer 是本插件的 RendererProvider 实现。
// 它按配置选择后端：默认 lightweight（CDP 接入 Lightpanda），
// 可选 chromium（chromedp 启动隐身 Chromium/CloakBrowser 兜底）。
//
// 全程用纯 Go 的 chromedp 走 CDP，不依赖 node/playwright 驱动进程，
// 默认路径进程树只有：本插件 Go 进程 + 轻量引擎子进程。
//
// 进程生命周期说明：宿主对 renderer 类操作是“一次调用拉起一次插件进程、
// 用完即 kill”（见主仓 pluginrpc.ExternalPlugin.withClientOperation），
// 插件进程短命，无需跨请求维持浏览器复用。
type renderer struct {
	cfg    Config
	logger runtimesdk.Feedback
}

func newRenderer(ctx context.Context, inst pluginsdk.Instance, secrets pluginsdk.SecretResolver) (providers.RendererProvider, error) {
	_ = secrets
	if inst.Runtime == nil || inst.Runtime.Feedback == nil {
		return nil, fmt.Errorf("宿主未提供插件 Runtime Feedback")
	}
	return &renderer{
		cfg:    parseConfig(inst.Config),
		logger: inst.Runtime.Feedback,
	}, nil
}

func (r *renderer) Kind() string { return "browser-emulator" }

// TestConnection 渲染一个 data: 页面验证后端可启动。
func (r *renderer) TestConnection(ctx context.Context) error {
	const probeURL = "data:text/html,<html><head><title>ok</title></head><body><span id=probe class=rendered>ok</span></body></html>"
	res, err := r.Render(ctx, providers.RenderRequest{
		URL:            probeURL,
		WaitUntil:      "load",
		WaitSelector:   "#probe",
		TimeoutSeconds: firstPositive(r.cfg.DefaultTimeoutSeconds, 30),
	})
	if err != nil {
		return fmt.Errorf("浏览器后端自检失败（backend=%s）: %w", r.cfg.Backend, err)
	}
	if !strings.Contains(res.HTML, "probe") {
		return fmt.Errorf("浏览器后端自检返回内容异常，未取到探针节点")
	}
	return nil
}

// Render 执行一次页面渲染。
func (r *renderer) Render(ctx context.Context, req providers.RenderRequest) (providers.RenderResult, error) {
	switch r.cfg.Backend {
	case BackendChromium:
		return r.renderChromium(ctx, req)
	case BackendLightweight, "":
		return r.renderLightweight(ctx, req)
	default:
		return providers.RenderResult{}, fmt.Errorf("未知渲染后端: %s", r.cfg.Backend)
	}
}

// renderLightweight 通过 CDP 接入轻量浏览器 Lightpanda：本进程内懒启动引擎子进程，
// 渲染完即回收。
func (r *renderer) renderLightweight(ctx context.Context, req providers.RenderRequest) (providers.RenderResult, error) {
	endpoint, cleanup, err := r.startLightweightEngine(ctx)
	if err != nil {
		return providers.RenderResult{}, err
	}
	defer cleanup()

	allocCtx, cancel := chromedp.NewRemoteAllocator(ctx, endpoint)
	defer cancel()
	return renderWithCDP(allocCtx, r.cfg, req)
}

// renderChromium 用 CloakBrowser（基于 Chromium 源码、带源码级指纹伪装的隐身浏览器，
// 能过 Cloudflare 人机验证）作为兜底后端：chromedp 直接启动其二进制并走 CDP 渲染。
//
// 二进制由用户在插件详情页手动安装（约 200MB，见 cloak.go），渲染时不自动下载——缺失即
// 给出可操作的报错，引导去安装。始终无头运行。
func (r *renderer) renderChromium(ctx context.Context, req providers.RenderRequest) (providers.RenderResult, error) {
	bin, err := resolveCloakBinary(engineDirs())
	if err != nil {
		return providers.RenderResult{}, err
	}

	// 从 chromedp 默认参数出发，但剔除 --enable-automation（它会让 navigator.webdriver=true，
	// 直接暴露自动化，破坏 CloakBrowser 的隐身），再按隐身要求追加指纹参数。默认参数已含无头。
	opts := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
	opts = append(opts, chromedp.Flag("enable-automation", false), chromedp.ExecPath(bin))
	for _, flag := range cloakStealthArgs() {
		name, value := parseChromiumFlag(flag)
		opts = append(opts, chromedp.Flag(name, value))
	}
	if r.cfg.ProxyURL != "" {
		opts = append(opts, chromedp.ProxyServer(r.cfg.ProxyURL))
	}
	allocCtx, cancel := chromedp.NewExecAllocator(ctx, opts...)
	defer cancel()
	return renderWithCDP(allocCtx, r.cfg, req)
}

// parseChromiumFlag 把 "--k=v" / "--k" 形式的命令行参数拆成 chromedp.Flag 的 (name, value)：
// 带 = 的返回字符串值，纯开关返回 bool true。
func parseChromiumFlag(flag string) (string, any) {
	flag = strings.TrimPrefix(flag, "--")
	if i := strings.IndexByte(flag, '='); i >= 0 {
		return flag[:i], flag[i+1:]
	}
	return flag, true
}

// startLightweightEngine 启动轻量引擎子进程并返回 CDP 端点与清理函数。
func (r *renderer) startLightweightEngine(ctx context.Context) (string, func(), error) {
	bin, err := r.resolveLightweightBinary(ctx)
	if err != nil {
		return "", nil, err
	}
	port, err := freePort()
	if err != nil {
		return "", nil, fmt.Errorf("分配 CDP 端口失败: %w", err)
	}

	args := lightweightServeArgs(r.cfg, port)
	cmd := exec.Command(bin, args...)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return "", nil, fmt.Errorf("启动轻量引擎 Lightpanda 失败: %w", err)
	}
	cleanup := func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	}

	endpoint := fmt.Sprintf("http://127.0.0.1:%d", port)
	if err := waitForCDP(ctx, endpoint, 10*time.Second); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("等待轻量引擎 CDP 就绪失败: %w", err)
	}
	return endpoint, cleanup, nil
}

// lightweightServeArgs 组装轻量引擎 Lightpanda 的 CDP serve 命令行参数。
func lightweightServeArgs(_ Config, port int) []string {
	return []string{"serve", "--host", "127.0.0.1", "--port", strconv.Itoa(port)}
}

// resolveLightweightBinary 查找轻量引擎二进制，缺失则运行时兜底下载。
// 具体解析顺序见 resolveEngineBinary（三候选目录 → PATH → 兜底下载）。
func (r *renderer) resolveLightweightBinary(ctx context.Context) (string, error) {
	return resolveEngineBinary(ctx, r.cfg.LightweightEngine, engineDirs())
}

// resolveEngineBinary 是与 renderer 解耦的纯解析逻辑（dirs 可注入以便单测）：
//
//	① 三候选目录中的 <engine>-<os>-<arch> 或裸 <engine>（见 engineDirs）
//	② PATH
//	③ 运行时兜底下载（默认引擎 Lightpanda 始终兜底，保证默认后端开箱即用）
//
// 引擎二进制通常已由详情页安装组件（启用时自动预装）就位，此处兜底只覆盖首次渲染早于
// 预装完成、或预装失败的边角情况。
func resolveEngineBinary(ctx context.Context, engine LightweightEngine, dirs []string) (string, error) {
	name := string(engine)
	fileName := engineFileName(engine)
	for _, dir := range dirs {
		if c := filepath.Join(dir, fileName); isExecutable(c) {
			return c, nil
		}
		if c := filepath.Join(dir, name); isExecutable(c) {
			return c, nil
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	if engine == EngineLightpanda {
		url, err := lightpandaDownloadURL()
		if err != nil {
			return "", err
		}
		dest, err := downloadToWritableDir(ctx, dirs, fileName, url)
		if err != nil {
			return "", fmt.Errorf("轻量引擎 %s 缺失且自动下载失败: %w", name, err)
		}
		return dest, nil
	}
	return "", fmt.Errorf("未找到轻量引擎 %s 二进制，请在插件详情页安装「轻量引擎」组件", name)
}

func isExecutable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	return info.Mode().Perm()&0o111 != 0
}
