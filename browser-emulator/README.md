# 浏览器仿真插件（browser-emulator）

站点直接 HTTP 命中 Cloudflare / DDoS-GUARD 反爬挑战时（见主仓
`server/internal/plugins/official/site/provider.go` 的 `underChallenge`），
宿主调用本插件用真实浏览器环境渲染页面，取回真实 HTML 和渲染后的全量 cookie
（如 Cloudflare 通过后的 `cf_clearance`）。

实现主仓 `providers.RendererProvider`：`Kind` / `TestConnection` / `Render`，
通过 HashiCorp go-plugin（`renderer.render` / `renderer.test` capability）与宿主通信。

## 后端架构

全程用**纯 Go 的 [chromedp](https://github.com/chromedp/chromedp)** 通过 CDP 驱动浏览器，
**不依赖 node / playwright 驱动进程**。默认路径进程树只有：本插件 Go 进程 + 轻量引擎子进程。

| 后端 | 说明 | 浏览器进程 |
| --- | --- | --- |
| `lightweight`（默认） | `chromedp.NewRemoteAllocator` 接入轻量引擎 CDP | Lightpanda（插件懒启动子进程，或接外部 CDP 端点） |
| `chromium`（可选兜底） | `chromedp.NewExecAllocator` 启动隐身 Chromium | CloakBrowser（基于 Chromium 源码、源码级指纹伪装，过 Cloudflare 人机验证；约 200MB，需先在详情页安装「隐身 Chromium」组件） |

一次 `Render` 的流程（`render.go`）：`Network.enable`/`Page.enable` → 注入 UA(`Emulation.setUserAgentOverride`)
/ headers(`Network.setExtraHTTPHeaders`) / cookie(`Network.setCookie`) → `Page.navigate`
→ 按 `WaitUntil`（默认 `domcontentloaded`，监听 `domContentEventFired`/`loadEventFired` 事件）
/ `WaitSelector`（`Runtime.evaluate` 轮询 `querySelector`）等待
→ 若命中反爬挑战页（`underChallenge` 启发式，标记列表对齐主仓）则轮询等待挑战清除
→ 取回渲染后 HTML(`outerHTML`)、最终 URL(`location.href`)、状态码(`Network.responseReceived`)
和全量 cookie(`Storage.getCookies`，退回 `Network.getCookies`)。每次渲染独立 CDP context，用完即关。

**默认后端 `lightweight` / `lightpanda`**：轻量引擎已实测能完全覆盖本插件核心功能
（JS 渲染、cookie 注入与回读、WaitSelector 等待），进程树总内存约 72MB，冷启动数十毫秒。
Chromium 仅作可选兜底，用本机浏览器，不下载。

## 依赖与安装

**无 node / playwright / driver 依赖**。轻量后端只需轻量引擎二进制；Chromium 兜底用本机浏览器。

### 轻量引擎二进制（不入仓，宿主启用时自动预装）

二进制体积大，不纳入仓库（见 `.gitignore`）。插件声明了 `lifecycle.install` 能力：

- **加载时**：宿主调用只读的 `CheckInstall` 探测引擎是否就绪（扫候选目录 + `PATH`，**绝不下载**），
  据此把安装状态标记为 `installed` 或 `pending`。
- **安装时**：用户在插件详情页手动点「开始安装」触发 `Install`，从 GitHub 下载默认引擎
  **Lightpanda** 到第一个可写目录：

  1. 插件 `bin/`（与二进制同级，随包分发）
  2. `os.UserCacheDir()/media-agent-browser-emulator/bin`（用户级缓存，跨升级保留）
  3. `TMPDIR/media-agent-browser-emulator/bin`（最后兜底）

下载走 `<dest>.part` 临时文件 + `chmod +x` + 原子 `rename`，中途失败自动清理并回退下一个候选目录。
`Install` **幂等**：已就绪则快速返回 `{Installed:false}` 不重复下载，安装失败后可反复重试且不会残留半成品。
安装进度按行写入宿主提供的 progress 接收器（外部插件里即插件进程 stderr），经 go-plugin `SyncStderr`
实时流回宿主，宿主再转发给前端在详情页逐行展示。

> Lightpanda 用 `nightly` 滚动构建，官方不发布稳定 SHA256 校验清单，因此无法做“钉死版本 + 校验和”
> 强校验；插件只保证下载产物非空、可执行并记录来源 URL。需要可复现构建或离线部署时，改用下方
> `make fetch-*` 预置固定二进制。

**离线 / 手动 / 内网**：可预先拉取到 `bin/`，`Install` 检测到已就绪即跳过下载：

```bash
make fetch-lightpanda   # -> bin/lightpanda-<os>-<arch>
```

运行时解析顺序：配置 `lightweight_binary_path` → 上述三候选目录 → `PATH` →
（缺失且 `auto_download_engine=true` 时）运行时兜底下载 Lightpanda。
关闭 `auto_download_engine` 后，缺失将直接报错并提示 `make fetch-lightpanda`。
也可用配置 `lightweight_cdp_url` 直接接入已运行的外部 CDP 端点（此时不自管子进程）。

启用 `backend=chromium` 时用已安装的 CloakBrowser（在详情页安装「隐身 Chromium」组件，
或用配置 `chromium_exec_path` 指定任意 Chromium / 自建 CloakBrowser 二进制路径）。

## 构建 / 测试

```bash
make build     # 产出 bin/media-agent-plugin-browser-emulator-<os>-<arch>
make test      # 单元测试（无需浏览器）
make eval      # 后端功能评测（需本地测试页 + 轻量引擎 CDP）
make smoke     # 端到端 smoke（pluginrpc.ExternalPlugin 拉起已构建二进制走 RPC 全链路）
```

## 内存对比（迁移 playwright-go → chromedp）

实测 macOS arm64，渲染 JS 页面峰值 RSS，进程树逐项：

| 项 | 迁移前（playwright-go） | 迁移后（chromedp） |
| --- | --- | --- |
| 本插件 Go 进程 | ~25 MB | ~21 MB |
| **playwright node 驱动** | **~140 MB** | **已移除** |
| Lightpanda 引擎 | ~36 MB | ~51 MB |
| **默认轻量后端进程树合计** | **~200 MB** | **~72 MB** |

`memory_limit_mb` 已从 512 下调到 **256**（默认轻量路径 ~3.5x headroom）。
启用 `backend=chromium` 时进程树 ≈ 310MB（Go ~20MB + Chromium ~290MB），需把限额上调回 512MB。

## chromedp 对轻量引擎的兼容性（逐 CDP 方法实测）

| CDP 能力 | Lightpanda |
| --- | --- |
| 握手 / Target.createTarget+attach | 通过 |
| Network.enable / Page.enable | 通过 |
| Network.setCookie（cookie 注入） | 通过 |
| Storage.getCookies / Network.getCookies（回读） | 通过 |
| Network.setExtraHTTPHeaders | 通过 |
| Runtime.evaluate（querySelector 轮询 / outerHTML / location.href） | 通过 |
| Page.domContentEventFired / loadEventFired 事件 | 通过 |
| **Network.responseReceived（状态码）** | **不发事件** → 拿不到状态码，`Status=0` |
| **Emulation.setUserAgentOverride（页面 UA）** | 通过（`navigator.userAgent` 生效） |

已知坑与补丁：
- **Lightpanda 无状态码**：`RenderResult.Status` 兜底为 `0`（HTML/cookie 不受影响，站点兜底场景状态码非必需）。

chromedp 对 Lightpanda 可用，**无需回退 go-rod**。

## 功能与选型结论

功能页：本地 JS 测试页（setTimeout 改写 DOM）+ `quotes.toscrape.com/js`。

| 后端 | 基本 JS 渲染 | cookie 注入+回读 | WaitSelector | 状态码 | UA 覆盖 | Cloudflare Turnstile | 峰值 RSS(引擎) | 冷启动 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Lightpanda（默认轻量） | 通过 | 通过 | 通过 | 拿不到(0) | 生效 | 未通过 | ~51 MB | ~0.04s |
| CloakBrowser（隐身 Chromium 兜底） | 通过 | 通过 | 通过 | 200 | 生效 | **可过** | ~290 MB | ~0.17s |

**结论**：Lightpanda 作默认轻量引擎（RSS 低、UA 覆盖生效、无需额外 flag），已能覆盖 JS 渲染、
cookie 注入回读、WaitSelector 等核心功能。遇到 Cloudflare 人机验证（Turnstile / Managed Challenge）
时切到隐身 Chromium 兜底——CloakBrowser 基于 Chromium 源码做源码级指纹伪装，能自动通过 Turnstile，
代价是 ~200MB 体积与更高内存。

## 配置

见 `config.schema.json`。关键字段：`backend`（lightweight/chromium）、
`lightweight_binary_path`、`lightweight_cdp_url`、`chromium_exec_path`、
`headless`、`auto_download_engine`、`proxy_url`、`default_timeout_seconds`。
