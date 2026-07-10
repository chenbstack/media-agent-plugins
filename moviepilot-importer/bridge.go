package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var supportedSourceTypes = []string{"rules", "sites", "subscriptions", "subscribe_history", "transfer_history"}

type moviePilotClient struct {
	baseURL  string
	username string
	password string
	token    string
	http     *http.Client
	rules    *moviePilotRuleData
}

type pingResult struct {
	MoviePilot  map[string]any `json:"moviepilot"`
	ExportTypes []string       `json:"export_types"`
}

type exportPage struct {
	Type       string       `json:"type"`
	Total      int          `json:"total"`
	NextCursor string       `json:"next_cursor"`
	Done       bool         `json:"done"`
	Items      []exportItem `json:"items"`
}

type exportItem struct {
	SourceType string          `json:"source_type"`
	SourceID   string          `json:"source_id"`
	Hash       string          `json:"hash"`
	Data       json.RawMessage `json:"data"`
}

type loginResponse struct {
	AccessToken string `json:"access_token"`
	SuperUser   bool   `json:"super_user"`
	UserName    string `json:"user_name"`
}

func newMoviePilotClient(cfg config) *moviePilotClient {
	return &moviePilotClient{
		baseURL:  normalizeBaseURL(cfg.BaseURL),
		username: cfg.Username,
		password: cfg.Password,
		http:     &http.Client{Timeout: 60 * time.Second},
	}
}

func (c *moviePilotClient) ping(ctx context.Context) (pingResult, error) {
	login, err := c.login(ctx)
	if err != nil {
		return pingResult{}, err
	}
	var env struct {
		Success bool           `json:"success"`
		Message string         `json:"message"`
		Data    map[string]any `json:"data"`
	}
	if err := c.get(ctx, "/system/global", url.Values{"token": {"moviepilot"}}, &env); err != nil {
		return pingResult{}, err
	}
	if !env.Success {
		return pingResult{}, fmt.Errorf("读取 MoviePilot 版本失败: %s", env.Message)
	}
	return pingResult{
		MoviePilot: map[string]any{
			"version":    stringValue(env.Data, "BACKEND_VERSION"),
			"username":   login.UserName,
			"super_user": login.SuperUser,
		},
		ExportTypes: append([]string(nil), supportedSourceTypes...),
	}, nil
}

func (c *moviePilotClient) login(ctx context.Context) (loginResponse, error) {
	form := url.Values{
		"username": {c.username},
		"password": {c.password},
	}
	var result loginResponse
	if err := c.request(ctx, http.MethodPost, "/login/access-token", nil, form, &result); err != nil {
		return loginResponse{}, fmt.Errorf("MoviePilot 登录失败: %w", err)
	}
	if strings.TrimSpace(result.AccessToken) == "" {
		return loginResponse{}, fmt.Errorf("MoviePilot 登录响应缺少访问令牌")
	}
	if !result.SuperUser {
		return loginResponse{}, fmt.Errorf("请使用 MoviePilot 管理员账号执行数据迁移")
	}
	c.token = result.AccessToken
	return result, nil
}

func (c *moviePilotClient) export(ctx context.Context, sourceType, cursor string, limit int) (exportPage, error) {
	if c.token == "" {
		if _, err := c.login(ctx); err != nil {
			return exportPage{}, err
		}
	}
	if limit <= 0 {
		limit = 500
	}
	if limit > 1000 {
		limit = 1000
	}
	switch sourceType {
	case "rules":
		return c.exportRules(ctx, cursor, limit)
	case "sites":
		return c.exportList(ctx, sourceType, "/site/", cursor, limit)
	case "subscriptions":
		return c.exportSubscriptions(ctx, cursor, limit)
	case "subscribe_history":
		return c.exportSubscribeHistory(ctx, cursor, limit)
	case "transfer_history":
		return c.exportTransferHistory(ctx, cursor, limit)
	default:
		return exportPage{}, fmt.Errorf("MoviePilot 不支持导出数据类型 %q", sourceType)
	}
}

func (c *moviePilotClient) exportList(ctx context.Context, sourceType, path, cursor string, limit int) (exportPage, error) {
	var rows []json.RawMessage
	if err := c.get(ctx, path, nil, &rows); err != nil {
		return exportPage{}, err
	}
	offset, err := parseCursorNumber(cursor, 0)
	if err != nil {
		return exportPage{}, err
	}
	if offset > len(rows) {
		offset = len(rows)
	}
	end := offset + limit
	if end > len(rows) {
		end = len(rows)
	}
	items, err := makeExportItems(sourceType, rows[offset:end])
	if err != nil {
		return exportPage{}, err
	}
	page := exportPage{Type: sourceType, Total: len(rows), Done: end >= len(rows), Items: items}
	if !page.Done {
		page.NextCursor = strconv.Itoa(end)
	}
	return page, nil
}

func (c *moviePilotClient) exportSubscribeHistory(ctx context.Context, cursor string, limit int) (exportPage, error) {
	kind, page, err := parseHistoryCursor(cursor)
	if err != nil {
		return exportPage{}, err
	}
	moviePilotType := "电影"
	if kind == "tv" {
		moviePilotType = "电视剧"
	}
	query := url.Values{"page": {strconv.Itoa(page)}, "count": {strconv.Itoa(limit)}}
	var rows []json.RawMessage
	if err := c.get(ctx, "/subscribe/history/"+url.PathEscape(moviePilotType), query, &rows); err != nil {
		return exportPage{}, err
	}
	items, err := makeExportItems("subscribe_history", rows)
	if err != nil {
		return exportPage{}, err
	}
	result := exportPage{Type: "subscribe_history", Items: items}
	if len(rows) == limit {
		result.NextCursor = fmt.Sprintf("%s:%d", kind, page+1)
		return result, nil
	}
	if kind == "movie" {
		result.NextCursor = "tv:1"
		return result, nil
	}
	result.Done = true
	return result, nil
}

func (c *moviePilotClient) exportTransferHistory(ctx context.Context, cursor string, limit int) (exportPage, error) {
	pageNumber, err := parseCursorNumber(cursor, 1)
	if err != nil || pageNumber < 1 {
		return exportPage{}, fmt.Errorf("无效的整理历史游标 %q", cursor)
	}
	query := url.Values{"page": {strconv.Itoa(pageNumber)}, "count": {strconv.Itoa(limit)}}
	var response struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
		Data    struct {
			List  []json.RawMessage `json:"list"`
			Total int               `json:"total"`
		} `json:"data"`
	}
	if err := c.get(ctx, "/history/transfer", query, &response); err != nil {
		return exportPage{}, err
	}
	if !response.Success {
		return exportPage{}, fmt.Errorf("读取 MoviePilot 整理历史失败: %s", response.Message)
	}
	items, err := makeExportItems("transfer_history", response.Data.List)
	if err != nil {
		return exportPage{}, err
	}
	done := len(response.Data.List) == 0 || pageNumber*limit >= response.Data.Total
	result := exportPage{Type: "transfer_history", Total: response.Data.Total, Done: done, Items: items}
	if !done {
		result.NextCursor = strconv.Itoa(pageNumber + 1)
	}
	return result, nil
}

func (c *moviePilotClient) get(ctx context.Context, path string, query url.Values, output any) error {
	return c.request(ctx, http.MethodGet, path, query, nil, output)
}

func (c *moviePilotClient) request(ctx context.Context, method, path string, query, form url.Values, output any) error {
	endpoint := c.baseURL + "/api/v1" + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	if form != nil {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if c.token != "" {
		request.Header.Set("Authorization", "Bearer "+c.token)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("连接 MoviePilot 失败: %w", err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 64<<20))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if response.StatusCode == http.StatusUnauthorized && strings.EqualFold(response.Header.Get("X-MFA-Required"), "true") {
			return fmt.Errorf("该 MoviePilot 账号启用了 MFA，当前仅支持用户名和密码登录")
		}
		return fmt.Errorf("MoviePilot 返回 %d: %s", response.StatusCode, truncate(string(responseBody), 300))
	}
	if err := json.Unmarshal(responseBody, output); err != nil {
		return fmt.Errorf("解析 MoviePilot 响应失败: %w", err)
	}
	return nil
}

func makeExportItems(sourceType string, rows []json.RawMessage) ([]exportItem, error) {
	items := make([]exportItem, 0, len(rows))
	for _, row := range rows {
		var data map[string]any
		if err := json.Unmarshal(row, &data); err != nil {
			return nil, fmt.Errorf("解析 MoviePilot %s 数据失败: %w", sourceType, err)
		}
		normalized, err := json.Marshal(data)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(normalized)
		hash := hex.EncodeToString(sum[:])
		sourceID := stringValue(data, "id")
		if sourceID == "" {
			sourceID = hash[:24]
		}
		items = append(items, exportItem{SourceType: sourceType, SourceID: sourceID, Hash: hash, Data: normalized})
	}
	return items, nil
}

func parseCursorNumber(cursor string, fallback int) (int, error) {
	if strings.TrimSpace(cursor) == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(cursor)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("无效的数据游标 %q", cursor)
	}
	return value, nil
}

func parseHistoryCursor(cursor string) (string, int, error) {
	if strings.TrimSpace(cursor) == "" {
		return "movie", 1, nil
	}
	parts := strings.Split(cursor, ":")
	if len(parts) != 2 || parts[0] != "movie" && parts[0] != "tv" {
		return "", 0, fmt.Errorf("无效的订阅历史游标 %q", cursor)
	}
	page, err := strconv.Atoi(parts[1])
	if err != nil || page < 1 {
		return "", 0, fmt.Errorf("无效的订阅历史游标 %q", cursor)
	}
	return parts[0], page, nil
}

func truncate(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}
