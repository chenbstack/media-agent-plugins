package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildRuleBundlesCoversDefaultsAndSubscriptionSelections(t *testing.T) {
	data := &moviePilotRuleData{
		groups: []moviePilotRuleGroup{
			{Name: "高清", RuleString: "4K > 1080P"},
			{Name: "中文字幕", RuleString: "CNSUB"},
			{Name: "高清", RuleString: "720P"}, // duplicate: MoviePilot uses the first.
		},
		searchDefaults:    []string{"高清"},
		subscribeDefaults: []string{"高清", "中文字幕"},
		upgradeDefaults:   []string{"中文字幕", "高清"},
		subscriptions: []json.RawMessage{
			json.RawMessage(`{"id":1,"filter_groups":["高清","中文字幕"]}`),
			json.RawMessage(`{"id":2,"filter_groups":[],"best_version":1}`),
			json.RawMessage(`{"id":3,"filter_groups":["不存在","高清"]}`),
		},
	}
	bundles := buildRuleBundles(data)
	if len(bundles) != 4 {
		t.Fatalf("bundles = %d, want 4: %+v", len(bundles), bundles)
	}
	byKey := map[string]moviePilotRuleBundle{}
	for _, bundle := range bundles {
		byKey[bundle.ProfileKey] = bundle
	}
	if single := byKey[ruleProfileKey([]string{"高清"})]; len(single.Groups) != 1 || single.Groups[0].RuleString != "4K > 1080P" {
		t.Fatalf("duplicate-name first group not preserved: %+v", single)
	}
	if combined := byKey[ruleProfileKey([]string{"高清", "中文字幕"})]; combined.Name != "MoviePilot · 高清 + 中文字幕" || len(combined.Groups) != 2 {
		t.Fatalf("combined profile = %+v", combined)
	}
	if _, exists := byKey[ruleProfileKey([]string{"不存在", "高清"})]; exists {
		t.Fatal("missing rule names must not create an unbindable profile key")
	}
}

func TestRuleProfileKeyIsStableAndOrderSensitive(t *testing.T) {
	first := ruleProfileKey([]string{" A ", "B", "A", ""})
	again := ruleProfileKey([]string{"A", "B"})
	reversed := ruleProfileKey([]string{"B", "A"})
	if first == "" || first != again || first == reversed || !strings.HasPrefix(first, moviePilotRuleProfileKeyPrefix) {
		t.Fatalf("keys = %q %q %q", first, again, reversed)
	}
}

func TestExportRulesOrdersProfilesBeforeDefaults(t *testing.T) {
	data := &moviePilotRuleData{
		groups:            []moviePilotRuleGroup{{Name: "高清", RuleString: "4K"}},
		searchDefaults:    []string{"高清"},
		subscribeDefaults: []string{"高清"},
		torrentSort:       []string{"torrent"},
	}
	data.bundles = buildRuleBundles(data)
	client := &moviePilotClient{rules: data}
	page, err := client.exportRules(t.Context(), "", 100)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(page.Items))
	for _, item := range page.Items {
		ids = append(ids, item.SourceID)
	}
	if len(ids) != 4 || !strings.HasPrefix(ids[0], "10-profile-") || ids[1] != "20-default-search" || ids[2] != "20-default-subscription" || ids[3] != "30-global-sort" {
		t.Fatalf("rule item order = %v", ids)
	}
}

func TestSubscriptionRuleSelectionUsesExplicitThenUpgradeThenDefault(t *testing.T) {
	data := &moviePilotRuleData{
		groups: []moviePilotRuleGroup{
			{Name: "普通", RuleString: "1080P"},
			{Name: "洗版", RuleString: "4K"},
			{Name: "自选", RuleString: "CNSUB"},
		},
		subscribeDefaults: []string{"普通"},
		upgradeDefaults:   []string{"洗版"},
		subscriptions: []json.RawMessage{
			json.RawMessage(`{"id":1,"filter_groups":["自选"],"best_version":1}`),
			json.RawMessage(`{"id":2,"filter_groups":[],"best_version":1}`),
			json.RawMessage(`{"id":3,"filter_groups":[],"best_version":0}`),
			json.RawMessage(`{"id":4,"filter_groups":["已删除规则"]}`),
		},
	}
	client := &moviePilotClient{rules: data}
	page, err := client.exportSubscriptions(t.Context(), "", 100)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		ruleProfileKey([]string{"自选"}),
		ruleProfileKey([]string{"洗版"}),
		ruleProfileKey([]string{"普通"}),
		"",
	}
	for index, item := range page.Items {
		var value map[string]any
		if err := json.Unmarshal(item.Data, &value); err != nil {
			t.Fatal(err)
		}
		if got := stringValue(value, ruleSelectionMetadataKey); got != want[index] {
			t.Fatalf("subscription %d key = %q, want %q", index, got, want[index])
		}
	}
}

func TestStringSliceValueHandlesLegacyShapes(t *testing.T) {
	tests := []struct {
		value any
		want  string
	}{
		{value: []any{" A ", 2, "A"}, want: "A,2"},
		{value: []string{"A", "B"}, want: "A,B"},
		{value: " A, B,A ", want: "A,B"},
		{value: nil, want: ""},
	}
	for _, test := range tests {
		if got := strings.Join(stringSliceValue(test.value), ","); got != test.want {
			t.Fatalf("stringSliceValue(%#v) = %q, want %q", test.value, got, test.want)
		}
	}
}
