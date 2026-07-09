package main

import "media-agent-lab/server/pkg/pluginsdk/pluginrpc"

func main() {
	pluginrpc.Serve(Plugin())
}
