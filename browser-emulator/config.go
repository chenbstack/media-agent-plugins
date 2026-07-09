package main

import (
	"strconv"
	"strings"
)

// Backend 是浏览器渲染后端类型。
type Backend string

const (
	// BackendLightweight 通过 CDP 接入轻量浏览器（Lightpanda），
	// 内存和启动成本远低于 Chromium，作为默认后端。
	BackendLightweight Backend = "lightweight"
	// BackendChromium 由 chromedp 启动隐身 Chromium（CloakBrowser），
	// 作为轻量后端无法通过（如 Cloudflare 人机验证）时的可选兜底。
	BackendChromium Backend = "chromium"
)

// LightweightEngine 是轻量后端的具体引擎。当前仅支持 Lightpanda，
// 保留此类型作为引擎标识（文件名 / 路径解析判别符）。
type LightweightEngine string

const (
	EngineLightpanda LightweightEngine = "lightpanda"
)

// Config 是本插件的运行配置，来自宿主按 config.schema.json 校验后的实例配置。
//
// 只暴露三个面向用户的选项：渲染后端、代理、超时。引擎二进制一律由详情页安装组件
// （启用时自动预装 Lightpanda / 手动安装 CloakBrowser）就位，无头运行固定为真，
// 故不再提供二进制路径 / 外部 CDP 端点 / 无头开关 / 自动下载开关等高级旋钮。
type Config struct {
	Backend               Backend
	LightweightEngine     LightweightEngine // 内部固定为 Lightpanda，作文件名 / 路径解析判别符
	ProxyURL              string
	DefaultTimeoutSeconds int
}

func parseConfig(raw map[string]any) Config {
	cfg := Config{
		Backend:               BackendLightweight,
		LightweightEngine:     EngineLightpanda,
		DefaultTimeoutSeconds: 45,
	}
	if raw == nil {
		return cfg
	}
	if v := strings.TrimSpace(stringConfig(raw["backend"])); v != "" {
		cfg.Backend = Backend(v)
	}
	cfg.ProxyURL = strings.TrimSpace(stringConfig(raw["proxy_url"]))
	if v := intConfig(raw["default_timeout_seconds"]); v > 0 {
		cfg.DefaultTimeoutSeconds = v
	}
	return cfg
}

func stringConfig(value any) string {
	str, _ := value.(string)
	return str
}

func intConfig(value any) int {
	switch v := value.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0
		}
		return n
	default:
		return 0
	}
}
