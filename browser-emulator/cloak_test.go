package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildCloakArchive 现造一个 linux 布局的小 tar.gz：cloak-chromium-<version>/chrome（可执行位）
// 外加一个符号链接与一个越界条目，用于验证权限保留、符号链接处理与目录穿越防护。
// 返回归档字节及其十六进制 SHA256。
func buildCloakArchive(t *testing.T, version string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	root := "cloak-chromium-" + version

	writeEntry := func(hdr *tar.Header, body []byte) {
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("写 tar header %s: %v", hdr.Name, err)
		}
		if len(body) > 0 {
			if _, err := tw.Write(body); err != nil {
				t.Fatalf("写 tar body %s: %v", hdr.Name, err)
			}
		}
	}

	// 目录条目。
	writeEntry(&tar.Header{Name: root + "/", Typeflag: tar.TypeDir, Mode: 0o755}, nil)
	// 可执行文件 chrome。
	chromeBody := []byte("#!/bin/sh\necho cloak\n")
	writeEntry(&tar.Header{
		Name:     root + "/chrome",
		Typeflag: tar.TypeReg,
		Mode:     0o755,
		Size:     int64(len(chromeBody)),
	}, chromeBody)
	// 普通资源文件（非可执行）。
	resBody := []byte("resource-bytes")
	writeEntry(&tar.Header{
		Name:     root + "/resources.pak",
		Typeflag: tar.TypeReg,
		Mode:     0o644,
		Size:     int64(len(resBody)),
	}, resBody)
	// 符号链接（模拟 .app 内布局）。
	writeEntry(&tar.Header{
		Name:     root + "/chrome-link",
		Typeflag: tar.TypeSymlink,
		Linkname: "chrome",
		Mode:     0o777,
	}, nil)
	// 目录穿越条目：应被跳过，不得写到 destDir 之外。
	evilBody := []byte("evil")
	writeEntry(&tar.Header{
		Name:     "../evil.txt",
		Typeflag: tar.TypeReg,
		Mode:     0o644,
		Size:     int64(len(evilBody)),
	}, evilBody)

	if err := tw.Close(); err != nil {
		t.Fatalf("关闭 tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("关闭 gzip: %v", err)
	}
	data := buf.Bytes()
	sum := sha256.Sum256(data)
	return data, hex.EncodeToString(sum[:])
}

// cloakTestServer 起一个 httptest server 提供归档与 SHA256SUMS，并把主源/兜底源都指向它。
// sumsHash 为 SHA256SUMS 中登记的 hash（传入 realHash 即正确，传入别的即模拟不匹配）。
// serveSums=false 时不提供 SHA256SUMS（返回 404），模拟“取不到、跳过校验”。
func cloakTestServer(t *testing.T, version, tag string, archive []byte, sumsHash string, serveSums bool) {
	t.Helper()
	archiveName := cloakArchiveName(tag)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/SHA256SUMS"):
			if !serveSums {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			fmt.Fprintf(w, "%s  %s\n", sumsHash, archiveName)
		case strings.HasSuffix(r.URL.Path, archiveName):
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(archive)))
			_, _ = w.Write(archive)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	oldPrimary, oldGithub := cloakPrimaryBase, cloakGithubBase
	cloakPrimaryBase = srv.URL
	cloakGithubBase = srv.URL
	t.Cleanup(func() {
		cloakPrimaryBase = oldPrimary
		cloakGithubBase = oldGithub
	})
}

func TestCloakDownloadToDir_Success(t *testing.T) {
	const version = "1.2.3.4"
	const tag = "linux-x64"
	archive, hash := buildCloakArchive(t, version)
	cloakTestServer(t, version, tag, archive, hash, true)

	dir := t.TempDir()
	exec, err := downloadCloakToDir(context.Background(), dir, version, tag)
	if err != nil {
		t.Fatalf("下载/解压/校验失败: %v", err)
	}
	want := filepath.Join(dir, "cloak-chromium-"+version, "chrome")
	if exec != want {
		t.Fatalf("exec 路径应为 %s，实际 %s", want, exec)
	}
	if !isExecutable(exec) {
		t.Fatalf("chrome 应可执行: %s", exec)
	}
	// 符号链接应正确建立。
	link := filepath.Join(dir, "cloak-chromium-"+version, "chrome-link")
	if target, err := os.Readlink(link); err != nil || target != "chrome" {
		t.Fatalf("符号链接应指向 chrome，得到 %q err=%v", target, err)
	}
	// 目录穿越条目应被跳过，destDir 之外不得出现 evil.txt。
	if _, err := os.Stat(filepath.Join(dir, "..", "evil.txt")); err == nil {
		t.Fatalf("目录穿越条目不应被写出")
	}
	// 归档临时文件应已清理。
	if _, err := os.Stat(filepath.Join(dir, cloakArchiveName(tag)+".part")); !os.IsNotExist(err) {
		t.Fatalf(".part 临时归档应被清理")
	}
}

func TestCloakDownloadToDir_SkipVerifyWhenNoSums(t *testing.T) {
	const version = "1.2.3.4"
	const tag = "linux-x64"
	archive, _ := buildCloakArchive(t, version)
	cloakTestServer(t, version, tag, archive, "", false) // 不提供 SHA256SUMS

	dir := t.TempDir()
	if _, err := downloadCloakToDir(context.Background(), dir, version, tag); err != nil {
		t.Fatalf("取不到 SHA256SUMS 时应跳过校验并成功: %v", err)
	}
	if !isExecutable(filepath.Join(dir, "cloak-chromium-"+version, "chrome")) {
		t.Fatalf("跳过校验后仍应正常解压出可执行文件")
	}
}

func TestCloakDownloadToDir_SHAMismatch(t *testing.T) {
	const version = "1.2.3.4"
	const tag = "linux-x64"
	archive, _ := buildCloakArchive(t, version)
	// 登记一个错误 hash：应报错且不留下可执行文件。
	cloakTestServer(t, version, tag, archive, strings.Repeat("0", 64), true)

	dir := t.TempDir()
	_, err := downloadCloakToDir(context.Background(), dir, version, tag)
	if err == nil {
		t.Fatalf("SHA256 不匹配应报错")
	}
	if !strings.Contains(err.Error(), "SHA256") {
		t.Fatalf("错误应说明校验失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "cloak-chromium-"+version, "chrome")); err == nil {
		t.Fatalf("校验失败不应留下可执行文件")
	}
	if _, err := os.Stat(filepath.Join(dir, cloakArchiveName(tag)+".part")); !os.IsNotExist(err) {
		t.Fatalf("校验失败应清理 .part 归档")
	}
}

func TestFindInstalledCloak(t *testing.T) {
	version, err := cloakVersion()
	if err != nil {
		t.Skipf("当前平台不支持 CloakBrowser: %v", err)
	}
	dir := t.TempDir()
	if _, ok := findInstalledCloak([]string{dir}); ok {
		t.Fatalf("空目录不应探测到 CloakBrowser")
	}

	exec, err := cloakExecPath(dir, version)
	if err != nil {
		t.Fatalf("解析 exec 路径: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(exec), 0o755); err != nil {
		t.Fatalf("建目录: %v", err)
	}
	if err := os.WriteFile(exec, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("写占位可执行文件: %v", err)
	}
	p, ok := findInstalledCloak([]string{dir})
	if !ok || p != exec {
		t.Fatalf("应探测到 %s，得到 %q ok=%v", exec, p, ok)
	}
}

func TestRemoveCloak_Idempotent(t *testing.T) {
	version, err := cloakVersion()
	if err != nil {
		t.Skipf("当前平台不支持 CloakBrowser: %v", err)
	}
	dir := t.TempDir()
	// 布置一个 cloak-chromium-<version> 目录与一个残留 .part。
	cloakDir := filepath.Join(dir, "cloak-chromium-"+version)
	if err := os.MkdirAll(cloakDir, 0o755); err != nil {
		t.Fatalf("建 cloak 目录: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cloakDir, "chrome"), []byte("x"), 0o755); err != nil {
		t.Fatalf("写 chrome: %v", err)
	}
	part := filepath.Join(dir, "cloakbrowser-linux-x64.tar.gz.part")
	if err := os.WriteFile(part, []byte("y"), 0o644); err != nil {
		t.Fatalf("写 .part: %v", err)
	}

	res, err := removeCloak([]string{dir})
	if err != nil {
		t.Fatalf("卸载失败: %v", err)
	}
	if !res.Removed {
		t.Fatalf("应删除资源，得到 %+v", res)
	}
	if _, err := os.Stat(cloakDir); !os.IsNotExist(err) {
		t.Fatalf("cloak 目录应被删除")
	}
	if _, err := os.Stat(part); !os.IsNotExist(err) {
		t.Fatalf(".part 应被删除")
	}

	// 幂等：再次卸载无资源可删。
	res2, err := removeCloak([]string{dir})
	if err != nil {
		t.Fatalf("二次卸载失败: %v", err)
	}
	if res2.Removed {
		t.Fatalf("二次应 Removed=false，得到 %+v", res2)
	}
}

func TestUninstallCloak_EmptyIdempotent(t *testing.T) {
	// 用一个空临时目录当唯一候选，避免碰真实缓存；这里直接测 removeCloak 的幂等语义。
	res, err := removeCloak([]string{t.TempDir()})
	if err != nil {
		t.Fatalf("卸载失败: %v", err)
	}
	if res.Removed {
		t.Fatalf("空目录应 Removed=false，得到 %+v", res)
	}
}

func TestCloakStealthArgs(t *testing.T) {
	args := cloakStealthArgs()
	joined := strings.Join(args, " ")
	if !containsArg(args, "--no-sandbox") {
		t.Fatalf("应含 --no-sandbox，实际 %v", args)
	}
	if !hasArgPrefix(args, "--fingerprint=") {
		t.Fatalf("应含 --fingerprint=<n>，实际 %v", args)
	}
	if !hasArgPrefix(args, "--fingerprint-platform=") {
		t.Fatalf("应含 --fingerprint-platform=，实际 %v", args)
	}
	_ = joined
}

func TestResolveCloakBinary(t *testing.T) {
	// 未安装应报错并给出安装提示。
	if _, err := resolveCloakBinary([]string{t.TempDir()}); err == nil || !strings.Contains(err.Error(), "请在插件详情页点安装") {
		t.Fatalf("未安装应报安装提示，得到 %v", err)
	}

	// 已安装则命中：在候选目录里构造当前平台期望的可执行路径。
	spec, err := cloakPlatformSpecFor()
	if err != nil {
		t.Skipf("当前平台无 CloakBrowser 规格，跳过: %v", err)
	}
	dir := t.TempDir()
	execPath := cloakExecPathForTag(dir, spec.version, spec.tag)
	if err := os.MkdirAll(filepath.Dir(execPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(execPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("写占位: %v", err)
	}
	got, err := resolveCloakBinary([]string{dir})
	if err != nil || got != execPath {
		t.Fatalf("已安装应命中 %s，得到 %q err=%v", execPath, got, err)
	}
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func hasArgPrefix(args []string, prefix string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, prefix) && len(a) > len(prefix) {
			return true
		}
	}
	return false
}
