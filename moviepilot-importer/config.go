package main

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/chenbstack/media-agent-plugin-sdk-go"
)

type config struct {
	BaseURL   string
	Username  string
	Password  string
	PageLimit int
	Sources   []string
}

var sourceConfigFields = []struct {
	Type  string
	Field string
}{
	{"sites", "include_sites"},
	{"subscriptions", "include_subscriptions"},
	{"subscribe_history", "include_subscribe_history"},
	{"transfer_history", "include_transfer_history"},
}

func parseConfig(values map[string]any) config {
	out := config{
		BaseURL:   normalizeBaseURL(stringValue(values, "base_url")),
		Username:  stringValue(values, "username"),
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
	if cfg.Username == "" {
		errs["username"] = "请输入 MoviePilot 用户名"
	}
	if cfg.PageLimit < 1 || cfg.PageLimit > 1000 {
		errs["page_limit"] = "分页大小必须在 1 到 1000 之间"
	}
	if len(errs) > 0 {
		return &pluginsdk.ValidationError{Fields: errs}
	}
	return nil
}

func configWithSecret(values map[string]any, password string) (config, error) {
	cfg := parseConfig(values)
	cfg.Password = strings.TrimSpace(password)
	if cfg.Password == "" {
		return config{}, fmt.Errorf("MoviePilot 密码未配置")
	}
	return cfg, nil
}

func normalizeBaseURL(value string) string {
	value = strings.TrimRight(strings.TrimSpace(value), "/")
	value = strings.TrimSuffix(value, "/api/v1")
	return strings.TrimRight(value, "/")
}
