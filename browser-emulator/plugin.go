package main

import (
	_ "embed"
	"fmt"
	"strings"

	"media-agent-lab/server/pkg/pluginsdk"
)

// 包目录中的 plugin.yaml 同时用于宿主扫描发现（server/internal/plugins/external.go
// 只读 plugin.yaml）和二进制内 go:embed 自描述，单一来源避免打包重命名导致的漂移。
//
//go:embed plugin.yaml
var manifestYAML []byte

//go:embed config.schema.json
var schemaJSON []byte

//go:embed icon.svg
var iconSVG []byte

// Plugin 构造本插件的 SDK 描述。
func Plugin() pluginsdk.Plugin {
	return pluginsdk.Plugin{
		Manifest:       pluginsdk.MustParseManifest(manifestYAML),
		ConfigSchema:   pluginsdk.MustParseConfigSchema(schemaJSON),
		IconSVG:        iconSVG,
		NewRenderer:    newRenderer,
		ValidateConfig: validateConfig,
		// lifecycle.install：本插件声明两个可独立安装的组件（见 plugin.yaml install.components）：
		//   lightpanda —— 默认轻量引擎（Lightpanda），几十 MB，auto_install=true，启用插件后自动预装；
		//   cloak      —— 隐身 Chromium（CloakBrowser），约 200MB，过 Cloudflare 人机验证的兜底，仅手动安装。
		// 三类钩子语义一致：Install 由宿主（auto_install 组件启用时自动 / 用户在详情页手动）触发并实时
		// 汇报进度；CheckInstall 在加载时只读探测是否就绪（不下载）；Uninstall 删除已下载资源、回收空间。
		InstallComponents: []pluginsdk.InstallComponent{
			{ID: "lightpanda", Install: installEngine, CheckInstall: checkEngineInstalled, Uninstall: uninstallEngine},
			{ID: "cloak", Install: installCloak, CheckInstall: checkCloakInstalled, Uninstall: uninstallCloak},
		},
	}
}

// validateConfig 在通用 schema 校验之后做后端相关的二次校验。
func validateConfig(config map[string]any) error {
	cfg := parseConfig(config)
	errs := map[string]string{}
	switch cfg.Backend {
	case BackendLightweight, BackendChromium:
	default:
		errs["backend"] = "后端必须是 lightweight 或 chromium"
	}
	if cfg.ProxyURL != "" && !strings.Contains(cfg.ProxyURL, "://") {
		errs["proxy_url"] = "代理地址必须包含协议，如 http://127.0.0.1:7890"
	}
	if len(errs) > 0 {
		return &pluginsdk.ValidationError{Fields: errs}
	}
	return nil
}

// 确保 embed 内容非空，避免构建产物缺资源。
func init() {
	if len(manifestYAML) == 0 || len(schemaJSON) == 0 {
		panic(fmt.Sprintf("插件资源缺失: manifest=%d schema=%d", len(manifestYAML), len(schemaJSON)))
	}
}
