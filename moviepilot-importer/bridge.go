package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const bridgeProtocol = "media-agent-moviepilot-bridge.v1"

type bridgeClient struct {
	baseURL string
	token   string
	http    *http.Client
}

type pingResult struct {
	Success     bool           `json:"success"`
	Protocol    string         `json:"protocol"`
	Plugin      map[string]any `json:"plugin"`
	MoviePilot  map[string]any `json:"moviepilot"`
	ExportTypes []string       `json:"export_types"`
}

type snapshot struct {
	Protocol string           `json:"protocol"`
	Types    []snapshotSource `json:"types"`
}

type snapshotSource struct {
	Type  string `json:"type"`
	Count int    `json:"count"`
	Error string `json:"error"`
}

type exportPage struct {
	Protocol   string       `json:"protocol"`
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

func newBridgeClient(cfg config) *bridgeClient {
	return &bridgeClient{
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		token:   cfg.BridgeToken,
		http:    &http.Client{Timeout: 60 * time.Second},
	}
}

func (c *bridgeClient) ping(ctx context.Context) (pingResult, error) {
	var result pingResult
	if err := c.get(ctx, "/ping", nil, &result); err != nil {
		return pingResult{}, err
	}
	if result.Protocol != bridgeProtocol {
		return pingResult{}, fmt.Errorf("迁移桥协议不兼容: %s", result.Protocol)
	}
	return result, nil
}

func (c *bridgeClient) snapshot(ctx context.Context) (snapshot, error) {
	var result snapshot
	if err := c.get(ctx, "/snapshot", nil, &result); err != nil {
		return snapshot{}, err
	}
	if result.Protocol != bridgeProtocol {
		return snapshot{}, fmt.Errorf("迁移桥协议不兼容: %s", result.Protocol)
	}
	return result, nil
}

func (c *bridgeClient) export(ctx context.Context, sourceType, cursor string, limit int) (exportPage, error) {
	query := url.Values{"type": {sourceType}, "limit": {fmt.Sprintf("%d", limit)}}
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	var page exportPage
	if err := c.get(ctx, "/export", query, &page); err != nil {
		return exportPage{}, err
	}
	if page.Protocol != bridgeProtocol {
		return exportPage{}, fmt.Errorf("迁移桥协议不兼容: %s", page.Protocol)
	}
	return page, nil
}

func (c *bridgeClient) get(ctx context.Context, path string, query url.Values, output any) error {
	endpoint := c.baseURL + "/api/v1/plugin/MediaAgentBridge" + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	request.Header.Set("X-Media-Agent-Bridge-Token", c.token)
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("连接 MoviePilot 失败: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("MoviePilot 返回 %d: %s", response.StatusCode, truncate(string(body), 300))
	}
	if err := json.Unmarshal(body, output); err != nil {
		return fmt.Errorf("解析 MoviePilot 响应失败: %w", err)
	}
	return nil
}

func truncate(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}
