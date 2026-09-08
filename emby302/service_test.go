package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	pluginsdk "github.com/chenbstack/media-agent-plugin-sdk-go"
)

type fakeConnections struct{ connection pluginsdk.Connection }

func (f fakeConnections) ListConnections(context.Context, string) ([]pluginsdk.Connection, error) {
	return []pluginsdk.Connection{f.connection}, nil
}
func (f fakeConnections) GetConnection(context.Context, string, string) (pluginsdk.Connection, error) {
	return f.connection, nil
}
func (fakeConnections) UpsertConnection(context.Context, pluginsdk.ConnectionWrite) (pluginsdk.HostWriteResult, error) {
	return pluginsdk.HostWriteResult{}, nil
}

func TestPluginDeclaresHTTPService(t *testing.T) {
	plugin := Plugin()
	if !plugin.HasExactCapability(pluginsdk.CapabilityHTTPService) || plugin.NewHTTPService == nil {
		t.Fatal("Emby 302 must expose service.http")
	}
	if len(plugin.Manifest.HTTPServices) != 1 || plugin.Manifest.HTTPServices[0].PublicHostConfigField != "" {
		t.Fatalf("http services = %+v", plugin.Manifest.HTTPServices)
	}
	if len(plugin.IconSVG) == 0 {
		t.Fatal("Emby 302 must include an icon")
	}
}

func TestProxyPatchesWebAndRedirectsRemoteSTRM(t *testing.T) {
	const userAgent = "EmbyWeb/1.0"
	var redirectBase string
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.UserAgent() != userAgent {
			t.Errorf("CDN User-Agent = %q", r.UserAgent())
		}
		w.Header().Set("Content-Type", "video/mp4")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("video"))
	}))
	defer cdn.Close()
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.UserAgent() != userAgent {
			t.Errorf("gateway User-Agent = %q", r.UserAgent())
		}
		if r.URL.Query().Get("redirect") != "1" {
			t.Errorf("gateway redirect query = %q", r.URL.Query().Get("redirect"))
		}
		http.Redirect(w, r, cdn.URL+"/movie.mp4", http.StatusFound)
	}))
	defer gateway.Close()
	redirectBase = gateway.URL + "/api/v1/play/redirect?sig=s&storage_id=s&path=p"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch strings.ToLower(r.URL.Path) {
		case "/items/item-1/playbackinfo":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"MediaSources":[{"Id":"source-1","Type":"Video","Path":"`+redirectBase+`","Protocol":"Http","IsRemote":true,"SupportsTranscoding":true,"TranscodingUrl":"/transcode"}]}`)
		case "/web/index.html":
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, "<html><head></head><body></body></html>")
		case "/web/modules/htmlvideoplayer/basehtmlplayer.js":
			w.Header().Set("Content-Type", "application/javascript")
			_, _ = io.WriteString(w, `return x.IsRemote&&"DirectPlay"===mode?null:"anonymous"`)
		case "/web/modules/htmlvideoplayer/plugin.js":
			w.Header().Set("Content-Type", "application/javascript")
			_, _ = io.WriteString(w, `ready&&(elem.crossOrigin=initialSubtitleStream)`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	upstreamURL, _ := url.Parse(upstream.URL)
	proxy := httptest.NewServer(newProxyService(upstreamURL, true, 60, nil))
	defer proxy.Close()

	response, err := http.Get(proxy.URL + "/Items/item-1/PlaybackInfo")
	if err != nil {
		t.Fatal(err)
	}
	var playback map[string]any
	if err := json.NewDecoder(response.Body).Decode(&playback); err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	source := playback["MediaSources"].([]any)[0].(map[string]any)
	if source["SupportsTranscoding"] != false || source["SupportsDirectPlay"] != true || source["DirectStreamUrl"] != "/videos/item-1/stream?MediaSourceId=source-1&Static=true" {
		t.Fatalf("patched source = %+v", source)
	}

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request, _ := http.NewRequest(http.MethodGet, proxy.URL+"/videos/item-1/stream?MediaSourceId=source-1", nil)
	request.Header.Set("User-Agent", userAgent)
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusFound || response.Header.Get("Location") != redirectBase {
		t.Fatalf("redirect = %d %q", response.StatusCode, response.Header.Get("Location"))
	}

	assertBodyContains(t, proxy.URL+"/web/index.html", crossOriginScriptPath)
	assertBodyNotContains(t, proxy.URL+"/web/modules/htmlvideoplayer/basehtmlplayer.js", "anonymous")
	assertBodyNotContains(t, proxy.URL+"/web/modules/htmlvideoplayer/plugin.js", "crossOrigin")
	assertBodyContains(t, proxy.URL+crossOriginScriptPath, "HTMLMediaElement.prototype")
}

func TestProxyStripsHostManagedBasePath(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	upstreamURL, _ := url.Parse(upstream.URL)
	proxyService := newProxyService(upstreamURL, true, 60, nil)
	proxyService.basePath = "/api/v1/plugins/emby302/emby"
	proxy := httptest.NewServer(proxyService)
	defer proxy.Close()

	response, err := http.Get(proxy.URL + proxyService.basePath + "/web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNoContent || gotPath != "/web/index.html" {
		t.Fatalf("status=%d upstream path=%q", response.StatusCode, gotPath)
	}
}

func TestInjectCrossOriginScriptUsesBasePath(t *testing.T) {
	source, changed := injectCrossOriginScriptAt("<html><head></head></html>", "/api/v1/plugins/emby302/emby/__media_agent/emby302-crossorigin.js")
	if !changed || !strings.Contains(source, `src="/api/v1/plugins/emby302/emby/__media_agent/emby302-crossorigin.js"`) {
		t.Fatalf("source = %q", source)
	}
}

func TestPrefixHTMLRootReferences(t *testing.T) {
	source, changed := prefixHTMLRootReferences(`<script src="/web/app.js"></script><link href="/web/app.css">`, "/api/v1/plugins/emby302/emby")
	if !changed || !strings.Contains(source, `src="/api/v1/plugins/emby302/emby/web/app.js"`) || !strings.Contains(source, `href="/api/v1/plugins/emby302/emby/web/app.css"`) {
		t.Fatalf("source = %q", source)
	}
}

func TestNewHTTPServiceUsesSavedEmbyConnection(t *testing.T) {
	service, err := newHTTPService(t.Context(), pluginsdk.Instance{
		Config:      map[string]any{"emby_connection_id": "emby-1", "redirect_cache_seconds": float64(30)},
		Connections: fakeConnections{connection: pluginsdk.Connection{ID: "emby-1", Kind: "emby", Enabled: true, Config: map[string]any{"base_url": "http://127.0.0.1:8096", "verify_tls": false}}},
	}, nil, "emby")
	if err != nil {
		t.Fatal(err)
	}
	proxy := service.(*proxyService)
	if proxy.upstream.String() != "http://127.0.0.1:8096" || proxy.verifyTLS || proxy.redirectTTL != 30*time.Second {
		t.Fatalf("proxy = %+v", proxy)
	}
}

func assertBodyContains(t *testing.T, target, expected string) {
	t.Helper()
	response, err := http.Get(target)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if !strings.Contains(string(body), expected) {
		t.Fatalf("%s missing %q: %s", target, expected, body)
	}
}

func assertBodyNotContains(t *testing.T, target, unwanted string) {
	t.Helper()
	response, err := http.Get(target)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if strings.Contains(string(body), unwanted) {
		t.Fatalf("%s contains %q: %s", target, unwanted, body)
	}
}
