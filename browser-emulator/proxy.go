package main

import (
	"net/http"
	"net/url"
	"os"
	"strings"
)

// envGitHubProxy 是宿主注入的 GitHub 加速代理前缀环境变量名（与宿主 plugins.EnvGitHubProxy 约定一致）。
// 宿主只在安装（引擎下载）子进程注入；宿主未启用时该变量为空，下载走原始 URL。
const envGitHubProxy = "MEDIA_AGENT_GITHUB_PROXY"

// proxyFromEnvTransport 返回一个走系统环境代理的 Transport。宿主按全局「代理服务器」设置
// 把 HTTP(S)_PROXY 注入到安装子进程，这里用 http.ProxyFromEnvironment 即可让所有下载源
// （含 cloakbrowser.dev）走该代理；未注入时为直连。
func proxyFromEnvTransport() *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = http.ProxyFromEnvironment
	return tr
}

// githubProxied 在设置了 GitHub 加速代理时，对 github.com / *.githubusercontent.com 的下载 URL
// 做前缀改写，形如 https://gh-proxy.example.com/https://github.com/...；非 github 主机
// （如 cloakbrowser.dev 主源）原样返回。加速代理是 URL 前缀镜像，不是 HTTP 代理，故只对 github 生效。
func githubProxied(raw string) string {
	prefix := strings.TrimSpace(os.Getenv(envGitHubProxy))
	if prefix == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	host := strings.ToLower(u.Hostname())
	if host != "github.com" && !strings.HasSuffix(host, ".githubusercontent.com") {
		return raw
	}
	return strings.TrimRight(prefix, "/") + "/" + raw
}
