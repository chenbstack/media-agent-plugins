package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chenbstack/media-agent-plugin-sdk-go"
)

const (
	webAPIBase = "https://webapi.115.com"
	proAPIBase = "https://proapi.115.com"
)

type client115 struct {
	cookie string
	userID string
	kv     pluginsdk.KVStore
	http   *http.Client
	lim    *rateLimiter

	mu          sync.Mutex
	pathToID    map[string]cacheID
	idToItem    map[int64]cacheItem
	missingPath map[string]time.Time
	userKey     string
	token       ossToken115
}

type item115 struct {
	ID       int64
	ParentID int64
	Name     string
	Path     string
	Size     int64
	IsDir    bool
	ModTime  time.Time
	PickCode string
}

type cacheID struct {
	id        int64
	expiresAt time.Time
}

type cacheItem struct {
	item      item115
	expiresAt time.Time
}

type rateLimiter struct {
	mu       sync.Mutex
	interval time.Duration
	next     time.Time
}

func newClient115(cookie string, httpClient *http.Client, kv pluginsdk.KVStore) *client115 {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 60 * time.Second}
	}
	return &client115{
		cookie:      strings.TrimSpace(cookie),
		userID:      userIDFromCookie(cookie),
		kv:          kv,
		http:        httpClient,
		lim:         &rateLimiter{interval: 500 * time.Millisecond},
		pathToID:    map[string]cacheID{},
		idToItem:    map[int64]cacheItem{},
		missingPath: map[string]time.Time{},
	}
}

func (c *client115) listDir(ctx context.Context, cid int64) ([]item115, error) {
	var out []item115
	for offset := 0; ; {
		resp := struct {
			State any               `json:"state"`
			ErrNo int               `json:"errno"`
			Error string            `json:"error"`
			Msg   string            `json:"msg"`
			CID   json.Number       `json:"cid"`
			Count json.Number       `json:"count"`
			Data  []json.RawMessage `json:"data"`
		}{}
		query := url.Values{
			"aid":              {"1"},
			"cid":              {strconv.FormatInt(cid, 10)},
			"limit":            {"1150"},
			"offset":           {strconv.Itoa(offset)},
			"show_dir":         {"1"},
			"cur":              {"1"},
			"fc_mix":           {"1"},
			"asc":              {"1"},
			"o":                {"user_ptime"},
			"custom_order":     {"1"},
			"record_open_time": {"1"},
		}
		if err := c.doJSON(ctx, http.MethodGet, webAPIBase+"/files", query, nil, &resp); err != nil {
			return nil, err
		}
		if err := check115State(resp.State, resp.ErrNo, firstNonEmpty(resp.Error, resp.Msg)); err != nil {
			return nil, err
		}
		for _, raw := range resp.Data {
			item, err := parseItem(raw)
			if err != nil {
				return nil, err
			}
			out = append(out, item)
		}
		if len(resp.Data) == 0 {
			break
		}
		count, _ := resp.Count.Int64()
		offset += len(resp.Data)
		if int64(offset) >= count || len(resp.Data) < 1150 {
			break
		}
	}
	return out, nil
}

func (c *client115) probeDir(ctx context.Context, cid int64) error {
	resp := struct {
		State any    `json:"state"`
		ErrNo int    `json:"errno"`
		Error string `json:"error"`
		Msg   string `json:"msg"`
	}{}
	query := url.Values{
		"aid":      {"1"},
		"cid":      {strconv.FormatInt(cid, 10)},
		"limit":    {"1"},
		"offset":   {"0"},
		"show_dir": {"1"},
		"cur":      {"1"},
		"fc_mix":   {"1"},
	}
	if err := c.doJSON(ctx, http.MethodGet, webAPIBase+"/files", query, nil, &resp); err != nil {
		return err
	}
	return check115State(resp.State, resp.ErrNo, firstNonEmpty(resp.Error, resp.Msg))
}

func (c *client115) quota(ctx context.Context) (total int64, used int64, err error) {
	checks := []struct {
		name     string
		endpoint string
		query    url.Values
	}{
		{name: "files/index_info", endpoint: webAPIBase + "/files/index_info", query: url.Values{"aid": {"1"}}},
		{name: "user/info", endpoint: webAPIBase + "/user/info"},
		{name: "open/user/info", endpoint: proAPIBase + "/open/user/info"},
	}
	errs := make([]string, 0, len(checks))
	for _, check := range checks {
		resp := map[string]any{}
		if err := c.doJSON(ctx, http.MethodGet, check.endpoint, check.query, nil, &resp); err != nil {
			errs = append(errs, check.name+": "+err.Error())
			continue
		}
		if err := check115OpenState(resp); err != nil {
			errs = append(errs, check.name+": "+err.Error())
			continue
		}
		total, used, ok := quotaFromResponse115(resp)
		if ok {
			return total, used, nil
		}
		errs = append(errs, check.name+": 响应缺少容量字段")
	}
	return 0, 0, fmt.Errorf("115 容量接口不可用: %s", strings.Join(errs, "; "))
}

func (c *client115) getDirID(ctx context.Context, cloudPath string) (int64, error) {
	cloudPath = cleanCloudPath(cloudPath)
	if cloudPath == "/" {
		return 0, nil
	}
	if id, ok := c.cachedID(cloudPath); ok {
		return id, nil
	}
	resp := struct {
		State any         `json:"state"`
		ErrNo int         `json:"errno"`
		Error string      `json:"error"`
		Msg   string      `json:"msg"`
		ID    json.Number `json:"id"`
	}{}
	query := url.Values{"path": {cloudPath}}
	if err := c.doJSON(ctx, http.MethodGet, webAPIBase+"/files/getid", query, nil, &resp); err != nil {
		return 0, err
	}
	if err := check115State(resp.State, resp.ErrNo, firstNonEmpty(resp.Error, resp.Msg)); err != nil {
		return 0, err
	}
	id, _ := resp.ID.Int64()
	if id <= 0 {
		return 0, osNotExist(cloudPath)
	}
	c.putID(cloudPath, id)
	return id, nil
}

func (c *client115) getItem(ctx context.Context, cloudPath string) (item115, error) {
	cloudPath = cleanCloudPath(cloudPath)
	if item, ok := c.cachedItemByPath(cloudPath); ok {
		return item, nil
	}
	if c.isMissing(cloudPath) {
		return item115{}, osNotExist(cloudPath)
	}
	if cloudPath == "/" {
		item := item115{ID: 0, Name: "/", Path: "/", IsDir: true}
		c.putItem(item)
		return item, nil
	}
	if id, err := c.getDirID(ctx, cloudPath); err == nil && id > 0 {
		item := item115{ID: id, Name: path.Base(cloudPath), Path: cloudPath, IsDir: true}
		c.putItem(item)
		return item, nil
	}
	parentPath := path.Dir(cloudPath)
	parentID, err := c.getDirID(ctx, parentPath)
	if err != nil {
		c.markMissing(cloudPath)
		return item115{}, err
	}
	items, err := c.listDir(ctx, parentID)
	if err != nil {
		return item115{}, err
	}
	name := path.Base(cloudPath)
	for _, item := range items {
		item.ParentID = parentID
		item.Path = path.Join(parentPath, item.Name)
		if item.IsDir {
			item.Path = cleanCloudPath(item.Path)
		}
		c.putItem(item)
		if item.Name == name {
			return item, nil
		}
	}
	c.markMissing(cloudPath)
	return item115{}, osNotExist(cloudPath)
}

func (c *client115) mkdirAll(ctx context.Context, cloudPath string) (int64, error) {
	cloudPath = cleanCloudPath(cloudPath)
	if cloudPath == "/" {
		return 0, nil
	}
	if id, err := c.getDirID(ctx, cloudPath); err == nil {
		return id, nil
	}
	parent := int64(0)
	current := "/"
	for _, segment := range strings.Split(strings.Trim(cloudPath, "/"), "/") {
		if segment == "" {
			continue
		}
		current = cleanCloudPath(path.Join(current, segment))
		if id, err := c.getDirID(ctx, current); err == nil {
			parent = id
			continue
		}
		item, err := c.mkdir(ctx, parent, segment)
		if err != nil {
			return 0, err
		}
		item.Path = current
		c.putItem(item)
		parent = item.ID
	}
	return parent, nil
}

func (c *client115) mkdir(ctx context.Context, parentID int64, name string) (item115, error) {
	resp := struct {
		State any         `json:"state"`
		ErrNo int         `json:"errno"`
		Error string      `json:"error"`
		Msg   string      `json:"msg"`
		CID   json.Number `json:"cid"`
		ID    json.Number `json:"file_id"`
	}{}
	form := url.Values{"pid": {strconv.FormatInt(parentID, 10)}, "cname": {name}}
	if err := c.doJSON(ctx, http.MethodPost, webAPIBase+"/files/add", nil, form, &resp); err != nil {
		return item115{}, err
	}
	if err := check115State(resp.State, resp.ErrNo, firstNonEmpty(resp.Error, resp.Msg)); err != nil {
		return item115{}, err
	}
	id, _ := resp.CID.Int64()
	if id == 0 {
		id, _ = resp.ID.Int64()
	}
	if id <= 0 {
		return item115{}, fmt.Errorf("115 创建目录未返回目录 ID")
	}
	return item115{ID: id, ParentID: parentID, Name: name, IsDir: true, ModTime: time.Now()}, nil
}

func (c *client115) delete(ctx context.Context, id int64) error {
	resp := simple115Response{}
	form := url.Values{"fid": {strconv.FormatInt(id, 10)}}
	if err := c.doJSON(ctx, http.MethodPost, webAPIBase+"/rb/delete", nil, form, &resp); err != nil {
		return err
	}
	if err := check115State(resp.State, resp.ErrNo, firstNonEmpty(resp.Error, resp.Msg)); err != nil {
		return err
	}
	c.invalidate(id, "")
	return nil
}

func (c *client115) rename(ctx context.Context, id int64, name string) error {
	resp := simple115Response{}
	form := url.Values{"files_new_name[" + strconv.FormatInt(id, 10) + "]": {name}}
	if err := c.doJSON(ctx, http.MethodPost, webAPIBase+"/files/batch_rename", nil, form, &resp); err != nil {
		return err
	}
	if err := check115State(resp.State, resp.ErrNo, firstNonEmpty(resp.Error, resp.Msg)); err != nil {
		return err
	}
	c.invalidate(id, "")
	return nil
}

func (c *client115) move(ctx context.Context, id, parentID int64) error {
	resp := simple115Response{}
	form := url.Values{"fid": {strconv.FormatInt(id, 10)}, "pid": {strconv.FormatInt(parentID, 10)}}
	if err := c.doJSON(ctx, http.MethodPost, webAPIBase+"/files/move", nil, form, &resp); err != nil {
		return err
	}
	if err := check115State(resp.State, resp.ErrNo, firstNonEmpty(resp.Error, resp.Msg)); err != nil {
		return err
	}
	c.invalidate(id, "")
	return nil
}

func (c *client115) copy(ctx context.Context, id, parentID int64) error {
	resp := simple115Response{}
	form := url.Values{"fid": {strconv.FormatInt(id, 10)}, "pid": {strconv.FormatInt(parentID, 10)}}
	if err := c.doJSON(ctx, http.MethodPost, webAPIBase+"/files/copy", nil, form, &resp); err != nil {
		return err
	}
	return check115State(resp.State, resp.ErrNo, firstNonEmpty(resp.Error, resp.Msg))
}

func (c *client115) downloadURL(ctx context.Context, pickCode string) (string, map[string]string, error) {
	if pickCode == "" {
		return "", nil, fmt.Errorf("115 文件缺少 pickcode")
	}
	payload := map[string]string{"pickcode": pickCode}
	if c.userID != "" {
		payload["user_id"] = c.userID
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", nil, err
	}
	encrypted, err := rsaEncrypt115(body)
	if err != nil {
		return "", nil, err
	}
	resp := struct {
		State any             `json:"state"`
		ErrNo int             `json:"errno"`
		Error string          `json:"error"`
		Msg   string          `json:"msg"`
		Data  json.RawMessage `json:"data"`
	}{}
	form := url.Values{"data": {string(encrypted)}}
	if err := c.doJSON(ctx, http.MethodPost, proAPIBase+"/app/chrome/downurl", nil, form, &resp); err != nil {
		return "", nil, err
	}
	if err := check115State(resp.State, resp.ErrNo, firstNonEmpty(resp.Error, resp.Msg)); err != nil {
		return "", nil, err
	}
	var encryptedData string
	if err := json.Unmarshal(resp.Data, &encryptedData); err != nil {
		return "", nil, err
	}
	decrypted, err := rsaDecrypt115([]byte(encryptedData))
	if err != nil {
		return "", nil, err
	}
	var data map[string]downloadInfo115
	if err := json.Unmarshal(decrypted, &data); err != nil {
		var single struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(decrypted, &single); err != nil {
			return "", nil, err
		}
		if single.URL == "" {
			return "", nil, fmt.Errorf("115 下载链接为空")
		}
		return single.URL, map[string]string{"User-Agent": userAgent115}, nil
	}
	for _, info := range data {
		if info.URL.URL == "" {
			return "", nil, fmt.Errorf("115 下载链接为空")
		}
		return info.URL.URL, map[string]string{"User-Agent": userAgent115}, nil
	}
	return "", nil, fmt.Errorf("115 下载链接响应为空")
}

type downloadInfo115 struct {
	URL struct {
		URL string `json:"url"`
	} `json:"url"`
}

type simple115Response struct {
	State any    `json:"state"`
	ErrNo int    `json:"errno"`
	Error string `json:"error"`
	Msg   string `json:"msg"`
}

func (c *client115) doJSON(ctx context.Context, method, endpoint string, query url.Values, form url.Values, out any) error {
	if err := c.lim.acquire(ctx); err != nil {
		return err
	}
	reqURL := endpoint
	if len(query) > 0 {
		reqURL += "?" + query.Encode()
	}
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, reqURL, body)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent115)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Cookie", c.cookie)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("115 HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(out); err != nil {
		ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
		return fmt.Errorf("解析 115 响应失败(%s): %w", ct, err)
	}
	return nil
}

func parseItem(raw json.RawMessage) (item115, error) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return item115{}, err
	}
	isFile := m["fid"] != nil
	var item item115
	if isFile {
		item.ID = int64FromAny(m["fid"])
		item.ParentID = int64FromAny(m["cid"])
		item.IsDir = false
		item.Size = int64FromAny(m["s"])
		item.PickCode = stringFromAny(m["pc"])
	} else {
		item.ID = int64FromAny(m["cid"])
		item.ParentID = int64FromAny(m["pid"])
		item.IsDir = true
	}
	item.Name = firstNonEmpty(stringFromAny(m["n"]), stringFromAny(m["file_name"]))
	item.ModTime = timeFromUnixAny(firstNonEmptyAny(m["te"], m["tp"], m["t"]))
	if item.ID < 0 || item.Name == "" {
		return item115{}, fmt.Errorf("115 文件条目无效")
	}
	return item, nil
}

func check115State(state any, errno int, message string) error {
	ok := true
	switch v := state.(type) {
	case nil:
		ok = errno == 0
	case bool:
		ok = v
	case float64:
		ok = v != 0
	case string:
		ok = v == "true" || v == "1" || v == "ok"
	}
	if ok && errno == 0 {
		return nil
	}
	if message == "" {
		message = "115 请求失败"
	}
	if errno == 99 || strings.Contains(message, "登录") || strings.Contains(message, "cookie") {
		return fmt.Errorf("115 Cookie 失效或无权限: %s", message)
	}
	return fmt.Errorf("%s", message)
}

func check115OpenState(resp map[string]any) error {
	code := int64FromAny(firstNonEmptyAny(resp["code"], resp["errno"]))
	if code == 0 {
		if state, ok := resp["state"]; ok {
			return check115State(state, 0, firstNonEmpty(stringFromAny(resp["message"]), stringFromAny(resp["msg"]), stringFromAny(resp["error"])))
		}
		if state, ok := resp["success"]; ok {
			return check115State(state, 0, firstNonEmpty(stringFromAny(resp["message"]), stringFromAny(resp["msg"]), stringFromAny(resp["error"])))
		}
		return nil
	}
	message := firstNonEmpty(stringFromAny(resp["message"]), stringFromAny(resp["msg"]), stringFromAny(resp["error"]))
	return check115State(false, int(code), message)
}

func (c *client115) cachedID(cloudPath string) (int64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.pathToID[cloudPath]
	if !ok || time.Now().After(entry.expiresAt) {
		return 0, false
	}
	return entry.id, true
}

func (c *client115) cachedItemByPath(cloudPath string) (item115, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	idEntry, ok := c.pathToID[cloudPath]
	if !ok || time.Now().After(idEntry.expiresAt) {
		return item115{}, false
	}
	itemEntry, ok := c.idToItem[idEntry.id]
	if !ok || time.Now().After(itemEntry.expiresAt) {
		return item115{}, false
	}
	return itemEntry.item, true
}

func (c *client115) putID(cloudPath string, id int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pathToID[cloudPath] = cacheID{id: id, expiresAt: time.Now().Add(30 * time.Minute)}
	delete(c.missingPath, cloudPath)
}

func (c *client115) putItem(item item115) {
	item.Path = cleanCloudPath(item.Path)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pathToID[item.Path] = cacheID{id: item.ID, expiresAt: time.Now().Add(30 * time.Minute)}
	c.idToItem[item.ID] = cacheItem{item: item, expiresAt: time.Now().Add(30 * time.Minute)}
	delete(c.missingPath, item.Path)
}

func (c *client115) invalidate(id int64, cloudPath string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cloudPath != "" {
		delete(c.pathToID, cleanCloudPath(cloudPath))
	}
	if item, ok := c.idToItem[id]; ok {
		delete(c.pathToID, item.item.Path)
	}
	delete(c.idToItem, id)
}

func (c *client115) invalidatePath(cloudPath string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.pathToID, cleanCloudPath(cloudPath))
	delete(c.missingPath, cleanCloudPath(cloudPath))
}

func (c *client115) markMissing(cloudPath string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.missingPath[cleanCloudPath(cloudPath)] = time.Now().Add(15 * time.Second)
}

func (c *client115) isMissing(cloudPath string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	until, ok := c.missingPath[cleanCloudPath(cloudPath)]
	if !ok {
		return false
	}
	if time.Now().After(until) {
		delete(c.missingPath, cleanCloudPath(cloudPath))
		return false
	}
	return true
}

func (l *rateLimiter) acquire(ctx context.Context) error {
	l.mu.Lock()
	now := time.Now()
	wait := l.next.Sub(now)
	if wait <= 0 {
		l.next = now.Add(l.interval)
		l.mu.Unlock()
		return nil
	}
	l.next = l.next.Add(l.interval)
	l.mu.Unlock()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func osNotExist(name string) error {
	return fmt.Errorf("%w: %s", os.ErrNotExist, name)
}

func int64FromAny(value any) int64 {
	switch v := value.(type) {
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return n
		}
		f, _ := v.Float64()
		return int64(f)
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	case string:
		cleaned := strings.ReplaceAll(strings.TrimSpace(v), ",", "")
		if n, err := strconv.ParseInt(cleaned, 10, 64); err == nil {
			return n
		}
		f, _ := strconv.ParseFloat(cleaned, 64)
		return int64(f)
	default:
		return 0
	}
}

func stringFromAny(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case json.Number:
		return v.String()
	case float64:
		return strconv.FormatInt(int64(v), 10)
	default:
		return ""
	}
}

func mapFromAny(value any) map[string]any {
	switch v := value.(type) {
	case map[string]any:
		return v
	default:
		return nil
	}
}

func quotaSizeFromAny(value any) int64 {
	if m := mapFromAny(value); m != nil {
		return int64FromAny(firstPresentAny(m["size"], m["value"], m["bytes"]))
	}
	return int64FromAny(value)
}

func quotaFromResponse115(resp map[string]any) (total int64, used int64, ok bool) {
	space := findQuotaSpace115(resp, 0)
	if space == nil {
		return 0, 0, false
	}
	total = quotaSizeFromAny(firstPresentAny(
		space["all_total"],
		space["total"],
		space["total_size"],
		space["total_bytes"],
		space["quota"],
		space["quota_size"],
	))
	remainValue := firstPresentAny(
		space["all_remain"],
		space["all_available"],
		space["remain"],
		space["remain_size"],
		space["remaining"],
		space["free"],
		space["free_size"],
		space["available"],
		space["available_bytes"],
	)
	remain := quotaSizeFromAny(remainValue)
	used = quotaSizeFromAny(firstPresentAny(
		space["all_use"],
		space["all_used"],
		space["used"],
		space["used_size"],
		space["use"],
		space["used_bytes"],
	))
	if total <= 0 {
		return 0, 0, false
	}
	if used <= 0 && remainValue != nil {
		used = total - remain
	}
	if used < 0 {
		used = 0
	}
	if used > total {
		used = total
	}
	return total, used, true
}

func findQuotaSpace115(value any, depth int) map[string]any {
	if depth > 6 || value == nil {
		return nil
	}
	m := mapFromAny(value)
	if m == nil {
		return nil
	}
	if quotaSizeFromAny(firstPresentAny(m["all_total"], m["total"], m["total_size"], m["total_bytes"], m["quota"], m["quota_size"])) > 0 {
		return m
	}
	for _, key := range []string{"data", "rt_space_info", "space_info", "storage_info", "quota_info", "quota", "space", "user_info", "user"} {
		if found := findQuotaSpace115(m[key], depth+1); found != nil {
			return found
		}
	}
	for _, nested := range m {
		if found := findQuotaSpace115(nested, depth+1); found != nil {
			return found
		}
	}
	return nil
}

func firstPresentAny(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func firstNonEmptyAny(values ...any) any {
	for _, value := range values {
		if value != nil && stringFromAny(value) != "" {
			return value
		}
	}
	return nil
}

func timeFromUnixAny(value any) time.Time {
	sec := int64FromAny(value)
	if sec <= 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0)
}

func userIDFromCookie(cookie string) string {
	for _, part := range strings.Split(cookie, ";") {
		part = strings.TrimSpace(part)
		if !strings.HasPrefix(part, "UID=") {
			continue
		}
		value := strings.TrimPrefix(part, "UID=")
		if idx := strings.Index(value, "_"); idx >= 0 {
			value = value[:idx]
		}
		return value
	}
	return ""
}
