package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

const (
	moviePilotRuleProfileKeyPrefix = "moviepilot:rule-profile:"
	ruleSelectionMetadataKey       = "_moviepilot_rule_profile_key"
)

type moviePilotCustomRule struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Include     any    `json:"include"`
	Exclude     any    `json:"exclude"`
	SizeRange   any    `json:"size_range"`
	Seeders     any    `json:"seeders"`
	PublishTime any    `json:"publish_time"`
}

type moviePilotRuleGroup struct {
	Name       string `json:"name"`
	RuleString string `json:"rule_string"`
	MediaType  string `json:"media_type"`
	Category   string `json:"category"`
}

type moviePilotRuleBundle struct {
	ID          string                 `json:"id"`
	Kind        string                 `json:"kind"`
	ProfileKey  string                 `json:"profile_key,omitempty"`
	Name        string                 `json:"name,omitempty"`
	Priority    int                    `json:"priority,omitempty"`
	Scope       string                 `json:"scope,omitempty"`
	Groups      []moviePilotRuleGroup  `json:"groups,omitempty"`
	CustomRules []moviePilotCustomRule `json:"custom_rules,omitempty"`
	Selected    []string               `json:"selected,omitempty"`
}

type moviePilotRuleData struct {
	groups            []moviePilotRuleGroup
	customRules       []moviePilotCustomRule
	searchDefaults    []string
	subscribeDefaults []string
	upgradeDefaults   []string
	torrentSort       []string
	subscriptions     []json.RawMessage
	bundles           []moviePilotRuleBundle
}

func (c *moviePilotClient) exportRules(ctx context.Context, cursor string, limit int) (exportPage, error) {
	data, err := c.loadRuleData(ctx)
	if err != nil {
		return exportPage{}, err
	}
	rows := make([]json.RawMessage, 0, len(data.bundles)+3)
	for _, bundle := range data.bundles {
		raw, err := json.Marshal(bundle)
		if err != nil {
			return exportPage{}, err
		}
		rows = append(rows, raw)
	}
	for _, item := range []struct {
		scope string
		names []string
	}{
		{scope: "search", names: data.searchDefaults},
		{scope: "subscription", names: data.subscribeDefaults},
	} {
		names := resolveRuleGroupNames(data.groups, item.names)
		if key := ruleProfileKey(names); key != "" {
			bundle := moviePilotRuleBundle{ID: "20-default-" + item.scope, Kind: "default", Scope: item.scope, ProfileKey: key}
			raw, err := json.Marshal(bundle)
			if err != nil {
				return exportPage{}, err
			}
			rows = append(rows, raw)
		}
	}
	sortBundle := moviePilotRuleBundle{ID: "30-global-sort", Kind: "sort", Selected: data.torrentSort}
	raw, err := json.Marshal(sortBundle)
	if err != nil {
		return exportPage{}, err
	}
	rows = append(rows, raw)
	return exportRawPage("rules", rows, cursor, limit)
}

func (c *moviePilotClient) exportSubscriptions(ctx context.Context, cursor string, limit int) (exportPage, error) {
	data, err := c.loadRuleData(ctx)
	if err != nil {
		return exportPage{}, err
	}
	rows := make([]json.RawMessage, 0, len(data.subscriptions))
	for _, raw := range data.subscriptions {
		var subscription map[string]any
		if err := json.Unmarshal(raw, &subscription); err != nil {
			return exportPage{}, fmt.Errorf("解析 MoviePilot subscriptions 数据失败: %w", err)
		}
		names := stringSliceValue(subscription["filter_groups"])
		if len(names) == 0 {
			if boolValue(subscription, "best_version", false) {
				names = data.upgradeDefaults
			} else {
				names = data.subscribeDefaults
			}
		}
		names = resolveRuleGroupNames(data.groups, names)
		if key := ruleProfileKey(names); key != "" {
			subscription[ruleSelectionMetadataKey] = key
		}
		normalized, err := json.Marshal(subscription)
		if err != nil {
			return exportPage{}, err
		}
		rows = append(rows, normalized)
	}
	return exportRawPage("subscriptions", rows, cursor, limit)
}

func (c *moviePilotClient) loadRuleData(ctx context.Context) (*moviePilotRuleData, error) {
	if c.rules != nil {
		return c.rules, nil
	}
	data := &moviePilotRuleData{}
	if err := c.getSetting(ctx, "UserFilterRuleGroups", &data.groups); err != nil {
		return nil, err
	}
	if err := c.getSetting(ctx, "CustomFilterRules", &data.customRules); err != nil {
		return nil, err
	}
	if err := c.getSetting(ctx, "SearchFilterRuleGroups", &data.searchDefaults); err != nil {
		return nil, err
	}
	if err := c.getSetting(ctx, "SubscribeFilterRuleGroups", &data.subscribeDefaults); err != nil {
		return nil, err
	}
	if err := c.getSetting(ctx, "BestVersionFilterRuleGroups", &data.upgradeDefaults); err != nil {
		return nil, err
	}
	if err := c.getSetting(ctx, "TorrentsPriority", &data.torrentSort); err != nil {
		return nil, err
	}
	if len(data.torrentSort) == 0 {
		data.torrentSort = []string{"torrent", "upload", "seeder"}
	}
	if err := c.get(ctx, "/subscribe/", nil, &data.subscriptions); err != nil {
		return nil, err
	}
	data.bundles = buildRuleBundles(data)
	c.rules = data
	return data, nil
}

func (c *moviePilotClient) getSetting(ctx context.Context, key string, output any) error {
	var response struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
		Data    struct {
			Value json.RawMessage `json:"value"`
		} `json:"data"`
	}
	if err := c.get(ctx, "/system/setting/"+url.PathEscape(key), nil, &response); err != nil {
		return err
	}
	if !response.Success {
		return fmt.Errorf("读取 MoviePilot 设置 %s 失败: %s", key, response.Message)
	}
	if len(response.Data.Value) == 0 || string(response.Data.Value) == "null" {
		return nil
	}
	if err := json.Unmarshal(response.Data.Value, output); err != nil {
		return fmt.Errorf("解析 MoviePilot 设置 %s 失败: %w", key, err)
	}
	return nil
}

func buildRuleBundles(data *moviePilotRuleData) []moviePilotRuleBundle {
	groupsByName := map[string]moviePilotRuleGroup{}
	groupPriority := map[string]int{}
	orderedNames := make([]string, 0, len(data.groups))
	for _, group := range data.groups {
		name := strings.TrimSpace(group.Name)
		if name == "" {
			continue
		}
		if _, exists := groupsByName[name]; exists {
			continue // MoviePilot resolves duplicate names to the first rule group.
		}
		group.Name = name
		groupsByName[name] = group
		groupPriority[name] = len(orderedNames) + 1
		orderedNames = append(orderedNames, name)
	}
	selections := make([][]string, 0, len(orderedNames)+3+len(data.subscriptions))
	for _, name := range orderedNames {
		selections = append(selections, []string{name})
	}
	selections = append(selections, data.searchDefaults, data.subscribeDefaults, data.upgradeDefaults)
	for _, raw := range data.subscriptions {
		var subscription map[string]any
		if json.Unmarshal(raw, &subscription) != nil {
			continue
		}
		names := stringSliceValue(subscription["filter_groups"])
		if len(names) == 0 {
			if boolValue(subscription, "best_version", false) {
				names = data.upgradeDefaults
			} else {
				names = data.subscribeDefaults
			}
		}
		selections = append(selections, names)
	}

	seen := map[string]bool{}
	bundles := make([]moviePilotRuleBundle, 0, len(selections))
	for _, selection := range selections {
		names := resolveRuleGroupNames(data.groups, selection)
		key := ruleProfileKey(names)
		if key == "" || seen[key] {
			continue
		}
		groups := make([]moviePilotRuleGroup, 0, len(names))
		resolvedNames := make([]string, 0, len(names))
		for _, name := range names {
			group, ok := groupsByName[name]
			if !ok {
				continue
			}
			groups = append(groups, group)
			resolvedNames = append(resolvedNames, name)
		}
		if len(groups) == 0 {
			continue
		}
		seen[key] = true
		name := groups[0].Name
		priority := groupPriority[resolvedNames[0]]
		if len(groups) > 1 {
			name = "MoviePilot · " + strings.Join(resolvedNames, " + ")
		}
		id := "10-profile-" + strings.TrimPrefix(key, moviePilotRuleProfileKeyPrefix)
		bundles = append(bundles, moviePilotRuleBundle{
			ID: id, Kind: "profile", ProfileKey: key, Name: name, Priority: priority,
			Groups: groups, CustomRules: append([]moviePilotCustomRule(nil), data.customRules...),
		})
	}
	sort.SliceStable(bundles, func(i, j int) bool {
		if bundles[i].Priority == bundles[j].Priority {
			return bundles[i].ID < bundles[j].ID
		}
		return bundles[i].Priority < bundles[j].Priority
	})
	return bundles
}

func resolveRuleGroupNames(groups []moviePilotRuleGroup, input []string) []string {
	available := map[string]bool{}
	for _, group := range groups {
		if name := strings.TrimSpace(group.Name); name != "" {
			available[name] = true
		}
	}
	out := make([]string, 0, len(input))
	for _, name := range cleanRuleGroupNames(input) {
		if available[name] {
			out = append(out, name)
		}
	}
	return out
}

func ruleProfileKey(names []string) string {
	names = cleanRuleGroupNames(names)
	if len(names) == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.Join(names, "\x1f")))
	return moviePilotRuleProfileKeyPrefix + hex.EncodeToString(sum[:12])
}

func cleanRuleGroupNames(input []string) []string {
	out := make([]string, 0, len(input))
	seen := map[string]bool{}
	for _, name := range input {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

func stringSliceValue(value any) []string {
	var values []string
	switch typed := value.(type) {
	case []string:
		values = append(values, typed...)
	case []any:
		for _, item := range typed {
			values = append(values, strings.TrimSpace(fmt.Sprint(item)))
		}
	case string:
		if strings.TrimSpace(typed) != "" {
			values = strings.Split(typed, ",")
		}
	}
	return cleanRuleGroupNames(values)
}

func exportRawPage(sourceType string, rows []json.RawMessage, cursor string, limit int) (exportPage, error) {
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
