package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"media-agent-lab/server/pkg/pluginsdk"
)

// CloakBrowser 是基于 Chromium 源码、内置指纹伪装、能过 Cloudflare 人机认证的隐身浏览器。
// 与 Lightpanda 不同，它以 .tar.gz 归档分发（解压后是一整棵 Chromium 目录树），且每个
// release 附带 SHA256SUMS 校验清单，故这里做钉死版本 + 归档校验的强校验。

// cloakPrimaryBase / cloakGithubBase 是归档与 SHA256SUMS 的两个下载根：先试官方主源，
// 失败再退到 GitHub release 兜底。抽成包级变量以便单测覆盖为 httptest 地址，避免真实网络。
var (
	cloakPrimaryBase = "https://cloakbrowser.dev"
	cloakGithubBase  = "https://github.com/CloakHQ/cloakbrowser/releases/download"
)

// cloakDownloadClient 用于 CloakBrowser 归档下载（约 200MB），超时给到 15 分钟；
// 抽成包级变量以便单测覆盖。Transport 走系统环境代理（宿主注入 HTTP(S)_PROXY），未注入时直连。
var cloakDownloadClient = &http.Client{Timeout: 15 * time.Minute, Transport: proxyFromEnvTransport()}

// cloakPlatformSpec 是某个平台对应的 release 资产 tag 与免费档钉死版本。
type cloakPlatformSpec struct {
	tag     string
	version string
}

// cloakPlatforms 把 GOOS/GOARCH 映射到 CloakBrowser 免费档的平台 tag 与版本。
// 只覆盖插件支持的三个平台；其余平台在解析时报错。
var cloakPlatforms = map[string]cloakPlatformSpec{
	"darwin/arm64": {tag: "darwin-arm64", version: "145.0.7632.109.2"},
	"linux/amd64":  {tag: "linux-x64", version: "146.0.7680.177.5"},
	"linux/arm64":  {tag: "linux-arm64", version: "146.0.7680.177.3"},
}

// cloakPlatformSpecFor 返回当前 GOOS/GOARCH 对应的平台规格；不支持的平台报错。
func cloakPlatformSpecFor() (cloakPlatformSpec, error) {
	key := runtime.GOOS + "/" + runtime.GOARCH
	if spec, ok := cloakPlatforms[key]; ok {
		return spec, nil
	}
	return cloakPlatformSpec{}, fmt.Errorf(
		"当前平台 %s 暂无内置 CloakBrowser 下载；请在配置中指定可执行路径或改用其他引擎", key)
}

// cloakPlatformTag 返回当前平台的 release 资产 tag。
func cloakPlatformTag() (string, error) {
	spec, err := cloakPlatformSpecFor()
	return spec.tag, err
}

// cloakVersion 返回当前平台的钉死版本。
func cloakVersion() (string, error) {
	spec, err := cloakPlatformSpecFor()
	return spec.version, err
}

// cloakArchiveName 返回平台 tag 对应的归档文件名（与 SHA256SUMS 中的文件名一致）。
func cloakArchiveName(tag string) string {
	return "cloakbrowser-" + tag + ".tar.gz"
}

// cloakArchiveURLs 组装归档下载地址：主源在前、GitHub 兜底在后。
func cloakArchiveURLs(version, tag string) []string {
	suffix := "/chromium-v" + version + "/" + cloakArchiveName(tag)
	return []string{cloakPrimaryBase + suffix, cloakGithubBase + suffix}
}

// cloakSumsURLs 组装 SHA256SUMS 下载地址：与归档同源、同目录。
func cloakSumsURLs(version string) []string {
	suffix := "/chromium-v" + version + "/SHA256SUMS"
	return []string{cloakPrimaryBase + suffix, cloakGithubBase + suffix}
}

// cloakExecPathForTag 按平台 tag 推导解压后可执行文件的路径（darwin 走 .app 布局，linux 走裸 chrome）。
// tag 显式传入以便单测在任意宿主上验证 linux 布局。
func cloakExecPathForTag(dir, version, tag string) string {
	root := filepath.Join(dir, "cloak-chromium-"+version)
	if strings.HasPrefix(tag, "darwin") {
		return filepath.Join(root, "Chromium.app", "Contents", "MacOS", "Chromium")
	}
	return filepath.Join(root, "chrome")
}

// cloakExecPath 返回当前平台在 dir 下的可执行文件路径。
func cloakExecPath(dir, version string) (string, error) {
	tag, err := cloakPlatformTag()
	if err != nil {
		return "", err
	}
	return cloakExecPathForTag(dir, version, tag), nil
}

// installCloak 是 CloakBrowser 的安装钩子入口：下载/校验/解压/定位。progress 是宿主提供的
// 进度接收器，安装过程按行写入，宿主实时转发前端。幂等：已就绪则快速返回 Installed=false。
func installCloak(ctx context.Context, progress io.Writer) (pluginsdk.InstallResult, error) {
	setProgressWriter(progress)
	defer setProgressWriter(nil)
	return installCloakToDirs(ctx, engineDirs())
}

// installCloakToDirs 在给定候选目录中确保 CloakBrowser 就绪；dirs 可注入以便单测。
func installCloakToDirs(ctx context.Context, dirs []string) (pluginsdk.InstallResult, error) {
	if p, ok := findInstalledCloak(dirs); ok {
		logProgress("CloakBrowser 已就绪，跳过下载: %s", p)
		return pluginsdk.InstallResult{Installed: false, Message: "已就绪: " + p}, nil
	}
	spec, err := cloakPlatformSpecFor()
	if err != nil {
		return pluginsdk.InstallResult{}, err
	}
	_, dir, err := downloadCloakToWritableDir(ctx, dirs, spec.version, spec.tag)
	if err != nil {
		return pluginsdk.InstallResult{}, err
	}
	return pluginsdk.InstallResult{
		Installed: true,
		Message:   fmt.Sprintf("已安装 CloakBrowser %s 到 %s", spec.version, dir),
	}, nil
}

// checkCloakInstalled 是 CheckInstall 钩子入口：只读探测 CloakBrowser 是否就绪，绝不下载。
func checkCloakInstalled(ctx context.Context) (pluginsdk.InstallResult, error) {
	_ = ctx
	if p, ok := findInstalledCloak(engineDirs()); ok {
		return pluginsdk.InstallResult{Installed: true, Message: "CloakBrowser 已就绪: " + p}, nil
	}
	return pluginsdk.InstallResult{Installed: false, Message: "未检测到 CloakBrowser，需下载安装"}, nil
}

// uninstallCloak 是 Uninstall 钩子入口：删除已下载的 CloakBrowser 目录回收磁盘空间。
func uninstallCloak(ctx context.Context, progress io.Writer) (pluginsdk.UninstallResult, error) {
	_ = ctx
	setProgressWriter(progress)
	defer setProgressWriter(nil)
	return removeCloak(engineDirs())
}

// removeCloak 删除各候选目录里的 cloak-chromium-* 目录及残留 .part；dirs 可注入以便单测。
// 幂等：无可删返回 Removed=false。
func removeCloak(dirs []string) (pluginsdk.UninstallResult, error) {
	removedDirs := 0
	removedParts := 0
	for _, dir := range dirs {
		matches, _ := filepath.Glob(filepath.Join(dir, "cloak-chromium-*"))
		for _, m := range matches {
			info, err := os.Stat(m)
			if err != nil || !info.IsDir() {
				continue
			}
			if err := os.RemoveAll(m); err != nil {
				return pluginsdk.UninstallResult{}, fmt.Errorf("删除 %s 失败: %w", m, err)
			}
			logProgress("已删除 %s", m)
			removedDirs++
		}
		parts, _ := filepath.Glob(filepath.Join(dir, "cloakbrowser-*.tar.gz.part"))
		for _, p := range parts {
			if err := os.Remove(p); err != nil {
				return pluginsdk.UninstallResult{}, fmt.Errorf("删除 %s 失败: %w", p, err)
			}
			logProgress("已删除残留 %s", p)
			removedParts++
		}
	}
	if removedDirs == 0 && removedParts == 0 {
		logProgress("未发现可卸载的 CloakBrowser 资源")
		return pluginsdk.UninstallResult{Removed: false, Message: "无可卸载资源"}, nil
	}
	return pluginsdk.UninstallResult{
		Removed: true,
		Message: fmt.Sprintf("已删除 %d 个 CloakBrowser 目录", removedDirs),
	}, nil
}

// findInstalledCloak 在候选目录里查找当前平台版本的可执行文件；只读、不下载。
func findInstalledCloak(dirs []string) (string, bool) {
	spec, err := cloakPlatformSpecFor()
	if err != nil {
		return "", false
	}
	for _, dir := range dirs {
		p := cloakExecPathForTag(dir, spec.version, spec.tag)
		if isExecutable(p) {
			return p, true
		}
	}
	return "", false
}

// resolveCloakBinary 在候选目录里查找已安装的 CloakBrowser 可执行文件（dirs 可注入以便单测）。
// CloakBrowser 约 200MB，仅由用户在详情页手动安装，渲染时绝不自动下载——缺失即报错引导安装。
func resolveCloakBinary(dirs []string) (string, error) {
	if p, ok := findInstalledCloak(dirs); ok {
		return p, nil
	}
	return "", errors.New("未找到 CloakBrowser，请在插件详情页点安装「隐身 Chromium」组件")
}

// cloakStealthArgs 返回隐身启动需要“额外追加”的参数（过 CF 的关键）。
// 调用方（chromedp）自行剔除默认的 --enable-automation。fingerprint 每次随机，用 math/rand 足够。
func cloakStealthArgs() []string {
	args := []string{
		"--no-sandbox",
		fmt.Sprintf("--fingerprint=%d", 10000+rand.Intn(90000)),
	}
	if runtime.GOOS == "darwin" {
		args = append(args, "--fingerprint-platform=macos")
	} else {
		args = append(args, "--fingerprint-platform=windows")
	}
	return args
}

// downloadCloakToWritableDir 依次尝试候选目录，第一个可写且下载/解压成功的目录胜出。
// 返回可执行文件路径与落地目录。
func downloadCloakToWritableDir(ctx context.Context, dirs []string, version, tag string) (execPath, dir string, err error) {
	var lastErr error
	for _, d := range dirs {
		if mkErr := os.MkdirAll(d, 0o755); mkErr != nil {
			lastErr = mkErr
			logProgress("候选目录不可创建，跳过: %s (%v)", d, mkErr)
			continue
		}
		exec, dlErr := downloadCloakToDir(ctx, d, version, tag)
		if dlErr != nil {
			lastErr = dlErr
			logProgress("下载/解压到 %s 失败，尝试下一个候选目录: %v", d, dlErr)
			// 清理可能残留的半成品解压目录，避免被误当成已安装。
			_ = os.RemoveAll(filepath.Join(d, "cloak-chromium-"+version))
			continue
		}
		return exec, d, nil
	}
	if lastErr == nil {
		lastErr = errors.New("无可用候选目录")
	}
	return "", "", fmt.Errorf("安装 CloakBrowser 失败，所有候选目录均不可用: %w", lastErr)
}

// downloadCloakToDir 把归档下载到 dir、校验 SHA256、解压，并返回可执行文件路径。
// dir/version/tag 显式传入以便单测在任意宿主上直接驱动，不依赖 runtime。
func downloadCloakToDir(ctx context.Context, dir, version, tag string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tarPath := filepath.Join(dir, cloakArchiveName(tag)+".part")
	// 无论成功与否都清理归档临时文件（解压后即无用）。
	defer os.Remove(tarPath)

	if err := downloadCloakArchive(ctx, version, tag, tarPath); err != nil {
		return "", err
	}
	if err := verifyCloakArchive(ctx, version, tag, tarPath); err != nil {
		return "", err
	}
	logProgress("开始解压 CloakBrowser 归档...")
	if err := extractTarGz(tarPath, dir); err != nil {
		return "", fmt.Errorf("解压 CloakBrowser 归档失败: %w", err)
	}
	execPath := cloakExecPathForTag(dir, version, tag)
	if !isExecutable(execPath) {
		return "", fmt.Errorf("解压后未找到可执行文件: %s", execPath)
	}
	logProgress("CloakBrowser 就绪: %s", execPath)
	return execPath, nil
}

// downloadCloakArchive 下载归档到 dest：先试主源，失败退到 GitHub 兜底。
func downloadCloakArchive(ctx context.Context, version, tag, dest string) error {
	urls := cloakArchiveURLs(version, tag)
	var lastErr error
	for i, url := range urls {
		if err := downloadCloakFile(ctx, url, dest); err != nil {
			lastErr = err
			logProgress("从 %s 下载失败: %v", url, err)
			if i < len(urls)-1 {
				logProgress("尝试兜底源...")
			}
			continue
		}
		return nil
	}
	return fmt.Errorf("下载 CloakBrowser 归档失败（主源与兜底源均不可用）: %w", lastErr)
}

// downloadCloakFile 把 url 下载到 dest（归档临时文件，非可执行）。用 progressReader 汇报百分比。
func downloadCloakFile(ctx context.Context, url, dest string) error {
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	success := false
	defer func() {
		f.Close()
		if !success {
			_ = os.Remove(dest)
		}
	}()

	effURL := githubProxied(url)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, effURL, nil)
	if err != nil {
		return err
	}
	logProgress("开始下载 %s", effURL)
	resp, err := cloakDownloadClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("下载返回 HTTP %d", resp.StatusCode)
	}

	total := resp.ContentLength
	if total > 0 {
		logProgress("归档大小 %.1f MB，开始下载", mib(total))
	}
	pr := &progressReader{r: resp.Body, total: total}
	n, err := io.Copy(f, pr)
	if err != nil {
		return err
	}
	if n == 0 {
		return errors.New("下载内容为空")
	}
	pr.finish(n)
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	success = true
	logProgress("已下载归档 %s（%d 字节）", dest, n)
	return nil
}

// verifyCloakArchive 用同源 SHA256SUMS 校验归档。取不到 SHA256SUMS（HTTP 非 200）时记日志并跳过，
// 不因此失败；只要取到且 hash 不匹配就报错（调用方会清理下载物）。
func verifyCloakArchive(ctx context.Context, version, tag, tarPath string) error {
	sums, err := fetchCloakSums(ctx, version)
	if err != nil {
		logProgress("未取到 SHA256SUMS，跳过校验")
		return nil
	}
	want, ok := sums[cloakArchiveName(tag)]
	if !ok {
		logProgress("未取到 SHA256SUMS，跳过校验")
		return nil
	}
	got, err := sha256File(tarPath)
	if err != nil {
		return err
	}
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("SHA256 校验失败: 期望 %s，实际 %s", want, got)
	}
	logProgress("SHA256 校验通过")
	return nil
}

// fetchCloakSums 拉取并解析 SHA256SUMS（先主源后兜底）；返回 文件名 -> hex hash。
func fetchCloakSums(ctx context.Context, version string) (map[string]string, error) {
	var lastErr error
	for _, url := range cloakSumsURLs(version) {
		m, err := fetchSumsFrom(ctx, url)
		if err != nil {
			lastErr = err
			continue
		}
		return m, nil
	}
	if lastErr == nil {
		lastErr = errors.New("无可用 SHA256SUMS 源")
	}
	return nil, lastErr
}

// fetchSumsFrom 从单个 URL 拉取并解析 SHA256SUMS 文本（每行 `<hex sha256>  <文件名>`）。
func fetchSumsFrom(ctx context.Context, url string) (map[string]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubProxied(url), nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpDownloadClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("SHA256SUMS 返回 HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	m := make(map[string]string)
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		// 文件名可能带 GNU coreutils 的二进制标记前缀 '*'。
		name := strings.TrimPrefix(fields[len(fields)-1], "*")
		m[name] = fields[0]
	}
	return m, nil
}

// sha256File 计算文件的十六进制 SHA256。
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// extractTarGz 把 .tar.gz 解压到 destDir：保留权限位（可执行位），跳过含 .. 的路径防目录穿越，
// 分别处理目录 / 普通文件 / 符号链接（macOS 的 .app 里含符号链接）。
func extractTarGz(archivePath, destDir string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()

	cleanDest := filepath.Clean(destDir)
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(hdr.Name)
		// 防目录穿越：跳过含 .. 的路径或指向 destDir 之外的条目。
		if name == ".." || strings.HasPrefix(name, ".."+string(os.PathSeparator)) || strings.Contains(hdr.Name, "..") {
			logProgress("跳过可疑归档路径: %s", hdr.Name)
			continue
		}
		target := filepath.Join(destDir, name)
		if target != cleanDest && !strings.HasPrefix(target, cleanDest+string(os.PathSeparator)) {
			logProgress("跳过越界归档路径: %s", hdr.Name)
			continue
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, hdr.FileInfo().Mode().Perm()|0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, hdr.FileInfo().Mode().Perm())
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
			// 部分文件系统 O_CREATE 的 mode 会被 umask 削掉，显式补回权限位（含可执行位）。
			if err := os.Chmod(target, hdr.FileInfo().Mode().Perm()); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			_ = os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		default:
			// 忽略其他类型（硬链接 / 设备节点等），CloakBrowser 归档不含它们。
		}
	}
	return nil
}
