package main

import "github.com/chenbstack/media-agent-plugin-sdk-go/pluginrpc"

func main() {
	pluginrpc.Serve(Plugin())
}
