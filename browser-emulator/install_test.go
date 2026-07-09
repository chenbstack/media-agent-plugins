package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeReleaseServer 返回一个模拟 Lightpanda release 的 httptest 服务器，
// 对任意路径返回固定的可执行占位内容，并统计命中次数。绝不触真实网络。
func fakeReleaseServer(t *testing.T, payload string) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// withReleaseBase 临时把下载根 URL 指向 httptest，测试结束后还原。
func withReleaseBase(t *testing.T, base string) {
	t.Helper()
	old := lightpandaReleaseBase
	lightpandaReleaseBase = base
	t.Cleanup(func() { lightpandaReleaseBase = old })
}

func TestInstallLightpanda_DownloadThenSkip(t *testing.T) {
	srv, hits := fakeReleaseServer(t, "#!/bin/sh\necho lightpanda\n")
	withReleaseBase(t, srv.URL)
	dir := t.TempDir()

	// 首次：真正下载。
	res, err := installLightpanda(context.Background(), []string{dir})
	if err != nil {
		t.Fatalf("首次安装失败: %v", err)
	}
	if !res.Installed {
		t.Fatalf("首次应 Installed=true，得到 %+v", res)
	}
	dest := filepath.Join(dir, engineFileName(EngineLightpanda))
	if !isExecutable(dest) {
		t.Fatalf("下载后二进制应可执行: %s", dest)
	}
	if _, err := os.Stat(dest + ".part"); !os.IsNotExist(err) {
		t.Fatalf(".part 临时文件应已被 rename 清理")
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("首次应命中服务器 1 次，实际 %d", got)
	}

	// 二次：幂等，跳过下载。
	res2, err := installLightpanda(context.Background(), []string{dir})
	if err != nil {
		t.Fatalf("二次安装失败: %v", err)
	}
	if res2.Installed {
		t.Fatalf("二次应 Installed=false（已就绪），得到 %+v", res2)
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("二次不应再命中服务器，总命中 %d", got)
	}
}

func TestDownloadToWritableDir_ReadOnlyFallback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("权限位语义不同")
	}
	srv, _ := fakeReleaseServer(t, "binary-bytes")
	withReleaseBase(t, srv.URL)

	// 第一个候选目录设为只读，第二个可写：期望回退到第二个。
	readOnly := t.TempDir()
	if err := os.Chmod(readOnly, 0o555); err != nil {
		t.Fatalf("chmod 只读失败: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(readOnly, 0o755) })
	writable := t.TempDir()

	url, err := lightpandaDownloadURL()
	if err != nil {
		t.Fatalf("URL: %v", err)
	}
	dest, err := downloadToWritableDir(context.Background(), []string{readOnly, writable}, engineFileName(EngineLightpanda), url)
	if err != nil {
		t.Fatalf("回退下载失败: %v", err)
	}
	if filepath.Dir(dest) != writable {
		t.Fatalf("应落在可写目录 %s，实际 %s", writable, dest)
	}
	if _, err := os.Stat(filepath.Join(readOnly, engineFileName(EngineLightpanda)+".part")); !os.IsNotExist(err) {
		t.Fatalf("只读目录不应残留 .part")
	}
}

func TestDownloadFile_HTTPErrorNoLeftover(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	dest := filepath.Join(dir, "engine")
	err := downloadFile(context.Background(), srv.URL, dest)
	if err == nil {
		t.Fatalf("404 应返回错误")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("错误应含状态码: %v", err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("失败不应生成目标文件")
	}
	if _, err := os.Stat(dest + ".part"); !os.IsNotExist(err) {
		t.Fatalf("失败应清理 .part")
	}
}

func TestDownloadFile_AtomicRename(t *testing.T) {
	srv, _ := fakeReleaseServer(t, "payload-xyz")
	withReleaseBase(t, srv.URL)
	dir := t.TempDir()
	dest := filepath.Join(dir, "engine")

	// 预置一个残留的 .part，验证会被 O_TRUNC 覆盖而非追加。
	if err := os.WriteFile(dest+".part", []byte("stale-old-content"), 0o644); err != nil {
		t.Fatalf("预置残留 .part: %v", err)
	}
	if err := downloadFile(context.Background(), srv.URL, dest); err != nil {
		t.Fatalf("下载失败: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("读取目标: %v", err)
	}
	if string(got) != "payload-xyz" {
		t.Fatalf("内容应为新下载而非残留追加，实际 %q", string(got))
	}
	if _, err := os.Stat(dest + ".part"); !os.IsNotExist(err) {
		t.Fatalf("成功后 .part 应消失")
	}
}

func TestResolveEngineBinary_AutoDownloadFallback(t *testing.T) {
	// 目录/PATH 找不到 Lightpanda 时，固定兜底下载（默认后端始终开箱即用）。
	srv, hits := fakeReleaseServer(t, "engine-bin")
	withReleaseBase(t, srv.URL)
	dir := t.TempDir()

	got, err := resolveEngineBinary(context.Background(), EngineLightpanda, []string{dir})
	if err != nil {
		t.Fatalf("自动下载兜底失败: %v", err)
	}
	if got != filepath.Join(dir, engineFileName(EngineLightpanda)) {
		t.Fatalf("解析路径异常: %s", got)
	}
	if atomic.LoadInt32(hits) != 1 {
		t.Fatalf("应触发一次下载")
	}
}

func TestResolveEngineBinary_FindsInstalled(t *testing.T) {
	// 已安装时直接命中，不触发下载。
	dir := t.TempDir()
	bin := filepath.Join(dir, engineFileName(EngineLightpanda))
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("写入: %v", err)
	}
	got, err := resolveEngineBinary(context.Background(), EngineLightpanda, []string{dir})
	if err != nil {
		t.Fatalf("已安装应直接命中: %v", err)
	}
	if got != bin {
		t.Fatalf("应返回 %s，实际 %s", bin, got)
	}
}

// TestFindInstalledEngine 验证只读探测：空目录返回未安装，放入可执行文件后返回已安装。
func TestFindInstalledEngine(t *testing.T) {
	dir := t.TempDir()
	if _, ok := findInstalledEngine(EngineLightpanda, []string{dir}); ok {
		t.Fatalf("空目录不应探测到引擎")
	}
	bin := filepath.Join(dir, engineFileName(EngineLightpanda))
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("写入占位引擎: %v", err)
	}
	p, ok := findInstalledEngine(EngineLightpanda, []string{dir})
	if !ok || p != bin {
		t.Fatalf("应探测到 %s，得到 %q ok=%v", bin, p, ok)
	}
}

// TestCheckEngineInstalled_NoDownload 验证 CheckInstall 只读、不触网。
func TestCheckEngineInstalled_NoDownload(t *testing.T) {
	srv, hits := fakeReleaseServer(t, "#!/bin/sh\n")
	withReleaseBase(t, srv.URL)
	res, err := checkEngineInstalled(context.Background())
	if err != nil {
		t.Fatalf("检查安装失败: %v", err)
	}
	// 测试环境通常未预装 Lightpanda，应返回未安装；且绝不触发下载。
	if res.Installed {
		t.Logf("本机已预装引擎，Installed=true: %s", res.Message)
	}
	if got := atomic.LoadInt32(hits); got != 0 {
		t.Fatalf("CheckInstall 不应触网，命中 %d 次", got)
	}
}

// TestInstallEngine_WritesProgress 验证安装把进度按行写入 progress 接收器。
func TestInstallEngine_WritesProgress(t *testing.T) {
	srv, _ := fakeReleaseServer(t, "#!/bin/sh\necho lightpanda\n")
	withReleaseBase(t, srv.URL)
	dir := t.TempDir()

	var buf strings.Builder
	setProgressWriter(&buf)
	defer setProgressWriter(nil)
	if _, err := installLightpanda(context.Background(), []string{dir}); err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "开始下载") || !strings.Contains(out, "已保存") {
		t.Fatalf("进度应包含下载与保存行，实际:\n%s", out)
	}
}

// TestDownloadFile_PercentProgress 验证已知 Content-Length 时下载会汇报百分比。
func TestDownloadFile_PercentProgress(t *testing.T) {
	payload := strings.Repeat("x", 512*1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	var buf strings.Builder
	setProgressWriter(&buf)
	defer setProgressWriter(nil)
	if err := downloadFile(context.Background(), srv.URL, filepath.Join(dir, "eng")); err != nil {
		t.Fatalf("下载失败: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "100%") {
		t.Fatalf("进度应含百分比（100%%），实际:\n%s", out)
	}
}

// TestRemoveEngines 验证卸载删除引擎二进制且幂等。
func TestRemoveEngines(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, engineFileName(EngineLightpanda))
	part := bin + ".part"
	if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(part, []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := removeEngines([]LightweightEngine{EngineLightpanda}, []string{dir})
	if err != nil {
		t.Fatalf("卸载失败: %v", err)
	}
	if !res.Removed {
		t.Fatalf("应删除资源，得到 %+v", res)
	}
	if _, err := os.Stat(bin); !os.IsNotExist(err) {
		t.Fatalf("引擎二进制应被删除")
	}
	if _, err := os.Stat(part); !os.IsNotExist(err) {
		t.Fatalf(".part 应被删除")
	}

	// 幂等：再次卸载无资源可删。
	res2, err := removeEngines([]LightweightEngine{EngineLightpanda}, []string{dir})
	if err != nil {
		t.Fatalf("二次卸载失败: %v", err)
	}
	if res2.Removed {
		t.Fatalf("二次应 Removed=false，得到 %+v", res2)
	}
}
