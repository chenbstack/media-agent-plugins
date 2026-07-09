package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"media-agent-lab/server/pkg/pluginsdk"
)

const (
	qrAuthFlow115      = "qrcode"
	qrAuthTokenApp115  = "web"
	qrAuthCookieApp115 = "alipaymini"
	qrAuthTTL115       = 5 * time.Minute
	qrAPIBase115       = "https://qrcodeapi.115.com"
)

type qrAuthSession115 struct {
	UID       string    `json:"uid"`
	Time      string    `json:"time"`
	Sign      string    `json:"sign"`
	App       string    `json:"app"`
	CodeURL   string    `json:"code_url"`
	ExpiresAt time.Time `json:"expires_at"`
}

func startAuth(ctx context.Context, inst pluginsdk.Instance, flow string) (pluginsdk.AuthStartResult, error) {
	if flow != qrAuthFlow115 {
		return pluginsdk.AuthStartResult{}, fmt.Errorf("115 不支持认证流程 %q", flow)
	}
	if inst.KV == nil {
		return pluginsdk.AuthStartResult{}, fmt.Errorf("插件 KV 未启用，无法保存扫码会话")
	}
	token, err := requestQRCodeToken115(ctx)
	if err != nil {
		return pluginsdk.AuthStartResult{}, err
	}
	sessionID, err := randomSessionID115()
	if err != nil {
		return pluginsdk.AuthStartResult{}, err
	}
	expiresAt := time.Now().UTC().Add(qrAuthTTL115)
	session := qrAuthSession115{
		UID:       token.UID,
		Time:      token.Time,
		Sign:      token.Sign,
		App:       qrAuthCookieApp115,
		CodeURL:   token.CodeURL,
		ExpiresAt: expiresAt,
	}
	if err := inst.KV.Set(ctx, qrAuthSessionKey115(sessionID), session, qrAuthTTL115); err != nil {
		return pluginsdk.AuthStartResult{}, fmt.Errorf("保存 115 扫码会话: %w", err)
	}
	return pluginsdk.AuthStartResult{
		Flow:        qrAuthFlow115,
		SessionID:   sessionID,
		CodeContent: "https://115.com/scan/dg-" + token.UID,
		CodeURL:     token.CodeURL,
		ExpiresAt:   expiresAt.Format(time.RFC3339),
		Message:     "请使用 115 手机客户端扫码确认登录",
	}, nil
}

func checkAuth(ctx context.Context, inst pluginsdk.Instance, flow, sessionID string) (pluginsdk.AuthCheckResult, error) {
	if flow != qrAuthFlow115 {
		return pluginsdk.AuthCheckResult{}, fmt.Errorf("115 不支持认证流程 %q", flow)
	}
	if inst.KV == nil {
		return pluginsdk.AuthCheckResult{}, fmt.Errorf("插件 KV 未启用，无法读取扫码会话")
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return pluginsdk.AuthCheckResult{}, fmt.Errorf("扫码会话 ID 不能为空")
	}
	var session qrAuthSession115
	ok, err := inst.KV.Get(ctx, qrAuthSessionKey115(sessionID), &session)
	if err != nil {
		return pluginsdk.AuthCheckResult{}, fmt.Errorf("读取 115 扫码会话: %w", err)
	}
	if !ok || session.UID == "" {
		return pluginsdk.AuthCheckResult{Status: "expired", Message: "扫码会话已过期，请重新扫码"}, nil
	}
	if !session.ExpiresAt.IsZero() && time.Now().After(session.ExpiresAt) {
		_ = inst.KV.Delete(ctx, qrAuthSessionKey115(sessionID))
		return pluginsdk.AuthCheckResult{Status: "expired", Message: "二维码已过期，请重新扫码"}, nil
	}
	status, message, err := requestQRCodeStatus115(ctx, session)
	if err != nil {
		return pluginsdk.AuthCheckResult{}, err
	}
	switch status {
	case 0:
		return pluginsdk.AuthCheckResult{Status: "pending", Message: firstNonEmpty(message, "等待扫码")}, nil
	case 1:
		return pluginsdk.AuthCheckResult{Status: "scanned", Message: firstNonEmpty(message, "已扫码，请在手机上确认登录")}, nil
	case 2:
		cookie, err := requestQRCodeCookie115(ctx, session.UID, session.App)
		if err != nil {
			return pluginsdk.AuthCheckResult{}, err
		}
		_ = inst.KV.Delete(ctx, qrAuthSessionKey115(sessionID))
		return pluginsdk.AuthCheckResult{
			Status:  "completed",
			Message: "扫码登录成功，请保存存储配置",
			Config:  map[string]any{"cookie": cookie},
		}, nil
	case -1:
		_ = inst.KV.Delete(ctx, qrAuthSessionKey115(sessionID))
		return pluginsdk.AuthCheckResult{Status: "expired", Message: firstNonEmpty(message, "二维码已过期，请重新扫码")}, nil
	case -2:
		_ = inst.KV.Delete(ctx, qrAuthSessionKey115(sessionID))
		return pluginsdk.AuthCheckResult{Status: "canceled", Message: firstNonEmpty(message, "扫码登录已取消")}, nil
	default:
		return pluginsdk.AuthCheckResult{Status: "error", Message: firstNonEmpty(message, fmt.Sprintf("未知扫码状态: %d", status))}, nil
	}
}

type qrToken115 struct {
	UID     string
	Time    string
	Sign    string
	CodeURL string
}

func requestQRCodeToken115(ctx context.Context) (qrToken115, error) {
	var resp struct {
		State any    `json:"state"`
		ErrNo int    `json:"errno"`
		Error string `json:"error"`
		Msg   string `json:"msg"`
		Data  struct {
			UID    string `json:"uid"`
			Time   any    `json:"time"`
			Sign   string `json:"sign"`
			QRCode string `json:"qrcode"`
		} `json:"data"`
	}
	endpoint := qrAPIBase115 + "/api/1.0/" + qrAuthTokenApp115 + "/1.0/token/"
	if err := do115AuthJSON(ctx, http.MethodGet, endpoint, nil, nil, &resp); err != nil {
		return qrToken115{}, err
	}
	if err := check115State(resp.State, resp.ErrNo, firstNonEmpty(resp.Error, resp.Msg)); err != nil {
		return qrToken115{}, err
	}
	token := qrToken115{
		UID:  strings.TrimSpace(resp.Data.UID),
		Time: strings.TrimSpace(stringFromAny(resp.Data.Time)),
		Sign: strings.TrimSpace(resp.Data.Sign),
	}
	if token.UID == "" || token.Time == "" || token.Sign == "" {
		return qrToken115{}, fmt.Errorf("115 扫码 token 响应缺少 uid/time/sign")
	}
	token.CodeURL = strings.TrimSpace(resp.Data.QRCode)
	if token.CodeURL == "" {
		token.CodeURL = qrAPIBase115 + "/api/1.0/" + qrAuthTokenApp115 + "/1.0/qrcode?uid=" + url.QueryEscape(token.UID)
	}
	return token, nil
}

func requestQRCodeStatus115(ctx context.Context, session qrAuthSession115) (int64, string, error) {
	var resp struct {
		State  any             `json:"state"`
		ErrNo  int             `json:"errno"`
		Error  string          `json:"error"`
		Msg    string          `json:"msg"`
		Status any             `json:"status"`
		Code   any             `json:"code"`
		Data   json.RawMessage `json:"data"`
	}
	query := url.Values{
		"uid":  {session.UID},
		"time": {session.Time},
		"sign": {session.Sign},
	}
	if err := do115AuthJSON(ctx, http.MethodGet, qrAPIBase115+"/get/status/", query, nil, &resp); err != nil {
		return 0, "", err
	}
	message := firstNonEmpty(resp.Error, resp.Msg)
	if status, ok := int64Value115(resp.Status); ok {
		return status, message, nil
	}
	if len(resp.Data) > 0 && string(resp.Data) != "null" {
		var data map[string]any
		if err := json.Unmarshal(resp.Data, &data); err == nil {
			message = firstNonEmpty(message, stringFromAny(data["message"]), stringFromAny(data["msg"]), stringFromAny(data["error"]))
			if status, ok := int64Value115(data["status"]); ok {
				return status, message, nil
			}
			if status, ok := int64Value115(data["code"]); ok {
				return status, message, nil
			}
		}
	}
	if status, ok := int64Value115(resp.Code); ok {
		return status, message, nil
	}
	if err := check115State(resp.State, resp.ErrNo, message); err != nil {
		return 0, "", err
	}
	return 0, message, nil
}

func requestQRCodeCookie115(ctx context.Context, uid, app string) (string, error) {
	if strings.TrimSpace(app) == "" {
		app = qrAuthCookieApp115
	}
	var resp struct {
		State any    `json:"state"`
		ErrNo int    `json:"errno"`
		Error string `json:"error"`
		Msg   string `json:"msg"`
		Data  struct {
			Cookie any `json:"cookie"`
		} `json:"data"`
	}
	endpoint := qrAPIBase115 + "/app/1.0/" + url.PathEscape(app) + "/1.0/login/qrcode/"
	form := url.Values{"account": {uid}}
	if err := do115AuthJSON(ctx, http.MethodPost, endpoint, nil, form, &resp); err != nil {
		return "", err
	}
	if err := check115State(resp.State, resp.ErrNo, firstNonEmpty(resp.Error, resp.Msg)); err != nil {
		return "", err
	}
	cookie := cookieHeaderFromAny115(resp.Data.Cookie)
	if cookie == "" {
		return "", fmt.Errorf("115 扫码登录未返回 Cookie")
	}
	return cookie, nil
}

func do115AuthJSON(ctx context.Context, method, endpoint string, query url.Values, form url.Values, out any) error {
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
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("115 扫码接口 HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(out); err != nil {
		return fmt.Errorf("解析 115 扫码响应失败: %w", err)
	}
	return nil
}

func cookieHeaderFromAny115(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			val := strings.TrimSpace(stringFromAny(v[key]))
			if key == "" || val == "" {
				continue
			}
			parts = append(parts, key+"="+val)
		}
		return strings.Join(parts, "; ")
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			if part := cookieHeaderFromAny115(item); part != "" {
				parts = append(parts, part)
			}
		}
		return strings.Join(parts, "; ")
	default:
		return ""
	}
}

func int64Value115(value any) (int64, bool) {
	switch v := value.(type) {
	case nil:
		return 0, false
	case json.Number:
		n, err := v.Int64()
		return n, err == nil
	case float64:
		return int64(v), true
	case int64:
		return v, true
	case int:
		return int64(v), true
	case string:
		if strings.TrimSpace(v) == "" {
			return 0, false
		}
		var n int64
		_, err := fmt.Sscan(strings.TrimSpace(v), &n)
		return n, err == nil
	default:
		return 0, false
	}
}

func randomSessionID115() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func qrAuthSessionKey115(sessionID string) string {
	return "auth:qrcode:" + sessionID
}
