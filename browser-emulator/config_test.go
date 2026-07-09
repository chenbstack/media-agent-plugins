package main

import "testing"

func TestParseConfigDefaults(t *testing.T) {
	cfg := parseConfig(nil)
	if cfg.Backend != BackendLightweight {
		t.Fatalf("默认后端应为 lightweight，得到 %s", cfg.Backend)
	}
	if cfg.LightweightEngine != EngineLightpanda {
		t.Fatalf("默认引擎应为 lightpanda，得到 %s", cfg.LightweightEngine)
	}
	if cfg.DefaultTimeoutSeconds != 45 {
		t.Fatalf("默认超时应为 45，得到 %d", cfg.DefaultTimeoutSeconds)
	}
}

func TestParseConfigOverrides(t *testing.T) {
	cfg := parseConfig(map[string]any{
		"backend":                 "chromium",
		"proxy_url":               "http://127.0.0.1:7890",
		"default_timeout_seconds": float64(60),
	})
	if cfg.Backend != BackendChromium {
		t.Fatalf("backend = %s", cfg.Backend)
	}
	if cfg.ProxyURL != "http://127.0.0.1:7890" {
		t.Fatalf("proxy = %s", cfg.ProxyURL)
	}
	if cfg.DefaultTimeoutSeconds != 60 {
		t.Fatalf("timeout = %d", cfg.DefaultTimeoutSeconds)
	}
}

func TestParseConfigIntStrings(t *testing.T) {
	cfg := parseConfig(map[string]any{
		"default_timeout_seconds": "30",
	})
	if cfg.DefaultTimeoutSeconds != 30 {
		t.Fatalf("字符串 '30' 应解析为 30，得到 %d", cfg.DefaultTimeoutSeconds)
	}
}

func TestValidateConfig(t *testing.T) {
	if err := validateConfig(map[string]any{"backend": "lightweight"}); err != nil {
		t.Fatalf("合法配置不应报错: %v", err)
	}
	if err := validateConfig(map[string]any{"backend": "invalid"}); err == nil {
		t.Fatalf("非法 backend 应报错")
	}
	if err := validateConfig(map[string]any{"backend": "chromium", "proxy_url": "127.0.0.1:7890"}); err == nil {
		t.Fatalf("缺少协议的代理地址应报错")
	}
}

func TestLightweightServeArgs(t *testing.T) {
	lp := lightweightServeArgs(Config{LightweightEngine: EngineLightpanda}, 9222)
	if lp[0] != "serve" || !contains(lp, "9222") || !contains(lp, "127.0.0.1") {
		t.Fatalf("lightpanda args 异常: %v", lp)
	}
	// Lightpanda 支持 CDP 层 UA 覆盖，不应把 UA 塞进启动参数。
	if contains(lp, "--user-agent") {
		t.Fatalf("lightpanda 不应带 --user-agent: %v", lp)
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
