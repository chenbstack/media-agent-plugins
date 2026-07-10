package main

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/chenbstack/media-agent-plugin-sdk-go"
)

type config struct {
	BaseURL     string
	BridgeToken string
	PageLimit   int
	Sources     []string
}

var sourceConfigFields = []struct {
	Type  string
	Field string
}{
	{"system_settings", "include_system_settings"},
	{"plugin_configs", "include_plugin_configs"},
	{"plugin_data", "include_plugin_data"},
	{"sites", "include_sites"},
	{"subscriptions", "include_subscriptions"},
	{"subscribe_history", "include_subscribe_history"},
	{"transfer_history", "include_transfer_history"},
}

func parseConfig(values map[string]any) config {
	out := config{
		BaseURL:   strings.TrimRight(stringValue(values, "base_url"), "/"),
		PageLimit: intValue(values, "page_limit"),
	}
	if out.PageLimit <= 0 {
		out.PageLimit = 500
	}
	if out.PageLimit > 1000 {
		out.PageLimit = 1000
	}
	for _, source := range sourceConfigFields {
		if boolValue(values, source.Field, true) {
			out.Sources = append(out.Sources, source.Type)
		}
	}
	return out
}

func validateConfig(values map[string]any) error {
	cfg := parseConfig(values)
	errs := map[string]string{}
	parsed, err := url.Parse(cfg.BaseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		errs["base_url"] = "请输入有效的 MoviePilot 地址"
	}
	if cfg.PageLimit < 1 || cfg.PageLimit > 1000 {
		errs["page_limit"] = "分页大小必须在 1 到 1000 之间"
	}
	if len(errs) > 0 {
		return &pluginsdk.ValidationError{Fields: errs}
	}
	return nil
}

func configWithSecret(values map[string]any, token string) (config, error) {
	cfg := parseConfig(values)
	cfg.BridgeToken = strings.TrimSpace(token)
	if cfg.BridgeToken == "" {
		return config{}, fmt.Errorf("迁移桥 Token 未配置")
	}
	return cfg, nil
}
