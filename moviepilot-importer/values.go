package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/chenbstack/media-agent-plugin-sdk-go"
)

func stringValue(data map[string]any, key string) string {
	value := data[key]
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case json.Number:
		return typed.String()
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	case nil:
		return ""
	default:
		return strings.TrimSpace(fmt.Sprint(typed))
	}
}

func intValue(data map[string]any, key string) int {
	value := stringValue(data, key)
	parsed, _ := strconv.Atoi(strings.TrimSpace(value))
	return parsed
}

func boolValue(data map[string]any, key string, fallback bool) bool {
	value, ok := data[key]
	if !ok || value == nil {
		return fallback
	}
	switch typed := value.(type) {
	case bool:
		return typed
	case float64:
		return typed != 0
	case int:
		return typed != 0
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "true", "1", "yes", "on":
			return true
		case "false", "0", "no", "off":
			return false
		}
	}
	return fallback
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func firstPositive(values ...int) int {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

func firstPositive64(values ...int64) int64 {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

func nestedString(data map[string]any, object, key string) string {
	nested, _ := data[object].(map[string]any)
	return stringValue(nested, key)
}

func nestedInt64(data map[string]any, object, key string) int64 {
	value := nestedString(data, object, key)
	parsed, _ := strconv.ParseInt(value, 10, 64)
	return parsed
}

func nonRedactedString(value any) string {
	text := strings.TrimSpace(fmt.Sprint(value))
	if text == "" || strings.EqualFold(text, "<nil>") || strings.Contains(strings.ToLower(text), "redact") || text == "***" {
		return ""
	}
	return text
}

func mediaIdentity(data map[string]any) pluginsdk.MediaIdentity {
	mediaType := "series"
	switch strings.ToLower(firstNonEmpty(stringValue(data, "type"), stringValue(data, "media_type"))) {
	case "movie", "电影":
		mediaType = "movie"
	}
	return pluginsdk.MediaIdentity{
		MediaType:     mediaType,
		Title:         firstNonEmpty(stringValue(data, "name"), stringValue(data, "title")),
		OriginalTitle: stringValue(data, "original_title"),
		Year:          intValue(data, "year"),
		TMDBID:        int64(intValue(data, "tmdbid")),
		IMDBID:        stringValue(data, "imdbid"),
		TVDBID:        int64(intValue(data, "tvdbid")),
		DoubanID:      stringValue(data, "doubanid"),
		PosterURL:     stringValue(data, "poster"),
		BackdropURL:   stringValue(data, "backdrop"),
		Overview:      stringValue(data, "description"),
	}
}

func subscriptionStatus(value string) string {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "P", "PAUSED":
		return "paused"
	default:
		return "active"
	}
}

func transferOperation(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "copy":
		return "copy"
	case "move":
		return "move"
	case "softlink", "symlink", "soft_link":
		return "symlink"
	case "link", "hardlink", "hard_link":
		return "hardlink"
	case "server_copy":
		return "server_copy"
	default:
		return "auto"
	}
}

func transferStatus(value any) string {
	switch typed := value.(type) {
	case bool:
		if !typed {
			return "failed"
		}
	case float64:
		if typed == 0 {
			return "failed"
		}
	case string:
		if strings.EqualFold(strings.TrimSpace(typed), "false") || strings.EqualFold(strings.TrimSpace(typed), "failed") || strings.TrimSpace(typed) == "0" {
			return "failed"
		}
	}
	return "completed"
}

func parseSourceTime(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed.UTC().Format(time.RFC3339)
	}
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		location = time.Local
	}
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02"} {
		if parsed, err := time.ParseInLocation(layout, value, location); err == nil {
			return parsed.UTC().Format(time.RFC3339)
		}
	}
	return ""
}

var seasonEpisodePattern = regexp.MustCompile(`(?i)S0*([0-9]{1,3}).*?E0*([0-9]{1,4})`)
var episodePattern = regexp.MustCompile(`(?i)E0*([0-9]{1,4})`)
var numberRangePattern = regexp.MustCompile(`([0-9]{1,4})\s*-\s*([0-9]{1,4})`)
var plainNumberPattern = regexp.MustCompile(`(?:^|[,\s])([0-9]{1,4})(?:$|[,\s])`)

func parseSeasonEpisode(value string) (int, int) {
	if match := seasonEpisodePattern.FindStringSubmatch(value); len(match) == 3 {
		return atoi(match[1]), atoi(match[2])
	}
	if match := episodePattern.FindStringSubmatch(value); len(match) == 2 {
		return 0, atoi(match[1])
	}
	return 0, 0
}

func maxEpisode(value string) int {
	max := 0
	for _, match := range episodePattern.FindAllStringSubmatch(value, -1) {
		if len(match) == 2 && atoi(match[1]) > max {
			max = atoi(match[1])
		}
	}
	for _, match := range numberRangePattern.FindAllStringSubmatch(value, -1) {
		if len(match) == 3 && atoi(match[2]) > max {
			max = atoi(match[2])
		}
	}
	return max
}

func wantedEpisodes(season int, value string) []pluginsdk.EpisodeSelection {
	seen := map[int]bool{}
	for _, match := range episodePattern.FindAllStringSubmatch(value, -1) {
		if len(match) == 2 {
			seen[atoi(match[1])] = true
		}
	}
	for _, match := range numberRangePattern.FindAllStringSubmatch(value, -1) {
		if len(match) != 3 {
			continue
		}
		start, end := atoi(match[1]), atoi(match[2])
		if start > 0 && end >= start && end-start <= 1000 {
			for episode := start; episode <= end; episode++ {
				seen[episode] = true
			}
		}
	}
	if len(seen) == 0 {
		for _, match := range plainNumberPattern.FindAllStringSubmatch(" "+value+" ", -1) {
			if len(match) == 2 {
				seen[atoi(match[1])] = true
			}
		}
	}
	result := make([]pluginsdk.EpisodeSelection, 0, len(seen))
	maxSeen := maxEpisode(value)
	for episode := range seen {
		if episode > maxSeen {
			maxSeen = episode
		}
	}
	for episode := 1; episode <= maxSeen; episode++ {
		if seen[episode] {
			result = append(result, pluginsdk.EpisodeSelection{Season: season, Episode: episode})
		}
	}
	return result
}

func atoi(value string) int {
	parsed, _ := strconv.Atoi(value)
	return parsed
}

func joinPath(parent, child string) string {
	if strings.TrimSpace(parent) == "" {
		return strings.TrimSpace(child)
	}
	if strings.TrimSpace(child) == "" {
		return strings.TrimSpace(parent)
	}
	return filepath.Join(parent, child)
}
