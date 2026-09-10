package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"testing"
	"time"
)

func TestEnsureRootDirectory115UsesExistingDirectory(t *testing.T) {
	created := false
	id, err := ensureRootDirectory115(context.Background(), "/media",
		func(context.Context, string) (int64, error) { return 42, nil },
		func(context.Context, string) (int64, error) {
			created = true
			return 0, nil
		},
	)
	if err != nil || id != 42 || created {
		t.Fatalf("id=%d created=%v err=%v", id, created, err)
	}
}

func TestEnsureRootDirectory115CreatesMissingDirectory(t *testing.T) {
	createdPath := ""
	id, err := ensureRootDirectory115(context.Background(), "/media/video",
		func(context.Context, string) (int64, error) { return 0, os.ErrNotExist },
		func(_ context.Context, path string) (int64, error) {
			createdPath = path
			return 84, nil
		},
	)
	if err != nil || id != 84 || createdPath != "/media/video" {
		t.Fatalf("id=%d createdPath=%q err=%v", id, createdPath, err)
	}
}

func TestEnsureRootDirectory115DoesNotCreateOnOtherErrors(t *testing.T) {
	wantErr := errors.New("cookie expired")
	created := false
	_, err := ensureRootDirectory115(context.Background(), "/media",
		func(context.Context, string) (int64, error) { return 0, wantErr },
		func(context.Context, string) (int64, error) {
			created = true
			return 0, nil
		},
	)
	if !errors.Is(err, wantErr) || created {
		t.Fatalf("created=%v err=%v", created, err)
	}
}

func TestPlaybackUserAgent115(t *testing.T) {
	if got := playbackUserAgent115(map[string]any{"user_agent": "MediaPlayer/1.0"}); got != "MediaPlayer/1.0" {
		t.Fatalf("User-Agent = %q", got)
	}
	if got := playbackUserAgent115(nil); got != userAgent115 {
		t.Fatalf("fallback User-Agent = %q", got)
	}
}

func Test115PluginReusesResidentProviders(t *testing.T) {
	if !Plugin().ReuseProviders {
		t.Fatal("115 resident plugin must reuse providers across RPC calls")
	}
}

func TestSelectDownloadURLPrefersPickcodeAndIsDeterministic(t *testing.T) {
	entry := func(raw string) downloadInfo115 {
		var info downloadInfo115
		info.URL.URL = raw
		return info
	}
	if got, err := selectDownloadURL(map[string]downloadInfo115{
		"z":      entry("https://cdn.example/z"),
		"pick-1": entry("https://cdn.example/pick"),
		"a":      entry("https://cdn.example/a"),
	}, "pick-1"); err != nil || got != "https://cdn.example/pick" {
		t.Fatalf("pickcode URL = %q, err=%v", got, err)
	}
	if got, err := selectDownloadURL(map[string]downloadInfo115{
		"z": entry("https://cdn.example/z"),
		"a": entry("https://cdn.example/a"),
	}, "missing"); err != nil || got != "https://cdn.example/a" {
		t.Fatalf("deterministic fallback URL = %q, err=%v", got, err)
	}
}

func TestPlaybackURLTTL115(t *testing.T) {
	if playbackURLTTL115 != 5*time.Minute {
		t.Fatalf("playbackURLTTL115 = %s, want 5m", playbackURLTTL115)
	}
}

func TestPlaybackResolveTimingRecordsGetIDCacheHit(t *testing.T) {
	timing := &playbackResolveTiming{}
	ctx := withPlaybackTiming(context.Background(), timing)
	client := newClient115("", &http.Client{}, nil)
	client.putID("/media", 42)
	id, err := client.getDirID(ctx, "/media")
	if err != nil || id != 42 {
		t.Fatalf("id=%d err=%v", id, err)
	}
	if timing.getIDCalls != 1 || timing.getIDCacheHits != 1 {
		t.Fatalf("timing=%#v", timing)
	}
	if timing.getIDMs < 0 || timing.listDirCalls != 0 || timing.downurlCalls != 0 {
		t.Fatalf("unexpected other stages: %#v", timing)
	}
}

func TestPlaybackResolveTimingIgnoredWithoutContext(t *testing.T) {
	client := newClient115("", &http.Client{}, nil)
	client.putID("/media", 42)
	id, err := client.getDirID(context.Background(), "/media")
	if err != nil || id != 42 {
		t.Fatalf("id=%d err=%v", id, err)
	}
}
