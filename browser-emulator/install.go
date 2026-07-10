package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/chenbstack/media-agent-plugin-sdk-go"
)

// lightpandaVersion 是内置自动下载的 Lightpanda 版本标签。
//
// nightly 是滚动构建，官方不发布稳定 SHA256/校验清单，因此本插件无法做“钉死版本 + 校验和”
// 的强校验；这里只保证下载产物非空、可执行，并在日志里记录来源 URL（见 README 风险说明）。
// 离线或需要可复现构建时应改用 make fetch-lightpanda 预置固定二进制。
const lightpandaVersion = "nightly"

// lightpandaReleaseBase 是 Lightpanda release 下载根 URL。
// 抽成包级变量，单测可覆盖为 httptest 地址，避免真实网络。
var lightpandaReleaseBase = "https://github.com/lightpanda-io/browser/releases/download"

// engineCacheSubdir 是 UserCacheDir / TMPDIR 下引擎缓存的子目录名。
const engineCacheSubdir = "media-agent-browser-emulator"

// httpDownloadClient 用于引擎下载；给足超时容纳几十 MB 的二进制。
// Transport 走系统环境代理（宿主按全局「代理服务器」设置注入 HTTP(S)_PROXY），未注入时直连。
var httpDownloadClient = &http.Client{Timeout: 5 * time.Minute, Transport: proxyFromEnvTransport()}

// engineFileName 返回本平台的引擎二进制文件名（与 Makefile fetch-* 及路径解析约定一致）。
func engineFileName(engine LightweightEngine) string {
	return string(engine) + "-" + runtime.GOOS + "-" + runtime.GOARCH
}

// engineDirs 按优先级返回引擎二进制的候选可写目录：
//
//	① 插件二进制同级目录（bin/，随插件包分发，最贴近插件）
//	② os.UserCacheDir()/media-agent-browser-emulator/bin（用户级缓存，跨升级保留）
//	③ TMPDIR/media-agent-browser-emulator/bin（最后兜底，可能被系统清理）
//
// 解析与下载都用同一份顺序，保证“下到哪里就能从哪里找到”。
func engineDirs() []string {
	var dirs []string
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Dir(exe))
	}
	if cache, err := os.UserCacheDir(); err == nil {
		dirs = append(dirs, filepath.Join(cache, engineCacheSubdir, "bin"))
	}
	dirs = append(dirs, filepath.Join(os.TempDir(), engineCacheSubdir, "bin"))
	return dirs
}

// lightpandaAsset 把当前 GOOS/GOARCH 映射到 Lightpanda release 资产名。
// 只覆盖 plugin.yaml entry 声明的三个平台；其余平台报错并附手动下载指引。
func lightpandaAsset() (string, error) {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "darwin/arm64":
		return "lightpanda-aarch64-macos", nil
	case "linux/amd64":
		return "lightpanda-x86_64-linux", nil
	case "linux/arm64":
		return "lightpanda-aarch64-linux", nil
	default:
		return "", fmt.Errorf(
			"当前平台 %s/%s 暂无内置 Lightpanda 下载；请手动执行 make fetch-lightpanda 或在配置中指定 lightweight_binary_path",
			runtime.GOOS, runtime.GOARCH)
	}
}

// lightpandaDownloadURL 组装 Lightpanda 下载地址。
func lightpandaDownloadURL() (string, error) {
	asset, err := lightpandaAsset()
	if err != nil {
		return "", err
	}
	return lightpandaReleaseBase + "/" + lightpandaVersion + "/" + asset, nil
}

// installEngine 是 lifecycle.install 钩子入口：宿主触发安装时调用（无实例/密钥），
// 预装默认引擎 Lightpanda。progress 是宿主提供的进度接收器（外部插件里即插件进程
// stderr），安装过程按行写入,宿主实时转发前端。幂等：已就绪则快速返回 {Installed:false}。
func installEngine(ctx context.Context, progress io.Writer) (pluginsdk.InstallResult, error) {
	setProgressWriter(progress)
	defer setProgressWriter(nil)
	return installLightpanda(ctx, engineDirs())
}

// checkEngineInstalled 是 CheckInstall 钩子入口：只读探测默认引擎是否就绪，绝不下载。
// 供宿主在插件加载时确定初始安装状态。
func checkEngineInstalled(ctx context.Context) (pluginsdk.InstallResult, error) {
	_ = ctx
	if p, ok := findInstalledEngine(EngineLightpanda, engineDirs()); ok {
		return pluginsdk.InstallResult{Installed: true, Message: "引擎已就绪: " + p}, nil
	}
	return pluginsdk.InstallResult{Installed: false, Message: "未检测到 Lightpanda 引擎，需下载安装"}, nil
}

// uninstallEngine 是 Uninstall 钩子入口：删除已下载的 Lightpanda 引擎二进制
// 回收磁盘空间。幂等：无引擎可删时返回 Removed=false。progress 汇报删除进度。
func uninstallEngine(ctx context.Context, progress io.Writer) (pluginsdk.UninstallResult, error) {
	_ = ctx
	setProgressWriter(progress)
	defer setProgressWriter(nil)
	return removeEngines([]LightweightEngine{EngineLightpanda}, engineDirs())
}

// removeEngines 删除给定引擎在各候选目录中的二进制（含 <engine>-<os>-<arch> 与裸 <engine>
// 及残留 .part）；dirs 可注入以便单测。
func removeEngines(engines []LightweightEngine, dirs []string) (pluginsdk.UninstallResult, error) {
	var removed []string
	for _, engine := range engines {
		for _, dir := range dirs {
			for _, name := range []string{engineFileName(engine), string(engine)} {
				for _, candidate := range []string{filepath.Join(dir, name), filepath.Join(dir, name) + ".part"} {
					if _, err := os.Stat(candidate); err != nil {
						continue
					}
					if err := os.Remove(candidate); err != nil {
						return pluginsdk.UninstallResult{}, fmt.Errorf("删除 %s 失败: %w", candidate, err)
					}
					logProgress("已删除 %s", candidate)
					removed = append(removed, candidate)
				}
			}
		}
	}
	if len(removed) == 0 {
		logProgress("未发现可卸载的引擎资源")
		return pluginsdk.UninstallResult{Removed: false, Message: "无可卸载资源"}, nil
	}
	return pluginsdk.UninstallResult{Removed: true, Message: fmt.Sprintf("已删除 %d 个引擎文件", len(removed))}, nil
}

// findInstalledEngine 在候选目录及 PATH 中查找已安装的引擎二进制；只读、不下载。
func findInstalledEngine(engine LightweightEngine, dirs []string) (string, bool) {
	fileName := engineFileName(engine)
	name := string(engine)
	for _, dir := range dirs {
		if p := filepath.Join(dir, fileName); isExecutable(p) {
			return p, true
		}
		if p := filepath.Join(dir, name); isExecutable(p) {
			return p, true
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p, true
	}
	return "", false
}

// installLightpanda 在给定候选目录中确保 Lightpanda 就绪；dirs 可注入以便单测。
func installLightpanda(ctx context.Context, dirs []string) (pluginsdk.InstallResult, error) {
	// 幂等：任一候选目录（或 PATH）已存在可执行文件即视为已就绪，绝不重复下载。
	if p, ok := findInstalledEngine(EngineLightpanda, dirs); ok {
		logProgress("引擎已就绪，跳过下载: %s", p)
		return pluginsdk.InstallResult{Installed: false, Message: "引擎已就绪: " + p}, nil
	}
	fileName := engineFileName(EngineLightpanda)
	url, err := lightpandaDownloadURL()
	if err != nil {
		return pluginsdk.InstallResult{}, err
	}
	dest, err := downloadToWritableDir(ctx, dirs, fileName, url)
	if err != nil {
		return pluginsdk.InstallResult{}, err
	}
	return pluginsdk.InstallResult{
		Installed: true,
		Message:   fmt.Sprintf("已下载 Lightpanda %s 到 %s", lightpandaVersion, dest),
	}, nil
}

// downloadToWritableDir 依次尝试候选目录，第一个可写且下载成功的目录胜出。
// 目录不可写（如插件包为只读挂载）会自动回退到下一个候选。
func downloadToWritableDir(ctx context.Context, dirs []string, fileName, url string) (string, error) {
	var lastErr error
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			lastErr = err
			logProgress("候选目录不可创建，跳过: %s (%v)", dir, err)
			continue
		}
		dest := filepath.Join(dir, fileName)
		if err := downloadFile(ctx, url, dest); err != nil {
			lastErr = err
			logProgress("下载到 %s 失败，尝试下一个候选目录: %v", dir, err)
			continue
		}
		return dest, nil
	}
	if lastErr == nil {
		lastErr = errors.New("无可用候选目录")
	}
	return "", fmt.Errorf("下载 Lightpanda 失败，所有候选目录均不可用: %w", lastErr)
}

// downloadFile 将 url 下载到 dest：先写 dest.part，成功后 chmod +x 并原子 rename 到 dest。
// 中途失败会清理 .part，避免留下半截文件被误当成可执行引擎。
// 先创建 .part 再发起网络请求，使“目录只读”这类错误能在触网前就快速回退。
func downloadFile(ctx context.Context, url, dest string) error {
	partPath := dest + ".part"
	f, err := os.OpenFile(partPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	success := false
	defer func() {
		f.Close()
		if !success {
			_ = os.Remove(partPath)
		}
	}()

	effURL := githubProxied(url)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, effURL, nil)
	if err != nil {
		return err
	}
	logProgress("开始下载 %s", effURL)
	resp, err := httpDownloadClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("下载返回 HTTP %d", resp.StatusCode)
	}

	total := resp.ContentLength // GitHub release 资产带 Content-Length；-1 表示未知
	if total > 0 {
		logProgress("引擎大小 %.1f MB，开始下载", mib(total))
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
	// rename 前再确保可执行位（部分文件系统 O_CREATE 的 mode 会被 umask 削掉）。
	if err := os.Chmod(partPath, 0o755); err != nil {
		return err
	}
	if err := os.Rename(partPath, dest); err != nil {
		return err
	}
	success = true
	logProgress("已保存 %s（%d 字节）", dest, n)
	return nil
}

// progressWriter 是安装进度的输出目标，默认插件进程 stderr（go-plugin 会捕获并转交
// 宿主，用户可见）。installEngine 期间会切换为宿主提供的 progress 接收器。安装被宿主
// 串行化（同一插件同一时刻只有一次安装），故此包级变量无需加锁。
var progressWriter io.Writer = os.Stderr

func setProgressWriter(w io.Writer) {
	if w == nil {
		progressWriter = os.Stderr
		return
	}
	progressWriter = w
}

// logProgress 把安装进度按行写入 progressWriter，宿主据此向前端展示实时进度。
func logProgress(format string, args ...any) {
	fmt.Fprintf(progressWriter, "[browser-emulator install] "+format+"\n", args...)
}

func mib(b int64) float64 { return float64(b) / (1024 * 1024) }

// progressReader 包裹下载流，边读边按百分比汇报进度：已知总长时每涨约 5%（且间隔
// ≥300ms）汇报一次“下载中 NN%（x/y MB）”；未知总长时每 500ms 汇报已下载量。宿主/前端
// 从进度行里解析出百分比渲染进度条。
type progressReader struct {
	r        io.Reader
	total    int64
	read     int64
	lastPct  int
	lastTick time.Time
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.read += int64(n)
	p.tick(false)
	return n, err
}

func (p *progressReader) tick(final bool) {
	now := time.Now()
	if p.total > 0 {
		pct := int(p.read * 100 / p.total)
		if pct > 100 {
			pct = 100
		}
		if final || (pct >= p.lastPct+5 && now.Sub(p.lastTick) >= 300*time.Millisecond) {
			p.lastPct = pct
			p.lastTick = now
			logProgress("下载中 %d%%（%.1f/%.1f MB）", pct, mib(p.read), mib(p.total))
		}
		return
	}
	if final || now.Sub(p.lastTick) >= 500*time.Millisecond {
		p.lastTick = now
		logProgress("下载中 %.1f MB", mib(p.read))
	}
}

// finish 在下载完成后补一条 100% 进度，避免因节流漏掉最后一档。
func (p *progressReader) finish(n int64) {
	p.read = n
	if p.total <= 0 {
		p.total = n
	}
	p.tick(true)
}
