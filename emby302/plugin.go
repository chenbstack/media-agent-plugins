package main

import (
	"context"
	_ "embed"
	"fmt"
	"net/url"
	"strings"

	pluginsdk "github.com/chenbstack/media-agent-plugin-sdk-go"
)

//go:embed plugin.yaml
var manifestYAML []byte

//go:embed config.schema.json
var schemaJSON []byte

//go:embed icon.svg
var iconSVG []byte

func Plugin() pluginsdk.Plugin {
	return pluginsdk.Plugin{
		Manifest:       pluginsdk.MustParseManifest(manifestYAML),
		ConfigSchema:   pluginsdk.MustParseConfigSchema(schemaJSON),
		IconSVG:        iconSVG,
		ValidateConfig: validateConfig,
		NewHTTPService: newHTTPService,
	}
}

func validateConfig(config map[string]any) error {
	errs := map[string]string{}
	if strings.TrimSpace(stringConfig(config, "emby_connection_id")) == "" {
		errs["emby_connection_id"] = "请选择 Emby 连接"
	}
	cacheSeconds := intConfig(config, "redirect_cache_seconds", 60)
	if cacheSeconds < 5 || cacheSeconds > 600 {
		errs["redirect_cache_seconds"] = "缓存时间必须在 5 到 600 秒之间"
	}
	if len(errs) != 0 {
		return &pluginsdk.ValidationError{Fields: errs}
	}
	return nil
}

func newHTTPService(ctx context.Context, inst pluginsdk.Instance, _ pluginsdk.SecretResolver, name string) (pluginsdk.HTTPService, error) {
	if name != "emby" {
		return nil, fmt.Errorf("未知 HTTP 服务 %q", name)
	}
	if inst.Connections == nil {
		return nil, fmt.Errorf("宿主未提供连接读取能力")
	}
	connectionID := strings.TrimSpace(stringConfig(inst.Config, "emby_connection_id"))
	connection, err := inst.Connections.GetConnection(ctx, "media_servers", connectionID)
	if err != nil {
		return nil, fmt.Errorf("读取 Emby 连接: %w", err)
	}
	if !connection.Enabled || !strings.EqualFold(connection.Kind, "emby") {
		return nil, fmt.Errorf("所选连接不是已启用的 Emby 连接")
	}
	baseURL := strings.TrimRight(strings.TrimSpace(stringValue(connection.Config["base_url"])), "/")
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Hostname() == "" || parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("Emby 连接地址无效")
	}
	verifyTLS := true
	if value, ok := connection.Config["verify_tls"].(bool); ok {
		verifyTLS = value
	}
	return newProxyService(parsed, verifyTLS, intConfig(inst.Config, "redirect_cache_seconds", 60), inst.Logger), nil
}

func stringConfig(config map[string]any, key string) string {
	if config == nil {
		return ""
	}
	return stringValue(config[key])
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func intConfig(config map[string]any, key string, fallback int) int {
	switch value := config[key].(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(value)
	default:
		return fallback
	}
}
