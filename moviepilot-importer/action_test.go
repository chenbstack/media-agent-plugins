package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chenbstack/media-agent-plugin-sdk-go"
	runtimesdk "github.com/chenbstack/media-agent-plugin-sdk-go/runtime"
)

func TestConnectionActionWritesSanitizedLogs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/login/access-token":
			_, _ = io.WriteString(w, `{"access_token":"mp-token","super_user":true,"user_name":"admin"}`)
		case "/api/v1/system/global":
			_, _ = io.WriteString(w, `{"success":true,"data":{"BACKEND_VERSION":"2.9.11"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	logger := &recordingLogger{}
	handler := actionHandler{
		instance: pluginsdk.Instance{
			Config:  map[string]any{"base_url": server.URL, "username": "admin", "password": "secret-ref"},
			Runtime: &runtimesdk.Services{Feedback: logger},
		},
		secrets: staticSecretResolver("top-secret-password"),
	}
	result, err := handler.RunAction(context.Background(), "test", nil)
	if err != nil {
		t.Fatalf("RunAction(test): %v", err)
	}
	if result.Message != "MoviePilot 登录连接正常" {
		t.Fatalf("message = %q", result.Message)
	}
	logs := logger.String()
	if !strings.Contains(logs, "开始测试 MoviePilot 连接") || !strings.Contains(logs, "MoviePilot 连接测试成功") {
		t.Fatalf("missing connection logs: %s", logs)
	}
	if strings.Contains(logs, "top-secret-password") || strings.Contains(logs, "secret-ref") {
		t.Fatalf("connection logs leaked a secret: %s", logs)
	}
}

func TestSafeLogURLRemovesCredentialsAndQuery(t *testing.T) {
	got := safeLogURL("https://name:password@mp.example/api?token=secret#fragment")
	if got != "https://mp.example/api" {
		t.Fatalf("safeLogURL() = %q", got)
	}
}

func TestSafeLogErrorRedactsPassword(t *testing.T) {
	got := safeLogError(fmt.Errorf("request failed for password=%s", "top-secret"), "top-secret")
	if strings.Contains(got, "top-secret") || !strings.Contains(got, "***") {
		t.Fatalf("safeLogError() = %q", got)
	}
}

func TestConfigUsesFixedMigrationScope(t *testing.T) {
	cfg := parseConfig(map[string]any{
		"base_url": "https://mp.example", "username": "admin",
		"include_sites": false, "include_subscriptions": false, "include_transfer_history": false,
	})
	selected := selectedSet(cfg.Sources)
	if len(selected) != 5 || !selected["rules"] || !selected["transfer_history"] || !selected["subscriptions"] || !selected["subscribe_history"] || !selected["sites"] {
		t.Fatalf("recommended sources missing: %v", cfg.Sources)
	}
	if selected["download_history"] || selected["download_files"] {
		t.Fatalf("download history must not be part of the importer: %v", cfg.Sources)
	}
}

func TestValidateConfigRequiresMoviePilotUsername(t *testing.T) {
	err := validateConfig(map[string]any{"base_url": "https://mp.example"})
	validationErr, ok := err.(*pluginsdk.ValidationError)
	if !ok || validationErr.Fields["username"] == "" {
		t.Fatalf("expected username validation error, got %v", err)
	}
}

func TestApplyUsesTypedHostCapabilities(t *testing.T) {
	recorder := &capabilityRecorder{}
	handler := actionHandler{instance: pluginsdk.Instance{
		SiteAccounts: recorder, Subscriptions: recorder, Downloads: recorder, Transfers: recorder, Rules: recorder,
	}}
	ctx := context.Background()

	subscriptionRaw := `{"name":"Example Series","type":"电视剧","year":"2026","tmdbid":123,"season":1,"total_episode":3,"lack_episode":"1-3","state":"R"}`
	result, err := handler.apply(ctx, stagedItem{SourceType: "subscriptions", SourceID: "sub-1", DataJSON: subscriptionRaw})
	if err != nil || result.TargetID != "subscription-1" {
		t.Fatalf("subscription apply = %+v, %v", result, err)
	}
	if recorder.subscription.Media.Title != "Example Series" || len(recorder.subscription.WantedEpisodes) != 3 {
		t.Fatalf("subscription mapping = %+v", recorder.subscription)
	}

	ruleRaw := `{"id":"rule-one","kind":"profile","profile_key":"moviepilot:rule-profile:one","name":"高清","priority":1,"groups":[{"name":"高清","rule_string":"4K > 1080P"}]}`
	result, err = handler.apply(ctx, stagedItem{SourceType: "rules", SourceID: "rule-one", DataJSON: ruleRaw})
	if err != nil || result.TargetID != "rule-1" {
		t.Fatalf("rule apply = %+v, %v", result, err)
	}
	if recorder.ruleProfile.IdempotencyKey != "moviepilot:rule-profile:one" || len(recorder.ruleProfile.Preferences) == 0 {
		t.Fatalf("rule profile mapping = %+v", recorder.ruleProfile)
	}
	defaultRaw := `{"id":"20-default-subscription","kind":"default","scope":"subscription","profile_key":"moviepilot:rule-profile:one"}`
	result, err = handler.apply(ctx, stagedItem{SourceType: "rules", SourceID: "20-default-subscription", DataJSON: defaultRaw})
	if err != nil || result.TargetID != "rules:default:subscription" || recorder.ruleDefault.RuleProfileKey != "moviepilot:rule-profile:one" {
		t.Fatalf("rule default apply = %+v, recorder=%+v, %v", result, recorder.ruleDefault, err)
	}

	transferRaw := `{"title":"Example Series","type":"电视剧","tmdbid":123,"download_hash":"abc","src":"/downloads/Example.S01E02.mkv","dest":"/media/Example.S01E02.mkv","mode":"link","status":true,"date":"2026-07-01 12:00:00","dest_fileitem":{"size":100}}`
	result, err = handler.apply(ctx, stagedItem{SourceType: "transfer_history", SourceID: "transfer-1", DataJSON: transferRaw})
	if err != nil || result.TargetID != "transfer-1" {
		t.Fatalf("transfer apply = %+v, %v", result, err)
	}
	if recorder.transfer.Operation != "hardlink" || recorder.transfer.SeasonNumber != 1 || recorder.transfer.EpisodeNumber != 2 {
		t.Fatalf("transfer mapping = %+v", recorder.transfer)
	}
}

type capabilityRecorder struct {
	site         pluginsdk.SiteAccountWrite
	subscription pluginsdk.SubscriptionWrite
	download     pluginsdk.DownloadWrite
	transfer     pluginsdk.TransferWrite
	ruleProfile  pluginsdk.RuleProfileWrite
	ruleSort     pluginsdk.RuleSortWrite
	ruleDefault  pluginsdk.RuleDefaultWrite
}

type staticSecretResolver string

func (r staticSecretResolver) Reveal(context.Context, string, string) (string, error) {
	return string(r), nil
}

type recordingLogger struct {
	entries []string
}

func (l *recordingLogger) append(level runtimesdk.LogLevel, message string, attrs ...any) {
	l.entries = append(l.entries, fmt.Sprintf("%s %s %v", level, message, attrs))
}

func (l *recordingLogger) Log(_ context.Context, level runtimesdk.LogLevel, message string, attrs ...any) {
	l.append(level, message, attrs...)
}

func (l *recordingLogger) Debug(_ context.Context, message string, attrs ...any) {
	l.append(runtimesdk.LogDebug, message, attrs...)
}

func (l *recordingLogger) Info(_ context.Context, message string, attrs ...any) {
	l.append(runtimesdk.LogInfo, message, attrs...)
}

func (l *recordingLogger) Warn(_ context.Context, message string, attrs ...any) {
	l.append(runtimesdk.LogWarn, message, attrs...)
}

func (l *recordingLogger) Error(_ context.Context, message string, attrs ...any) {
	l.append(runtimesdk.LogError, message, attrs...)
}

func (l *recordingLogger) Toast(context.Context, runtimesdk.ToastInput) error         { return nil }
func (l *recordingLogger) Notify(context.Context, runtimesdk.NotificationInput) error { return nil }

func (l *recordingLogger) String() string {
	return strings.Join(l.entries, "\n")
}

func (r *capabilityRecorder) UpsertSiteAccount(ctx context.Context, input pluginsdk.SiteAccountWrite) (pluginsdk.HostWriteResult, error) {
	r.site = input
	return pluginsdk.HostWriteResult{TargetID: "site-1", Change: "created"}, nil
}

func (r *capabilityRecorder) UpsertSubscription(ctx context.Context, input pluginsdk.SubscriptionWrite) (pluginsdk.HostWriteResult, error) {
	r.subscription = input
	return pluginsdk.HostWriteResult{TargetID: "subscription-1", Change: "created"}, nil
}

func (r *capabilityRecorder) UpsertDownload(ctx context.Context, input pluginsdk.DownloadWrite) (pluginsdk.HostWriteResult, error) {
	r.download = input
	return pluginsdk.HostWriteResult{TargetID: "download-1", Change: "created"}, nil
}

func (r *capabilityRecorder) FindDownloadByHash(ctx context.Context, hash string) (pluginsdk.HostWriteResult, bool, error) {
	return pluginsdk.HostWriteResult{TargetID: "download-1"}, true, nil
}

func (r *capabilityRecorder) UpsertTransfer(ctx context.Context, input pluginsdk.TransferWrite) (pluginsdk.HostWriteResult, error) {
	r.transfer = input
	return pluginsdk.HostWriteResult{TargetID: "transfer-1", Change: "created"}, nil
}

func (r *capabilityRecorder) GetRuleCatalog(context.Context) (pluginsdk.RuleCatalog, error) {
	return testRuleCatalog(), nil
}

func (r *capabilityRecorder) UpsertRuleProfile(ctx context.Context, input pluginsdk.RuleProfileWrite) (pluginsdk.HostWriteResult, error) {
	r.ruleProfile = input
	return pluginsdk.HostWriteResult{TargetID: "rule-1", Change: "created"}, nil
}

func (r *capabilityRecorder) SetRuleSort(ctx context.Context, input pluginsdk.RuleSortWrite) (pluginsdk.RuleSortResult, error) {
	r.ruleSort = input
	return pluginsdk.RuleSortResult{Selected: input.Selected}, nil
}

func (r *capabilityRecorder) SetRuleDefault(ctx context.Context, input pluginsdk.RuleDefaultWrite) (pluginsdk.RuleDefaultResult, error) {
	r.ruleDefault = input
	return pluginsdk.RuleDefaultResult{Scope: input.Scope, RuleProfileID: "rule-1"}, nil
}
