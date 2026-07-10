package main

import (
	_ "embed"

	"github.com/chenbstack/media-agent-plugin-sdk-go"
)

//go:embed plugin.yaml
var manifestYAML []byte

//go:embed config.schema.json
var schemaJSON []byte

func Plugin() pluginsdk.Plugin {
	return pluginsdk.Plugin{
		Manifest:         pluginsdk.MustParseManifest(manifestYAML),
		ConfigSchema:     pluginsdk.MustParseConfigSchema(schemaJSON),
		NewActionHandler: newActionHandler,
		ValidateConfig:   validateConfig,
	}
}
