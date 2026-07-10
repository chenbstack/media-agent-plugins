package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/chenbstack/media-agent-plugin-sdk-go"
)

type stateStore struct {
	db    pluginsdk.PluginDB
	items string
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
	store := &stateStore{db: db, items: items}
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
	return store, nil
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
	return map[string]any{"total": total, "sources": counts}, nil
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
