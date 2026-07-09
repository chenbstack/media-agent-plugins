package main

import (
	"encoding/json"
	"testing"
)

func TestQuotaFromResponse115OpenUserInfo(t *testing.T) {
	total, used, ok := quotaFromResponse115(map[string]any{
		"data": map[string]any{
			"rt_space_info": map[string]any{
				"all_total":  map[string]any{"size": int64(1000)},
				"all_remain": map[string]any{"size": int64(250)},
			},
		},
	})
	if !ok || total != 1000 || used != 750 {
		t.Fatalf("quota = %d/%d ok=%v, want 750/1000 true", used, total, ok)
	}
}

func TestQuotaFromResponse115IndexInfo(t *testing.T) {
	total, used, ok := quotaFromResponse115(map[string]any{
		"state": true,
		"data": map[string]any{
			"space_info": map[string]any{
				"total_size": int64(4096),
				"used_size":  int64(1024),
			},
		},
	})
	if !ok || total != 4096 || used != 1024 {
		t.Fatalf("quota = %d/%d ok=%v, want 1024/4096 true", used, total, ok)
	}
}

func TestQuotaFromResponse115DecimalJSONNumber(t *testing.T) {
	total, used, ok := quotaFromResponse115(map[string]any{
		"data": map[string]any{
			"space_info": map[string]any{
				"all_total":  map[string]any{"size": json.Number("110717638682843.4")},
				"all_remain": map[string]any{"size": json.Number("107482844631662.4")},
				"all_use":    map[string]any{"size": json.Number("3234794051181")},
			},
		},
	})
	if !ok || total != 110717638682843 || used != 3234794051181 {
		t.Fatalf("quota = %d/%d ok=%v, want 3234794051181/110717638682843 true", used, total, ok)
	}
}
