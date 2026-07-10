package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/chenbstack/media-agent-plugin-sdk-go"
	runtimesdk "github.com/chenbstack/media-agent-plugin-sdk-go/runtime"
)

type actionHandler struct {
	instance pluginsdk.Instance
	secrets  pluginsdk.SecretResolver
}

func newActionHandler(ctx context.Context, instance pluginsdk.Instance, secrets pluginsdk.SecretResolver) (pluginsdk.ActionHandler, error) {
	if instance.Runtime == nil || instance.Runtime.Feedback == nil {
		return nil, fmt.Errorf("宿主未提供插件 Runtime Feedback")
	}
	return &actionHandler{instance: instance, secrets: secrets}, nil
}

func (h *actionHandler) RunAction(ctx context.Context, actionID string, input map[string]any) (pluginsdk.ActionResult, error) {
	switch actionID {
	case "test":
		return h.testConnection(ctx)
	case "sync":
		return h.sync(ctx)
	case "status":
		store, err := newStateStore(ctx, h.instance.DB)
		if err != nil {
			return pluginsdk.ActionResult{}, err
		}
		status, err := store.status(ctx)
		if err != nil {
			return pluginsdk.ActionResult{}, err
		}
		return pluginsdk.ActionResult{Message: firstNonEmpty(stringValue(status, "message"), "已读取同步状态"), Data: status}, nil
	default:
		return pluginsdk.ActionResult{}, fmt.Errorf("未知插件动作 %q", actionID)
	}
}

func (h *actionHandler) logger() runtimesdk.Feedback {
	return h.instance.Runtime.Feedback
}

func (h *actionHandler) testConnection(ctx context.Context) (pluginsdk.ActionResult, error) {
	logger := h.logger()
	logger.Info(ctx, "开始测试 MoviePilot 连接")
	cfg, err := h.config(ctx)
	if err != nil {
		logger.Error(ctx, "读取 MoviePilot 连接配置失败", "error", err.Error())
		return pluginsdk.ActionResult{}, err
	}
	baseURL := safeLogURL(cfg.BaseURL)
	logger.Debug(ctx, "MoviePilot 连接配置已读取", "base_url", baseURL)
	ping, err := newMoviePilotClient(cfg).ping(ctx)
	if err != nil {
		logger.Error(ctx, "MoviePilot 连接测试失败", "base_url", baseURL, "error", safeLogError(err, cfg.Password))
		return pluginsdk.ActionResult{}, err
	}
	logger.Info(ctx, "MoviePilot 连接测试成功", "base_url", baseURL, "version", stringValue(ping.MoviePilot, "version"))
	return pluginsdk.ActionResult{Message: "MoviePilot 登录连接正常", Data: map[string]any{
		"moviepilot": ping.MoviePilot, "export_types": ping.ExportTypes,
	}}, nil
}

func safeLogURL(value string) string {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "<invalid>"
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return strings.TrimRight(parsed.String(), "/")
}

func safeLogError(err error, sensitiveValues ...string) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	for _, value := range sensitiveValues {
		value = strings.TrimSpace(value)
		if value != "" {
			message = strings.ReplaceAll(message, value, "***")
		}
	}
	return message
}

func (h *actionHandler) config(ctx context.Context) (config, error) {
	if err := validateConfig(h.instance.Config); err != nil {
		return config{}, err
	}
	if h.secrets == nil {
		return config{}, fmt.Errorf("宿主未提供密钥读取能力")
	}
	ref := stringValue(h.instance.Config, "password")
	password, err := h.secrets.Reveal(ctx, ref, "登录 MoviePilot 读取迁移数据")
	if err != nil {
		return config{}, err
	}
	return configWithSecret(h.instance.Config, password)
}

func (h *actionHandler) sync(ctx context.Context) (output pluginsdk.ActionResult, syncErr error) {
	logger := h.logger()
	logger.Info(ctx, "开始从 MoviePilot 同步并导入数据")
	runID := ""
	sensitiveValues := []string{}
	defer func() {
		if syncErr != nil {
			attrs := []any{"error", safeLogError(syncErr, sensitiveValues...)}
			if runID != "" {
				attrs = append(attrs, "run_id", runID)
			}
			logger.Error(context.WithoutCancel(ctx), "MoviePilot 同步并导入失败", attrs...)
		}
	}()
	cfg, err := h.config(ctx)
	if err != nil {
		return pluginsdk.ActionResult{}, err
	}
	sensitiveValues = append(sensitiveValues, cfg.Password)
	baseURL := safeLogURL(cfg.BaseURL)
	logger.Debug(ctx, "MoviePilot 同步配置已读取", "base_url", baseURL, "sources", strings.Join(cfg.Sources, ","))
	store, err := newStateStore(ctx, h.instance.DB)
	if err != nil {
		return pluginsdk.ActionResult{}, err
	}
	runID, err = store.startRun(ctx, cfg.Sources)
	if err != nil {
		return pluginsdk.ActionResult{}, err
	}
	defer func() {
		if syncErr != nil {
			_ = store.finishRun(context.WithoutCancel(ctx), runID, "failed", syncErr.Error())
		}
	}()
	client := newMoviePilotClient(cfg)
	if _, err := client.ping(ctx); err != nil {
		return pluginsdk.ActionResult{}, err
	}
	logger.Info(ctx, "MoviePilot 同步前连接检查通过", "base_url", baseURL, "run_id", runID)
	selected := map[string]bool{}
	staged := map[string]map[string]int{}
	for _, sourceType := range cfg.Sources {
		logger.Info(ctx, "开始读取 MoviePilot 数据", "run_id", runID, "source_type", sourceType, "source_name", migrationTaskNames[sourceType])
		selected[sourceType] = true
		staged[sourceType] = map[string]int{"created": 0, "updated": 0, "unchanged": 0}
		if err := store.updateTask(ctx, runID, sourceType, "running", 0, 0, "正在从 MoviePilot 读取"); err != nil {
			return pluginsdk.ActionResult{}, err
		}
		cursor := ""
		fetched := 0
		total := 0
		for {
			page, err := client.export(ctx, sourceType, cursor, cfg.PageLimit)
			if err != nil {
				_ = store.updateTask(context.WithoutCancel(ctx), runID, sourceType, "failed", fetched, total, err.Error())
				return pluginsdk.ActionResult{}, err
			}
			if page.Total > 0 {
				total = page.Total
			}
			for _, item := range page.Items {
				change, err := store.stage(ctx, sourceType, item)
				if err != nil {
					_ = store.updateTask(context.WithoutCancel(ctx), runID, sourceType, "failed", fetched, total, err.Error())
					return pluginsdk.ActionResult{}, err
				}
				staged[sourceType][change]++
				fetched++
			}
			if err := store.updateTask(ctx, runID, sourceType, "running", fetched, total, fmt.Sprintf("已读取 %d 项", fetched)); err != nil {
				return pluginsdk.ActionResult{}, err
			}
			if page.Done || page.NextCursor == "" {
				break
			}
			cursor = page.NextCursor
		}
		if err := store.updateTask(ctx, runID, sourceType, "pending", fetched, total, fmt.Sprintf("已读取 %d 项，等待导入", fetched)); err != nil {
			return pluginsdk.ActionResult{}, err
		}
		logger.Info(ctx, "MoviePilot 数据读取完成", "run_id", runID, "source_type", sourceType, "source_name", migrationTaskNames[sourceType], "count", fetched)
	}
	items, err := store.list(ctx, selected)
	if err != nil {
		return pluginsdk.ActionResult{}, err
	}
	sort.SliceStable(items, func(i, j int) bool {
		left, right := sourcePriority(items[i].SourceType), sourcePriority(items[j].SourceType)
		if left == right {
			return items[i].SourceID < items[j].SourceID
		}
		return left < right
	})
	applied := map[string]map[string]int{}
	taskTotals := map[string]int{}
	taskCurrent := map[string]int{}
	for _, sourceType := range cfg.Sources {
		applied[sourceType] = map[string]int{"created": 0, "updated": 0, "unchanged": 0, "failed": 0}
	}
	for _, item := range items {
		taskTotals[item.SourceType]++
	}
	for _, sourceType := range cfg.Sources {
		message := "等待导入"
		if taskTotals[sourceType] == 0 {
			message = "没有可导入数据"
		}
		if err := store.updateTask(ctx, runID, sourceType, "pending", 0, taskTotals[sourceType], message); err != nil {
			return pluginsdk.ActionResult{}, err
		}
	}
	failedMessages := []string{}
	for _, item := range items {
		if taskCurrent[item.SourceType] == 0 {
			if err := store.updateTask(ctx, runID, item.SourceType, "running", 0, taskTotals[item.SourceType], "正在导入"); err != nil {
				return pluginsdk.ActionResult{}, err
			}
		}
		// 规则集需要每次同步都重新写入：宿主侧可能被用户删除，增量缓存不能代表目标仍存在。
		if item.SourceType != "rules" && item.AppliedStatus == "success" && item.AppliedHash == item.Hash {
			applied[item.SourceType]["unchanged"]++
		} else {
			result, applyErr := h.apply(ctx, item)
			if applyErr != nil {
				applied[item.SourceType]["failed"]++
				failedMessages = append(failedMessages, item.SourceType+"/"+item.SourceID+": "+applyErr.Error())
				if err := store.mark(ctx, item, "failed", item.TargetID, applyErr); err != nil {
					return pluginsdk.ActionResult{}, err
				}
			} else {
				change := result.Change
				if change == "" {
					change = "unchanged"
				}
				applied[item.SourceType][change]++
				if err := store.mark(ctx, item, "success", result.TargetID, nil); err != nil {
					return pluginsdk.ActionResult{}, err
				}
			}
		}
		taskCurrent[item.SourceType]++
		message := fmt.Sprintf("正在导入 %d/%d", taskCurrent[item.SourceType], taskTotals[item.SourceType])
		if err := store.updateTask(ctx, runID, item.SourceType, "running", taskCurrent[item.SourceType], taskTotals[item.SourceType], message); err != nil {
			return pluginsdk.ActionResult{}, err
		}
	}
	message := "MoviePilot 数据同步并导入完成"
	runStatus := "completed"
	if len(failedMessages) > 0 {
		message = fmt.Sprintf("同步完成，%d 个对象导入失败", len(failedMessages))
		runStatus = "partial"
	}
	for _, sourceType := range cfg.Sources {
		taskStatus := "completed"
		taskMessage := fmt.Sprintf("已处理 %d 项", taskTotals[sourceType])
		if applied[sourceType]["failed"] > 0 {
			taskStatus = "partial"
			taskMessage = fmt.Sprintf("已处理 %d 项，%d 项失败", taskTotals[sourceType], applied[sourceType]["failed"])
		}
		if err := store.updateTask(ctx, runID, sourceType, taskStatus, taskTotals[sourceType], taskTotals[sourceType], taskMessage); err != nil {
			return pluginsdk.ActionResult{}, err
		}
		logger.Info(ctx, "MoviePilot 数据导入任务完成",
			"run_id", runID, "source_type", sourceType, "source_name", migrationTaskNames[sourceType],
			"status", taskStatus, "total", taskTotals[sourceType],
			"created", applied[sourceType]["created"], "updated", applied[sourceType]["updated"],
			"unchanged", applied[sourceType]["unchanged"], "failed", applied[sourceType]["failed"])
	}
	if err := store.finishRun(ctx, runID, runStatus, message); err != nil {
		return pluginsdk.ActionResult{}, err
	}
	data := map[string]any{"selected_sources": cfg.Sources, "staged": staged, "applied": applied}
	if len(failedMessages) > 0 {
		data["errors"] = failedMessages
		logger.Warn(ctx, "MoviePilot 数据同步完成，部分对象导入失败", "run_id", runID, "failed", len(failedMessages))
	} else {
		logger.Info(ctx, "MoviePilot 数据同步并导入完成", "run_id", runID)
	}
	return pluginsdk.ActionResult{Message: message, Data: data}, nil
}

func (h *actionHandler) apply(ctx context.Context, item stagedItem) (pluginsdk.HostWriteResult, error) {
	data := map[string]any{}
	if err := json.Unmarshal([]byte(item.DataJSON), &data); err != nil {
		return pluginsdk.HostWriteResult{}, fmt.Errorf("解析源对象: %w", err)
	}
	metadata := json.RawMessage(item.DataJSON)
	switch item.SourceType {
	case "rules":
		if h.instance.Rules == nil {
			return pluginsdk.HostWriteResult{}, fmt.Errorf("宿主未提供规则能力")
		}
		var bundle moviePilotRuleBundle
		if err := json.Unmarshal([]byte(item.DataJSON), &bundle); err != nil {
			return pluginsdk.HostWriteResult{}, fmt.Errorf("解析 MoviePilot 规则数据: %w", err)
		}
		catalog, err := h.instance.Rules.GetRuleCatalog(ctx)
		if err != nil {
			return pluginsdk.HostWriteResult{}, err
		}
		switch bundle.Kind {
		case "profile":
			conversion, err := convertMoviePilotRuleBundle(bundle, catalog)
			if err != nil {
				return pluginsdk.HostWriteResult{}, err
			}
			conversion.Write.TargetID = item.TargetID
			return h.instance.Rules.UpsertRuleProfile(ctx, conversion.Write)
		case "sort":
			selected, _ := mapMoviePilotSort(bundle.Selected, catalog)
			if _, err := h.instance.Rules.SetRuleSort(ctx, pluginsdk.RuleSortWrite{Selected: selected}); err != nil {
				return pluginsdk.HostWriteResult{}, err
			}
			return pluginsdk.HostWriteResult{TargetID: "rules:global-sort", Change: "updated"}, nil
		case "default":
			result, err := h.instance.Rules.SetRuleDefault(ctx, pluginsdk.RuleDefaultWrite{
				Scope: bundle.Scope, RuleProfileKey: bundle.ProfileKey,
			})
			if err != nil {
				return pluginsdk.HostWriteResult{}, err
			}
			return pluginsdk.HostWriteResult{TargetID: "rules:default:" + result.Scope, Change: "updated"}, nil
		default:
			return pluginsdk.HostWriteResult{}, fmt.Errorf("未知 MoviePilot 规则数据类型 %q", bundle.Kind)
		}
	case "sites":
		if h.instance.SiteAccounts == nil {
			return pluginsdk.HostWriteResult{}, fmt.Errorf("宿主未提供站点账号能力")
		}
		baseURL := firstNonEmpty(stringValue(data, "url"), stringValue(data, "domain"))
		return h.instance.SiteAccounts.UpsertSiteAccount(ctx, pluginsdk.SiteAccountWrite{
			TargetID: item.TargetID, IdempotencyKey: "moviepilot:site:" + item.SourceID,
			Name:    firstNonEmpty(stringValue(data, "name"), stringValue(data, "domain"), "MoviePilot 站点 "+item.SourceID),
			BaseURL: baseURL, Enabled: boolValue(data, "is_active", true), UserAgent: stringValue(data, "ua"),
			Cookie: nonRedactedString(data["cookie"]), Metadata: metadata,
		})
	case "subscriptions":
		if h.instance.Subscriptions == nil {
			return pluginsdk.HostWriteResult{}, fmt.Errorf("宿主未提供订阅能力")
		}
		season := intValue(data, "season")
		if season <= 0 {
			season = 1
		}
		return h.instance.Subscriptions.UpsertSubscription(ctx, pluginsdk.SubscriptionWrite{
			TargetID: item.TargetID, IdempotencyKey: "moviepilot:subscription:" + item.SourceID,
			Media: mediaIdentity(data), Season: season,
			TotalEpisodes:  firstPositive(intValue(data, "total_episode"), intValue(data, "manual_total_episode"), maxEpisode(stringValue(data, "lack_episode"))),
			WantedEpisodes: wantedEpisodes(season, stringValue(data, "lack_episode")),
			Status:         subscriptionStatus(stringValue(data, "state")), SourceName: firstNonEmpty(stringValue(data, "username"), "MoviePilot"),
			RuleProfileKey: stringValue(data, ruleSelectionMetadataKey),
			CreatedAt:      parseSourceTime(stringValue(data, "date")), Metadata: metadata,
		})
	case "transfer_history":
		if h.instance.Transfers == nil {
			return pluginsdk.HostWriteResult{}, fmt.Errorf("宿主未提供整理记录能力")
		}
		sourcePath := firstNonEmpty(stringValue(data, "src"), nestedString(data, "src_fileitem", "path"))
		targetPath := firstNonEmpty(stringValue(data, "dest"), nestedString(data, "dest_fileitem", "path"))
		season, episode := parseSeasonEpisode(firstNonEmpty(stringValue(data, "seasons")+stringValue(data, "episodes"), sourcePath, targetPath))
		size := firstPositive64(nestedInt64(data, "dest_fileitem", "size"), nestedInt64(data, "src_fileitem", "size"))
		return h.instance.Transfers.UpsertTransfer(ctx, pluginsdk.TransferWrite{
			TargetID: item.TargetID, IdempotencyKey: "moviepilot:transfer:" + item.SourceID,
			Media: mediaIdentity(data), DownloadHash: stringValue(data, "download_hash"),
			SourcePath: sourcePath, TargetPath: targetPath, Operation: transferOperation(stringValue(data, "mode")),
			Status: transferStatus(data["status"]), Error: stringValue(data, "errmsg"), SizeBytes: size,
			SeasonNumber: season, EpisodeNumber: episode, OccurredAt: parseSourceTime(stringValue(data, "date")), Metadata: metadata,
		})
	case "subscribe_history":
		return pluginsdk.HostWriteResult{TargetID: "archive:" + item.SourceType + ":" + item.SourceID, Change: "created"}, nil
	default:
		return pluginsdk.HostWriteResult{TargetID: item.TargetID, Change: "unchanged"}, nil
	}
}

func sourcePriority(sourceType string) int {
	for index, source := range []string{"rules", "sites", "subscriptions", "transfer_history", "subscribe_history"} {
		if source == sourceType {
			return index
		}
	}
	return 100
}

func selectedSet(values []string) map[string]bool {
	out := map[string]bool{}
	for _, value := range values {
		out[strings.TrimSpace(value)] = true
	}
	return out
}
