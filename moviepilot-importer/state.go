package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/chenbstack/media-agent-plugin-sdk-go"
)

type stateStore struct {
	db       pluginsdk.PluginDB
	items    string
	runs     string
	runTasks string
}

type stagedItem struct {
	SourceType    string
	SourceID      string
	Hash          string
	DataJSON      string
	AppliedHash   string
	AppliedStatus string
	AppliedError  string
	TargetID      string
}

func newStateStore(ctx context.Context, db pluginsdk.PluginDB) (*stateStore, error) {
	if db == nil {
		return nil, fmt.Errorf("宿主未提供插件私有数据库")
	}
	items, err := db.TableName("items")
	if err != nil {
		return nil, err
	}
	runs, err := db.TableName("runs")
	if err != nil {
		return nil, err
	}
	runTasks, err := db.TableName("run_tasks")
	if err != nil {
		return nil, err
	}
	store := &stateStore{db: db, items: items, runs: runs, runTasks: runTasks}
	_, err = db.Exec(ctx, `CREATE TABLE IF NOT EXISTS `+items+` (
source_type TEXT NOT NULL,
source_id TEXT NOT NULL,
item_hash TEXT NOT NULL,
data_json TEXT NOT NULL,
applied_hash TEXT NOT NULL DEFAULT '',
applied_status TEXT NOT NULL DEFAULT '',
applied_error TEXT NOT NULL DEFAULT '',
target_id TEXT NOT NULL DEFAULT '',
created_at TEXT NOT NULL,
updated_at TEXT NOT NULL,
PRIMARY KEY(source_type, source_id)
)`)
	if err != nil {
		return nil, err
	}
	_, err = db.Exec(ctx, `CREATE TABLE IF NOT EXISTS `+runs+` (
run_id TEXT PRIMARY KEY,
status TEXT NOT NULL,
message TEXT NOT NULL DEFAULT '',
started_at TEXT NOT NULL,
updated_at TEXT NOT NULL,
finished_at TEXT
)`)
	if err != nil {
		return nil, err
	}
	_, err = db.Exec(ctx, `CREATE TABLE IF NOT EXISTS `+runTasks+` (
run_id TEXT NOT NULL,
task_id TEXT NOT NULL,
name TEXT NOT NULL,
position INTEGER NOT NULL DEFAULT 0,
status TEXT NOT NULL,
current_count INTEGER NOT NULL DEFAULT 0,
total_count INTEGER NOT NULL DEFAULT 0,
message TEXT NOT NULL DEFAULT '',
updated_at TEXT NOT NULL,
PRIMARY KEY(run_id, task_id)
)`)
	if err != nil {
		return nil, err
	}
	return store, nil
}

var migrationTaskNames = map[string]string{
	"sites":             "站点",
	"subscriptions":     "订阅",
	"subscribe_history": "订阅历史快照",
	"transfer_history":  "整理历史",
}

func (s *stateStore) startRun(ctx context.Context, sources []string) (string, error) {
	now := time.Now().UTC()
	timestamp := now.Format(time.RFC3339Nano)
	_, _ = s.db.Exec(ctx, `UPDATE `+s.runs+`
SET status = 'failed', message = '同步已中断', updated_at = ?, finished_at = ?
WHERE status = 'running'`, timestamp, timestamp)
	runID := fmt.Sprintf("moviepilot-%d", now.UnixNano())
	if _, err := s.db.Exec(ctx, `INSERT INTO `+s.runs+`
(run_id, status, message, started_at, updated_at)
VALUES (?, 'running', '准备同步', ?, ?)`, runID, timestamp, timestamp); err != nil {
		return "", err
	}
	for position, source := range sources {
		name := firstNonEmpty(migrationTaskNames[source], source)
		if _, err := s.db.Exec(ctx, `INSERT INTO `+s.runTasks+`
(run_id, task_id, name, position, status, updated_at)
VALUES (?, ?, ?, ?, 'pending', ?)`, runID, source, name, position, timestamp); err != nil {
			return "", err
		}
	}
	return runID, nil
}

func (s *stateStore) updateTask(ctx context.Context, runID, taskID, status string, current, total int, message string) error {
	if current < 0 {
		current = 0
	}
	if total < 0 {
		total = 0
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := s.db.Exec(ctx, `UPDATE `+s.runTasks+`
SET status = ?, current_count = ?, total_count = ?, message = ?, updated_at = ?
WHERE run_id = ? AND task_id = ?`, status, current, total, message, now, runID, taskID); err != nil {
		return err
	}
	runMessage := firstNonEmpty(migrationTaskNames[taskID], taskID)
	if strings.TrimSpace(message) != "" {
		runMessage += " · " + strings.TrimSpace(message)
	}
	_, err := s.db.Exec(ctx, `UPDATE `+s.runs+` SET message = ?, updated_at = ? WHERE run_id = ?`, runMessage, now, runID)
	return err
}

func (s *stateStore) finishRun(ctx context.Context, runID, status, message string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.Exec(ctx, `UPDATE `+s.runs+`
SET status = ?, message = ?, updated_at = ?, finished_at = ?
WHERE run_id = ?`, status, message, now, now, runID)
	return err
}

func (s *stateStore) stage(ctx context.Context, sourceType string, item exportItem) (string, error) {
	rows, err := s.db.Query(ctx, `SELECT item_hash FROM `+s.items+` WHERE source_type = ? AND source_id = ?`, sourceType, item.SourceID)
	if err != nil {
		return "", err
	}
	change := "created"
	if len(rows) > 0 {
		if rowString(rows[0], "item_hash") == item.Hash {
			change = "unchanged"
		} else {
			change = "updated"
		}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err = s.db.Exec(ctx, `INSERT INTO `+s.items+`
(source_type, source_id, item_hash, data_json, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(source_type, source_id) DO UPDATE SET
 item_hash = excluded.item_hash,
 data_json = excluded.data_json,
 updated_at = excluded.updated_at`, sourceType, item.SourceID, item.Hash, string(item.Data), now, now)
	return change, err
}

func (s *stateStore) list(ctx context.Context, selected map[string]bool) ([]stagedItem, error) {
	rows, err := s.db.Query(ctx, `SELECT source_type, source_id, item_hash, data_json,
applied_hash, applied_status, applied_error, target_id
FROM `+s.items+` ORDER BY source_type, source_id`)
	if err != nil {
		return nil, err
	}
	items := make([]stagedItem, 0, len(rows))
	for _, row := range rows {
		sourceType := rowString(row, "source_type")
		if len(selected) > 0 && !selected[sourceType] {
			continue
		}
		items = append(items, stagedItem{
			SourceType:    sourceType,
			SourceID:      rowString(row, "source_id"),
			Hash:          rowString(row, "item_hash"),
			DataJSON:      rowString(row, "data_json"),
			AppliedHash:   rowString(row, "applied_hash"),
			AppliedStatus: rowString(row, "applied_status"),
			AppliedError:  rowString(row, "applied_error"),
			TargetID:      rowString(row, "target_id"),
		})
	}
	return items, nil
}

func (s *stateStore) mark(ctx context.Context, item stagedItem, status, targetID string, applyErr error) error {
	appliedHash := ""
	errorText := ""
	if status == "success" {
		appliedHash = item.Hash
	}
	if applyErr != nil {
		errorText = applyErr.Error()
	}
	_, err := s.db.Exec(ctx, `UPDATE `+s.items+`
SET applied_hash = ?, applied_status = ?, applied_error = ?, target_id = ?, updated_at = ?
WHERE source_type = ? AND source_id = ?`, appliedHash, status, errorText, targetID,
		time.Now().UTC().Format(time.RFC3339), item.SourceType, item.SourceID)
	return err
}

func (s *stateStore) status(ctx context.Context) (map[string]any, error) {
	rows, err := s.db.Query(ctx, `SELECT source_type, applied_status, COUNT(*) AS count
FROM `+s.items+` GROUP BY source_type, applied_status ORDER BY source_type, applied_status`)
	if err != nil {
		return nil, err
	}
	counts := map[string]map[string]int{}
	total := 0
	for _, row := range rows {
		sourceType := rowString(row, "source_type")
		status := firstNonEmpty(rowString(row, "applied_status"), "pending")
		if counts[sourceType] == nil {
			counts[sourceType] = map[string]int{}
		}
		count := rowInt(row, "count")
		counts[sourceType][status] = count
		total += count
	}
	result := map[string]any{
		"total": total, "sources": counts, "status": "idle", "message": "尚未执行同步",
		"progress": map[string]any{"current": 0, "total": 0}, "tasks": []map[string]any{},
	}
	runs, err := s.db.Query(ctx, `SELECT run_id, status, message, started_at, updated_at, finished_at
FROM `+s.runs+` ORDER BY started_at DESC LIMIT 1`)
	if err != nil {
		return nil, err
	}
	if len(runs) == 0 {
		return result, nil
	}
	run := runs[0]
	runID := rowString(run, "run_id")
	taskRows, err := s.db.Query(ctx, `SELECT task_id, name, status, current_count, total_count, message, updated_at
FROM `+s.runTasks+` WHERE run_id = ? ORDER BY position, task_id`, runID)
	if err != nil {
		return nil, err
	}
	tasks := make([]map[string]any, 0, len(taskRows))
	completed := 0
	for _, row := range taskRows {
		taskStatus := firstNonEmpty(rowString(row, "status"), "pending")
		if taskStatus == "completed" || taskStatus == "partial" || taskStatus == "failed" {
			completed++
		}
		tasks = append(tasks, map[string]any{
			"id": rowString(row, "task_id"), "name": rowString(row, "name"), "status": taskStatus,
			"current": rowInt(row, "current_count"), "total": rowInt(row, "total_count"),
			"message": rowString(row, "message"), "updated_at": rowString(row, "updated_at"),
		})
	}
	result["run_id"] = runID
	result["status"] = firstNonEmpty(rowString(run, "status"), "idle")
	result["message"] = rowString(run, "message")
	result["started_at"] = rowString(run, "started_at")
	result["updated_at"] = rowString(run, "updated_at")
	result["finished_at"] = rowString(run, "finished_at")
	result["progress"] = map[string]any{"current": completed, "total": len(tasks)}
	result["tasks"] = tasks
	return result, nil
}

func rowString(row map[string]any, key string) string {
	value := row[key]
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case []byte:
		return strings.TrimSpace(string(typed))
	case nil:
		return ""
	default:
		return strings.TrimSpace(fmt.Sprint(typed))
	}
}

func rowInt(row map[string]any, key string) int {
	value := rowString(row, key)
	var result int
	_, _ = fmt.Sscan(value, &result)
	return result
}
