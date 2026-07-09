package main

import (
	"context"
	_ "embed"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"time"

	"media-agent-lab/server/pkg/pluginsdk"
	"media-agent-lab/server/pkg/pluginsdk/providers"
)

//go:embed manifest.yaml
var manifestYAML []byte

//go:embed config.schema.json
var schemaJSON []byte

//go:embed icon.svg
var iconSVG []byte

func Plugin() pluginsdk.Plugin {
	return pluginsdk.Plugin{
		Manifest:       pluginsdk.MustParseManifest(manifestYAML),
		ConfigSchema:   pluginsdk.MustParseConfigSchema(schemaJSON),
		IconSVG:        iconSVG,
		NewStorage:     newStorage,
		StartAuth:      startAuth,
		CheckAuth:      checkAuth,
		ValidateConfig: validateConfig,
	}
}

type provider struct {
	root      string
	cookieRef string
	secrets   pluginsdk.SecretResolver
	logger    pluginsdk.Logger
	client    *client115
}

var _ providers.PlaybackURLProvider = (*provider)(nil)

func newStorage(ctx context.Context, inst pluginsdk.Instance, secrets pluginsdk.SecretResolver) (providers.StorageProvider, error) {
	root := strings.TrimSpace(stringConfig(inst.Config["root_path"]))
	if root == "" {
		root = strings.TrimSpace(stringConfig(inst.Config["remote_root"]))
	}
	if root == "" {
		root = "/"
	}
	p := &provider{
		root:      cleanCloudPath(root),
		cookieRef: strings.TrimSpace(stringConfig(inst.Config["cookie"])),
		secrets:   secrets,
		logger:    inst.Logger,
	}
	if p.cookieRef == "" {
		return nil, fmt.Errorf("115 Cookie 必填")
	}
	cookie, err := p.cookie(ctx)
	if err != nil {
		return nil, err
	}
	p.client = newClient115(cookie, &http.Client{Timeout: 60 * time.Second}, inst.KV)
	return p, nil
}

func validateConfig(config map[string]any) error {
	errs := map[string]string{}
	if str := strings.TrimSpace(stringConfig(config["cookie"])); str == "" {
		errs["cookie"] = "115 Cookie 必填"
	}
	if root := strings.TrimSpace(stringConfig(config["remote_root"])); root != "" && !strings.HasPrefix(root, "/") {
		errs["remote_root"] = "远端根目录必须以 / 开头"
	}
	if len(errs) > 0 {
		return &pluginsdk.ValidationError{Fields: errs}
	}
	return nil
}

func (p *provider) Kind() string { return "115" }

func (p *provider) TestConnection(ctx context.Context) error {
	parent := cleanCloudPath(p.root)
	dirID := int64(0)
	if parent != "/" {
		id, err := p.client.getDirID(ctx, parent)
		if err != nil {
			return fmt.Errorf("读取 115 远端根目录: %w", err)
		}
		dirID = id
	}
	if err := p.client.probeDir(ctx, dirID); err != nil {
		return fmt.Errorf("读取 115 目录元数据: %w", err)
	}
	return nil
}

func (p *provider) Info(ctx context.Context) (providers.StorageInfo, error) {
	info := providers.StorageInfo{Kind: p.Kind(), RootPath: p.root, Capabilities: []string{
		"storage.115", "storage.path", "storage.cloud", "storage.network", "storage.test", "storage.quota", "storage.browse",
		"storage.playback_url",
		"storage.operation.copy", "storage.operation.server_copy", "storage.operation.move",
	}}
	total, used, err := p.client.quota(ctx)
	if err == nil {
		info.TotalBytes = total
		info.UsedBytes = used
	} else if p.logger != nil {
		p.logger.Warn(ctx, "115 容量检测失败", "error", err.Error())
	}
	return info, nil
}

func (p *provider) EnsureMounted(ctx context.Context) error { return p.TestConnection(ctx) }

func (p *provider) Unmount(ctx context.Context) error { return nil }

func (p *provider) Stat(ctx context.Context, name string) (providers.StorageFileInfo, error) {
	item, err := p.item(ctx, name)
	if err != nil {
		return providers.StorageFileInfo{}, err
	}
	return providers.StorageFileInfo{Name: item.Name, Size: item.Size, IsDir: item.IsDir, ModTime: item.ModTime}, nil
}

func (p *provider) ListDir(ctx context.Context, name string) ([]providers.StorageFileInfo, error) {
	cloudPath, err := p.cloudPath(name)
	if err != nil {
		return nil, err
	}
	dirID := int64(0)
	if cloudPath != "/" {
		dirID, err = p.client.getDirID(ctx, cloudPath)
		if err != nil {
			return nil, err
		}
	}
	items, err := p.client.listDir(ctx, dirID)
	if err != nil {
		return nil, err
	}
	out := make([]providers.StorageFileInfo, 0, len(items))
	for _, item := range items {
		if !item.IsDir {
			continue
		}
		out = append(out, providers.StorageFileInfo{
			Name:    item.Name,
			Size:    item.Size,
			IsDir:   item.IsDir,
			ModTime: item.ModTime,
		})
	}
	return out, nil
}

func (p *provider) MkdirAll(ctx context.Context, name string) error {
	cloudPath, err := p.cloudPath(name)
	if err != nil {
		return err
	}
	if cloudPath == "/" {
		return nil
	}
	_, err = p.client.mkdirAll(ctx, cloudPath)
	return err
}

func (p *provider) Remove(ctx context.Context, name string) error {
	item, err := p.item(ctx, name)
	if err != nil {
		return err
	}
	if item.ID == 0 {
		return fmt.Errorf("拒绝删除 115 根目录")
	}
	return p.client.delete(ctx, item.ID)
}

func (p *provider) OpenReader(ctx context.Context, name string) (io.ReadCloser, error) {
	item, err := p.item(ctx, name)
	if err != nil {
		return nil, err
	}
	if item.IsDir {
		return nil, fmt.Errorf("%s 是目录", name)
	}
	downloadURL, headers, err := p.client.downloadURL(ctx, item.PickCode)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return nil, err
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := p.client.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, fmt.Errorf("下载 115 文件: HTTP %d", resp.StatusCode)
	}
	return resp.Body, nil
}

func (p *provider) ResolvePlaybackURL(ctx context.Context, input providers.PlaybackURLInput) (providers.PlaybackURLResult, error) {
	pickCode := firstNonEmpty(
		stringConfig(input.Metadata["pickcode"]),
		stringConfig(input.Metadata["pick_code"]),
		stringConfig(input.Metadata["pc"]),
	)
	if pickCode == "" {
		item, err := p.item(ctx, input.Path)
		if err != nil {
			return providers.PlaybackURLResult{}, err
		}
		if item.IsDir {
			return providers.PlaybackURLResult{}, fmt.Errorf("%s 是目录", input.Path)
		}
		pickCode = item.PickCode
	}
	playURL, headers, err := p.client.downloadURL(ctx, pickCode)
	if err != nil {
		return providers.PlaybackURLResult{}, err
	}
	return providers.PlaybackURLResult{
		URL:     playURL,
		Headers: headers,
	}, nil
}

func (p *provider) OpenWriter(ctx context.Context, name string) (io.WriteCloser, error) {
	return nil, fmt.Errorf("115 插件不支持 OpenWriter 流式写入，请使用 UploadProvider")
}

func (p *provider) Upload(ctx context.Context, name string, source providers.UploadSource) error {
	target, err := p.cloudPath(name)
	if err != nil {
		return err
	}
	if target == "/" {
		return fmt.Errorf("拒绝上传覆盖 115 根目录")
	}
	parentID, err := p.client.mkdirAll(ctx, path.Dir(target))
	if err != nil {
		return err
	}
	if err := p.client.uploadFast(ctx, parentID, path.Base(target), source); err != nil {
		return err
	}
	p.client.invalidatePath(target)
	p.client.invalidatePath(path.Dir(target))
	return nil
}

func (p *provider) Rename(ctx context.Context, oldpath, newpath string) error {
	item, err := p.item(ctx, oldpath)
	if err != nil {
		return err
	}
	target, err := p.cloudPath(newpath)
	if err != nil {
		return err
	}
	if target == "/" {
		return fmt.Errorf("拒绝移动到 115 根目录")
	}
	parentID, err := p.client.mkdirAll(ctx, path.Dir(target))
	if err != nil {
		return err
	}
	if item.ParentID != parentID {
		if err := p.client.move(ctx, item.ID, parentID); err != nil {
			return err
		}
	}
	if name := path.Base(target); name != item.Name {
		if err := p.client.rename(ctx, item.ID, name); err != nil {
			return err
		}
	}
	p.client.invalidate(item.ID, item.Path)
	p.client.invalidatePath(target)
	return nil
}

func (p *provider) Copy(ctx context.Context, oldname, newname string) error {
	item, err := p.item(ctx, oldname)
	if err != nil {
		return err
	}
	target, err := p.cloudPath(newname)
	if err != nil {
		return err
	}
	if target == "/" {
		return fmt.Errorf("拒绝复制到 115 根目录")
	}
	parentID, err := p.client.mkdirAll(ctx, path.Dir(target))
	if err != nil {
		return err
	}
	if err := p.client.copy(ctx, item.ID, parentID); err != nil {
		return err
	}
	if name := path.Base(target); name != item.Name {
		copied, err := p.client.getItem(ctx, path.Join(path.Dir(target), item.Name))
		if err != nil {
			return err
		}
		if err := p.client.rename(ctx, copied.ID, name); err != nil {
			return err
		}
	}
	p.client.invalidatePath(target)
	return nil
}

func (p *provider) Link(ctx context.Context, oldname, newname string) error {
	return fmt.Errorf("115 插件不支持硬链接")
}

func (p *provider) Symlink(ctx context.Context, oldname, newname string) error {
	return fmt.Errorf("115 插件不支持软链接")
}

func (p *provider) item(ctx context.Context, name string) (item115, error) {
	cloudPath, err := p.cloudPath(name)
	if err != nil {
		return item115{}, err
	}
	return p.client.getItem(ctx, cloudPath)
}

func (p *provider) cloudPath(name string) (string, error) {
	name = strings.TrimSpace(strings.ReplaceAll(name, "\\", "/"))
	root := cleanCloudPath(p.root)
	if name == "" || name == "." || cleanCloudPath(name) == root {
		return root, nil
	}
	if strings.HasPrefix(name, "115://") {
		name = strings.TrimPrefix(name, "115://")
		name = "/" + strings.TrimLeft(name, "/")
	}
	cleaned := cleanCloudPath(name)
	if root != "/" {
		if cleaned == root || strings.HasPrefix(cleaned, root+"/") {
			return cleaned, nil
		}
		cleaned = cleanCloudPath(path.Join(root, strings.TrimLeft(cleaned, "/")))
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("115 路径越界: %s", name)
	}
	return cleaned, nil
}

func (p *provider) cookie(ctx context.Context) (string, error) {
	if p.secrets == nil {
		return "", fmt.Errorf("115 Cookie 需要 SecretService")
	}
	cookie, err := p.secrets.Reveal(ctx, p.cookieRef, "connect 115 storage")
	if err != nil {
		return "", fmt.Errorf("读取 115 Cookie: %w", err)
	}
	if strings.TrimSpace(cookie) == "" {
		return "", fmt.Errorf("115 Cookie 为空")
	}
	return cookie, nil
}

func cleanCloudPath(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	if value == "" {
		return "/"
	}
	value = "/" + strings.TrimLeft(value, "/")
	cleaned := path.Clean(value)
	if cleaned == "." {
		return "/"
	}
	return cleaned
}

func stringConfig(value any) string {
	str, _ := value.(string)
	return str
}
