package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	pluginsdk "github.com/chenbstack/media-agent-plugin-sdk-go"
)

const (
	maxPatchedResponseBytes = 16 << 20
	crossOriginScriptPath   = "/__media_agent/emby302-crossorigin.js"
	healthPath              = "/__media_agent/health"
)

var (
	playbackInfoPath      = regexp.MustCompile(`(?i)(?:^|/)(?:emby/)?items/([^/]+)/playbackinfo$`)
	mediaStreamPath       = regexp.MustCompile(`(?i)(?:^|/)(?:emby/)?(videos|audio)/([^/]+)/(?:stream|[^/]+)$`)
	baseCrossOriginDouble = regexp.MustCompile(`\b\w+\.IsRemote\s*&&\s*"DirectPlay"\s*===\s*\w+\s*\?\s*null\s*:\s*"anonymous"`)
	baseCrossOriginSingle = regexp.MustCompile(`\b\w+\.IsRemote\s*&&\s*'DirectPlay'\s*===\s*\w+\s*\?\s*null\s*:\s*'anonymous'`)
	pluginCrossOrigin     = regexp.MustCompile(`&&\s*\(\w+\.crossOrigin\s*=\s*\w+\)`)
)

const crossOriginScript = `(function(){
  try {
    Object.defineProperty(HTMLMediaElement.prototype, "crossOrigin", {
      get: function(){ return null; },
      set: function(){},
      configurable: true
    });
  } catch (_) {}
  try {
    var remove = function(node){
      if (node && node.nodeType === 1 && (node.tagName === "VIDEO" || node.tagName === "AUDIO")) {
        node.removeAttribute("crossorigin");
      }
    };
    var observer = new MutationObserver(function(mutations){
      mutations.forEach(function(mutation){
        if (mutation.type === "attributes") remove(mutation.target);
        if (mutation.type === "childList") mutation.addedNodes.forEach(remove);
      });
    });
    var start = function(){
      observer.observe(document.documentElement, {attributes:true, attributeFilter:["crossorigin"], childList:true, subtree:true});
    };
    if (document.documentElement) start(); else document.addEventListener("DOMContentLoaded", start, {once:true});
  } catch (_) {}
})();`

type proxyService struct {
	upstream    *url.URL
	verifyTLS   bool
	redirectTTL time.Duration
	basePath    string
	logger      pluginsdk.Logger
	transport   *http.Transport
	proxy       *httputil.ReverseProxy
	server      *http.Server
	listener    net.Listener
	mu          sync.Mutex
	sources     map[sourceKey]sourceEntry
	redirects   map[string]redirectEntry
}

type sourceKey struct{ itemID, sourceID string }
type sourceEntry struct {
	url     string
	expires time.Time
}
type redirectEntry struct {
	url     string
	expires time.Time
}

func newProxyService(upstream *url.URL, verifyTLS bool, cacheSeconds int, logger pluginsdk.Logger) *proxyService {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyFromEnvironment
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: !verifyTLS} //nolint:gosec -- explicitly controlled by the saved Emby connection.
	service := &proxyService{
		upstream: upstream, verifyTLS: verifyTLS, redirectTTL: time.Duration(cacheSeconds) * time.Second,
		logger: logger, transport: transport, sources: map[sourceKey]sourceEntry{}, redirects: map[string]redirectEntry{},
	}
	service.proxy = service.newReverseProxy()
	return service
}

func (s *proxyService) Start(_ context.Context, options pluginsdk.HTTPServiceOptions) (pluginsdk.HTTPServiceInfo, error) {
	s.basePath = normalizeBasePath(options.BasePath)
	host := strings.Trim(strings.TrimSpace(options.ListenHost), "[]")
	if host == "" {
		host = "127.0.0.1"
	}
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return pluginsdk.HTTPServiceInfo{}, fmt.Errorf("只允许监听 loopback")
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(host, fmt.Sprint(options.ListenPort)))
	if err != nil {
		return pluginsdk.HTTPServiceInfo{}, err
	}
	s.listener = listener
	s.server = &http.Server{Handler: s, ReadHeaderTimeout: 15 * time.Second}
	go func() {
		if err := s.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) && s.logger != nil {
			s.logger.Error(context.Background(), "Emby 302 HTTP 服务异常退出", "error", err.Error())
		}
	}()
	return pluginsdk.HTTPServiceInfo{BaseURL: "http://" + listener.Addr().String(), HealthPath: healthPath}, nil
}

func (s *proxyService) Stop(ctx context.Context) error {
	if s.server == nil {
		return nil
	}
	err := s.server.Shutdown(ctx)
	s.transport.CloseIdleConnections()
	return err
}

func (s *proxyService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.stripBasePath(r) {
		http.NotFound(w, r)
		return
	}
	switch r.URL.Path {
	case healthPath:
		w.WriteHeader(http.StatusNoContent)
		return
	case crossOriginScriptPath:
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		_, _ = io.WriteString(w, crossOriginScript)
		return
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		if itemID, sourceID, ok := mediaRequest(r); ok {
			if sourceURL := s.sourceURL(itemID, sourceID); sourceURL != "" {
				// Keep the Media Agent gateway URL intact. Resolving it here would
				// turn the response into the provider's final CDN URL and discard
				// provider-supplied headers (for example the 115 Cookie), so the
				// subsequent Emby Range requests would bypass the host proxy.
				if isMediaAgentPlaybackURL(sourceURL) {
					w.Header().Set("Cache-Control", "no-store")
					http.Redirect(w, r, sourceURL, http.StatusFound)
					return
				}
				if finalURL, err := s.resolveRedirect(r.Context(), sourceURL, r.UserAgent()); err == nil {
					w.Header().Set("Cache-Control", "no-store")
					http.Redirect(w, r, finalURL, http.StatusFound)
					return
				} else if s.logger != nil {
					s.logger.Warn(r.Context(), "STRM 直链解析失败，回退 Emby 流式响应", "item_id", itemID, "error", err.Error())
				}
			}
		}
	}
	s.proxy.ServeHTTP(w, r)
}

func (s *proxyService) newReverseProxy() *httputil.ReverseProxy {
	proxy := httputil.NewSingleHostReverseProxy(s.upstream)
	baseDirector := proxy.Director
	proxy.Director = func(request *http.Request) {
		originalHost := request.Host
		baseDirector(request)
		request.Host = s.upstream.Host
		request.Header.Set("X-Forwarded-Host", originalHost)
		if s.basePath != "" {
			request.Header.Set("X-Forwarded-Prefix", s.basePath)
		}
		request.Header.Set("Accept-Encoding", "identity")
	}
	proxy.Transport = s.transport
	proxy.ModifyResponse = s.modifyResponse
	proxy.ErrorHandler = func(w http.ResponseWriter, request *http.Request, err error) {
		if s.logger != nil {
			s.logger.Error(request.Context(), "Emby 反向代理失败", "path", request.URL.Path, "error", err.Error())
		}
		http.Error(w, "Emby 暂不可用", http.StatusBadGateway)
	}
	return proxy
}

func (s *proxyService) modifyResponse(response *http.Response) error {
	s.rewriteLocation(response)
	if response.StatusCode != http.StatusOK || response.Request.Method == http.MethodHead {
		return nil
	}
	pathLower := strings.ToLower(response.Request.URL.Path)
	contentType := strings.ToLower(response.Header.Get("Content-Type"))
	switch {
	case playbackInfoPath.MatchString(response.Request.URL.Path) && strings.Contains(contentType, "json"):
		return s.patchPlaybackInfo(response)
	case strings.HasSuffix(pathLower, "/web/modules/htmlvideoplayer/basehtmlplayer.js"):
		return patchTextResponse(response, patchBaseHTMLPlayer)
	case strings.HasSuffix(pathLower, "/web/modules/htmlvideoplayer/plugin.js"):
		return patchTextResponse(response, patchHTMLVideoPlugin)
	case strings.Contains(contentType, "text/html"):
		return patchTextResponse(response, func(source string) (string, bool) {
			prefixed, changed := prefixHTMLRootReferences(source, s.basePath)
			injected, injectedChanged := injectCrossOriginScriptAt(prefixed, s.prefixedPath(crossOriginScriptPath))
			return injected, changed || injectedChanged
		})
	default:
		return nil
	}
}

func (s *proxyService) patchPlaybackInfo(response *http.Response) error {
	body, err := readResponseBody(response)
	if err != nil {
		return err
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		replaceResponseBody(response, body, false)
		return nil
	}
	match := playbackInfoPath.FindStringSubmatch(response.Request.URL.Path)
	if len(match) < 2 {
		replaceResponseBody(response, body, false)
		return nil
	}
	itemID := match[1]
	sources, _ := payload["MediaSources"].([]any)
	changed := false
	now := time.Now()
	s.mu.Lock()
	for _, raw := range sources {
		source, _ := raw.(map[string]any)
		pathValue, _ := source["Path"].(string)
		protocol, _ := source["Protocol"].(string)
		remote, _ := source["IsRemote"].(bool)
		if !remote || !strings.EqualFold(protocol, "Http") || !isHTTPURL(pathValue) {
			continue
		}
		sourceID, _ := source["Id"].(string)
		s.sources[sourceKey{itemID: itemID, sourceID: sourceID}] = sourceEntry{url: pathValue, expires: now.Add(5 * time.Minute)}
		source["SupportsDirectPlay"] = true
		source["SupportsDirectStream"] = true
		source["SupportsTranscoding"] = false
		delete(source, "TranscodingUrl")
		delete(source, "TranscodingContainer")
		delete(source, "TranscodingSubProtocol")
		mediaType, _ := source["Type"].(string)
		kind := "videos"
		if strings.EqualFold(mediaType, "Audio") {
			kind = "audio"
		}
		query := url.Values{"Static": {"true"}, "MediaSourceId": {sourceID}}
		source["DirectStreamUrl"] = "/" + kind + "/" + url.PathEscape(itemID) + "/stream?" + query.Encode()
		changed = true
	}
	s.pruneLocked(now)
	s.mu.Unlock()
	if !changed {
		replaceResponseBody(response, body, false)
		return nil
	}
	patched, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	replaceResponseBody(response, patched, true)
	return nil
}

func (s *proxyService) sourceURL(itemID, sourceID string) string {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	if entry, ok := s.sources[sourceKey{itemID: itemID, sourceID: sourceID}]; ok {
		return entry.url
	}
	if sourceID == "" {
		for key, entry := range s.sources {
			if key.itemID == itemID {
				return entry.url
			}
		}
	}
	return ""
}

func (s *proxyService) resolveRedirect(ctx context.Context, rawURL, userAgent string) (string, error) {
	cacheKey := rawURL + "\x00" + userAgent
	now := time.Now()
	s.mu.Lock()
	if entry, ok := s.redirects[cacheKey]; ok && now.Before(entry.expires) {
		s.mu.Unlock()
		return entry.url, nil
	}
	s.mu.Unlock()
	isGateway := isMediaAgentPlaybackURL(rawURL)
	if isGateway {
		u, err := url.Parse(rawURL)
		if err != nil {
			return "", err
		}
		query := u.Query()
		if strings.TrimSpace(query.Get("redirect")) == "" {
			query.Set("redirect", "1")
		}
		u.RawQuery = query.Encode()
		rawURL = u.String()
	}
	client := &http.Client{Transport: s.transport, Timeout: 25 * time.Second}
	if isGateway {
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	resolve := func(method string) (string, error) {
		request, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
		if err != nil {
			return "", err
		}
		request.Header.Set("User-Agent", userAgent)
		if method == http.MethodGet {
			request.Header.Set("Range", "bytes=0-0")
		}
		response, err := client.Do(request)
		if err != nil {
			return "", err
		}
		defer response.Body.Close()
		if isGateway {
			if response.StatusCode < 300 || response.StatusCode >= 400 {
				return "", fmt.Errorf("网关 HTTP %d", response.StatusCode)
			}
			location, err := response.Location()
			if err != nil {
				return "", fmt.Errorf("网关未返回 Location")
			}
			finalURL := location.String()
			if !isHTTPURL(finalURL) {
				return "", fmt.Errorf("最终地址无效")
			}
			return finalURL, nil
		}
		if response.StatusCode < 200 || response.StatusCode >= 400 {
			return "", fmt.Errorf("HTTP %d", response.StatusCode)
		}
		finalURL := response.Request.URL.String()
		if !isHTTPURL(finalURL) {
			return "", fmt.Errorf("最终地址无效")
		}
		return finalURL, nil
	}
	finalURL, err := resolve(http.MethodHead)
	if err != nil && !isGateway {
		finalURL, err = resolve(http.MethodGet)
	}
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.redirects[cacheKey] = redirectEntry{url: finalURL, expires: now.Add(s.redirectTTL)}
	s.pruneLocked(now)
	s.mu.Unlock()
	return finalURL, nil
}

func isMediaAgentPlaybackURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return false
	}
	return strings.HasSuffix(strings.TrimRight(u.Path, "/"), "/api/v1/play/redirect")
}

func (s *proxyService) pruneLocked(now time.Time) {
	for key, entry := range s.sources {
		if now.After(entry.expires) {
			delete(s.sources, key)
		}
	}
	for key, entry := range s.redirects {
		if now.After(entry.expires) {
			delete(s.redirects, key)
		}
	}
}

func (s *proxyService) rewriteLocation(response *http.Response) {
	raw := strings.TrimSpace(response.Header.Get("Location"))
	if raw == "" {
		return
	}
	location, err := url.Parse(raw)
	if err != nil || !location.IsAbs() || !strings.EqualFold(location.Host, s.upstream.Host) {
		return
	}
	location.Scheme = response.Request.Header.Get("X-Forwarded-Proto")
	if location.Scheme == "" {
		location.Scheme = "http"
	}
	location.Host = response.Request.Header.Get("X-Forwarded-Host")
	if s.basePath != "" {
		location.Path = s.basePath + "/" + strings.TrimPrefix(location.Path, "/")
	}
	response.Header.Set("Location", location.String())
}

func normalizeBasePath(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || value == "/" {
		return ""
	}
	return "/" + strings.Trim(value, "/")
}

func (s *proxyService) stripBasePath(request *http.Request) bool {
	if s.basePath == "" {
		return true
	}
	if request.URL.Path != s.basePath && !strings.HasPrefix(request.URL.Path, s.basePath+"/") {
		return false
	}
	request.URL.Path = strings.TrimPrefix(request.URL.Path, s.basePath)
	if request.URL.Path == "" {
		request.URL.Path = "/"
	}
	request.URL.RawPath = ""
	return true
}

func (s *proxyService) prefixedPath(path string) string {
	if s.basePath == "" {
		return path
	}
	return s.basePath + "/" + strings.TrimPrefix(path, "/")
}

func prefixHTMLRootReferences(source, basePath string) (string, bool) {
	if basePath == "" {
		return source, false
	}
	changed := false
	for _, attribute := range []string{`src="/`, `href="/`, `action="/`, `src='/`, `href='/`, `action='/`} {
		prefixed := attribute + strings.TrimPrefix(basePath, "/") + "/"
		if strings.Contains(source, prefixed) {
			continue
		}
		updated := strings.ReplaceAll(source, attribute, prefixed)
		changed = changed || updated != source
		source = updated
	}
	return source, changed
}

func patchTextResponse(response *http.Response, patch func(string) (string, bool)) error {
	body, err := readResponseBody(response)
	if err != nil {
		return err
	}
	patched, changed := patch(string(body))
	replaceResponseBody(response, []byte(patched), changed)
	return nil
}

func patchBaseHTMLPlayer(source string) (string, bool) {
	patched := baseCrossOriginDouble.ReplaceAllString(source, "null")
	patched = baseCrossOriginSingle.ReplaceAllString(patched, "null")
	return patched, patched != source
}

func patchHTMLVideoPlugin(source string) (string, bool) {
	patched := pluginCrossOrigin.ReplaceAllString(source, "")
	return patched, patched != source
}

func injectCrossOriginScript(source string) (string, bool) {
	return injectCrossOriginScriptAt(source, crossOriginScriptPath)
}

func injectCrossOriginScriptAt(source, scriptPath string) (string, bool) {
	if strings.Contains(source, scriptPath) {
		return source, false
	}
	tag := `<script data-media-agent-emby302 src="` + scriptPath + `"></script>`
	lower := strings.ToLower(source)
	if index := strings.Index(lower, "</head>"); index >= 0 {
		return source[:index] + tag + source[index:], true
	}
	if index := strings.Index(lower, "<head"); index >= 0 {
		if end := strings.Index(source[index:], ">"); end >= 0 {
			position := index + end + 1
			return source[:position] + tag + source[position:], true
		}
	}
	return source, false
}

func readResponseBody(response *http.Response) ([]byte, error) {
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxPatchedResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxPatchedResponseBytes {
		return nil, fmt.Errorf("可改写响应超过 %d 字节", maxPatchedResponseBytes)
	}
	return body, nil
}

func replaceResponseBody(response *http.Response, body []byte, changed bool) {
	response.Body = io.NopCloser(bytes.NewReader(body))
	response.ContentLength = int64(len(body))
	response.Header.Set("Content-Length", fmt.Sprint(len(body)))
	response.Header.Del("Content-Encoding")
	if changed {
		response.Header.Set("Cache-Control", "no-cache, no-store, must-revalidate")
		response.Header.Del("ETag")
		response.Header.Del("Last-Modified")
	}
}

func mediaRequest(request *http.Request) (itemID, sourceID string, ok bool) {
	match := mediaStreamPath.FindStringSubmatch(request.URL.Path)
	if len(match) < 3 {
		return "", "", false
	}
	sourceID = request.URL.Query().Get("MediaSourceId")
	if sourceID == "" {
		sourceID = request.URL.Query().Get("mediaSourceId")
	}
	return match[2], sourceID, true
}

func isHTTPURL(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && parsed.Hostname() != "" && (parsed.Scheme == "http" || parsed.Scheme == "https")
}
