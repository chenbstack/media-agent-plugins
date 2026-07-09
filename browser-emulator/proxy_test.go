package main

import "testing"

func TestGithubProxied(t *testing.T) {
	const gh = "https://github.com/lightpanda-io/browser/releases/download/nightly/lightpanda-aarch64-macos"
	const ghUserContent = "https://objects.githubusercontent.com/foo/bar"
	const cloak = "https://cloakbrowser.dev/releases/cloakbrowser-1.0.tar.gz"

	// 未设置加速代理：原样返回。
	t.Setenv(envGitHubProxy, "")
	if got := githubProxied(gh); got != gh {
		t.Fatalf("未设置代理应原样返回，实际 %q", got)
	}

	// 设置加速代理：github 主机前缀改写，尾部斜杠归一。
	t.Setenv(envGitHubProxy, "https://gh-proxy.example.com/")
	if got := githubProxied(gh); got != "https://gh-proxy.example.com/"+gh {
		t.Fatalf("github URL 应前缀改写，实际 %q", got)
	}
	if got := githubProxied(ghUserContent); got != "https://gh-proxy.example.com/"+ghUserContent {
		t.Fatalf("githubusercontent URL 应前缀改写，实际 %q", got)
	}
	// 非 github 主机（cloakbrowser.dev 主源）不改写。
	if got := githubProxied(cloak); got != cloak {
		t.Fatalf("非 github 主机不应改写，实际 %q", got)
	}

	// 前缀不带尾斜杠也应正确拼接。
	t.Setenv(envGitHubProxy, "https://gh-proxy.example.com")
	if got := githubProxied(gh); got != "https://gh-proxy.example.com/"+gh {
		t.Fatalf("无尾斜杠前缀应补斜杠，实际 %q", got)
	}
}
