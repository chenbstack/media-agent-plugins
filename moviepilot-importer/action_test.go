package main

import (
	"context"
	"testing"

	"github.com/chenbstack/media-agent-plugin-sdk-go"
)

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
