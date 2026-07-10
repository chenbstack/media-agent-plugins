package main

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/chenbstack/media-agent-plugin-sdk-go"
)

func TestConvertMoviePilotNativeRulesAndPriorityOrder(t *testing.T) {
	conversion := convertRuleForTest(t,
		"BLU & 4K & CNSUB & GZ & SPECSUB & BLURAY & UHD & H265 & H264 & DOLBY & ATMOS & HDR & SDR & REMUX & WEBDL & FREE & CNVOI & HKVOI & 60FPS & 3D > 1080P > 720P",
		nil,
	)
	assertSelected(t, conversion.Write, "resolution", []string{"4K", "1080P", "720P"})
	assertSelected(t, conversion.Write, "quality", []string{"蓝光原盘", "BLURAY", "UHD", "REMUX", "WEB-DL"})
	assertSelected(t, conversion.Write, "subtitle", []string{"中文字幕", "特效字幕"})
	assertSelected(t, conversion.Write, "source", []string{"官种"})
	assertSelected(t, conversion.Write, "codec", []string{"H265", "H264"})
	assertSelected(t, conversion.Write, "visualEffect", []string{"杜比视界", "HDR", "SDR", "60fps", "3D"})
	assertSelected(t, conversion.Write, "audioSpec", []string{"杜比全景声"})
	assertSelected(t, conversion.Write, "promotion", []string{"免费"})
	assertSelected(t, conversion.Write, "dubbing", []string{"国语", "粤语"})
	assertPrerequisiteSelected(t, conversion.Write, "resolution", []string{"4K", "1080P", "720P"})
	assertPrerequisiteSelected(t, conversion.Write, "codec", nil)
	if conversion.Write.Status != "enabled" || !warningsContain(conversion.Warnings, "多级优先级") {
		t.Fatalf("conversion status/warnings = %q, %v", conversion.Write.Status, conversion.Warnings)
	}
}

func TestConvertMoviePilotOnlyUsesSafeCommonNativePrerequisites(t *testing.T) {
	tests := []struct {
		name       string
		expression string
		want       map[string][]string
	}{
		{name: "same dimension alternatives", expression: "4K | 1080P", want: map[string][]string{"resolution": {"4K", "1080P"}}},
		{name: "different dimension alternatives", expression: "4K | CNSUB", want: map[string][]string{}},
		{name: "paired priority levels", expression: "4K & H265 > 1080P & H264", want: map[string][]string{
			"resolution": {"4K", "1080P"}, "codec": {"H265", "H264"},
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			conversion := convertRuleForTest(t, test.expression, nil)
			for _, id := range []string{"resolution", "subtitle", "codec"} {
				assertPrerequisiteSelected(t, conversion.Write, id, test.want[id])
			}
		})
	}
}

func TestConvertMoviePilotNegativeRulesAcrossBooleanBranches(t *testing.T) {
	tests := []struct {
		name       string
		expression string
		want       []string
	}{
		{name: "negative common after parenthesized or", expression: "(4K | 1080P) & !3D", want: []string{"排除: 3D"}},
		{name: "negative only in one or branch is not global", expression: "(4K & !3D) | 1080P", want: nil},
		{name: "de morgan keeps common negatives", expression: "!(3D | SDR) & 4K", want: []string{"排除: 3D", "排除: SDR"}},
		{name: "priority branch prevents unsafe negative", expression: "4K & !3D > 1080P", want: nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			conversion := convertRuleForTest(t, test.expression, nil)
			assertSelected(t, conversion.Write, "negativeFilter", test.want)
		})
	}
}

func TestConvertMoviePilotCustomRuleAllSupportedConstraints(t *testing.T) {
	conversion := convertRuleForTest(t, "USER1", []moviePilotCustomRule{{
		ID: "USER1", Name: "收藏规则", Include: `DIY|@Home`, Exclude: `CAM|TS`,
		SizeRange: "1024-4096", Seeders: "5", PublishTime: "60-1440",
	}})
	prereq := conversion.Write.Prerequisites
	if prereq.Size.MinGB == nil || *prereq.Size.MinGB != 1 || prereq.Size.MaxGB == nil || *prereq.Size.MaxGB != 4 {
		t.Fatalf("size = %+v", prereq.Size)
	}
	if prereq.MinSeeders == nil || *prereq.MinSeeders != 5 || prereq.MinAgeMinutes == nil || *prereq.MinAgeMinutes != 60 || prereq.MaxAgeMinutes == nil || *prereq.MaxAgeMinutes != 1440 {
		t.Fatalf("numeric prerequisites = %+v", prereq)
	}
	assertPatternMatches(t, prereq.IncludeKeywordPattern, "Movie.2026.DIY")
	assertPatternMatches(t, prereq.ExcludeKeywordPattern, "Movie.CAM")
	if !prereq.IncludePatternAdvanced || !prereq.ExcludePatternAdvanced {
		t.Fatalf("advanced flags = %+v", prereq)
	}
}

func TestConvertMoviePilotCustomSizeForms(t *testing.T) {
	tests := []struct {
		name    string
		value   any
		minGB   *float64
		maxGB   *float64
		warning bool
	}{
		{name: "greater than", value: ">2048", minGB: floatPtr(2)},
		{name: "less than", value: "<512", maxGB: floatPtr(0.5)},
		{name: "decimal interval", value: "512.5-1536", minGB: floatPtr(512.5 / 1024), maxGB: floatPtr(1.5)},
		{name: "plain number invalid in MoviePilot", value: "1024", warning: true},
		{name: "reversed interval", value: "4096-1024", warning: true},
		{name: "non numeric", value: "large", warning: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			conversion := convertRuleForTest(t, "SIZE", []moviePilotCustomRule{{ID: "SIZE", SizeRange: test.value}})
			assertOptionalFloat(t, conversion.Write.Prerequisites.Size.MinGB, test.minGB)
			assertOptionalFloat(t, conversion.Write.Prerequisites.Size.MaxGB, test.maxGB)
			if warningsContain(conversion.Warnings, "大小范围") != test.warning {
				t.Fatalf("warnings = %v", conversion.Warnings)
			}
		})
	}
}

func TestConvertMoviePilotCustomAgeAndSeederEdgeCases(t *testing.T) {
	tests := []struct {
		name       string
		seeders    any
		age        any
		wantSeed   *int
		wantMinAge *int
		wantMaxAge *int
		warning    bool
	}{
		{name: "minimum age", seeders: "1", age: "30", wantSeed: intPtrTest(1), wantMinAge: intPtrTest(30)},
		{name: "fractional age rounded", seeders: float64(3), age: "30.6-60.4", wantSeed: intPtrTest(3), wantMinAge: intPtrTest(31), wantMaxAge: intPtrTest(60)},
		{name: "zero seeders means unset", seeders: "0", age: nil},
		{name: "negative seeders", seeders: "-1", age: nil, warning: true},
		{name: "reversed age", seeders: nil, age: "120-60", warning: true},
		{name: "invalid age", seeders: nil, age: "recent", warning: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			conversion := convertRuleForTest(t, "LIMIT", []moviePilotCustomRule{{ID: "LIMIT", Seeders: test.seeders, PublishTime: test.age}})
			assertOptionalInt(t, conversion.Write.Prerequisites.MinSeeders, test.wantSeed)
			assertOptionalInt(t, conversion.Write.Prerequisites.MinAgeMinutes, test.wantMinAge)
			assertOptionalInt(t, conversion.Write.Prerequisites.MaxAgeMinutes, test.wantMaxAge)
			if (warningsContain(conversion.Warnings, "无效") || warningsContain(conversion.Warnings, "已跳过")) != test.warning {
				t.Fatalf("warnings = %v", conversion.Warnings)
			}
		})
	}
}

func TestConvertMoviePilotCustomBooleanExpressions(t *testing.T) {
	custom := []moviePilotCustomRule{
		{ID: "A", Include: "AAA", Seeders: "5", PublishTime: "60-120"},
		{ID: "B", Include: "BBB", Seeders: "2", PublishTime: "10-240"},
		{ID: "BLOCK", Exclude: "BLOCKED"},
	}
	t.Run("and requires both includes in either text order", func(t *testing.T) {
		conversion := convertRuleForTest(t, "A & B & BLOCK", custom)
		pattern := conversion.Write.Prerequisites.IncludeKeywordPattern
		assertPatternMatches(t, pattern, "AAA something BBB")
		assertPatternMatches(t, pattern, "BBB something AAA")
		assertPatternDoesNotMatch(t, pattern, "AAA only")
		if got := conversion.Write.Prerequisites.MinSeeders; got == nil || *got != 5 {
			t.Fatalf("and min seeders = %v", got)
		}
		assertPatternMatches(t, conversion.Write.Prerequisites.ExcludeKeywordPattern, "BLOCKED")
	})
	t.Run("or uses safe numeric envelope", func(t *testing.T) {
		conversion := convertRuleForTest(t, "A | B", custom)
		prereq := conversion.Write.Prerequisites
		if prereq.MinSeeders == nil || *prereq.MinSeeders != 2 || prereq.MinAgeMinutes == nil || *prereq.MinAgeMinutes != 10 || prereq.MaxAgeMinutes == nil || *prereq.MaxAgeMinutes != 240 {
			t.Fatalf("or envelope = %+v", prereq)
		}
		assertPatternMatches(t, prereq.IncludeKeywordPattern, "AAA")
		assertPatternMatches(t, prereq.IncludeKeywordPattern, "BBB")
	})
	t.Run("branch without include removes unsafe global include", func(t *testing.T) {
		conversion := convertRuleForTest(t, "A | 4K", custom)
		if conversion.Write.Prerequisites.IncludeKeywordPattern != "" {
			t.Fatalf("include pattern should be empty: %s", conversion.Write.Prerequisites.IncludeKeywordPattern)
		}
	})
	t.Run("negated include becomes exclude", func(t *testing.T) {
		conversion := convertRuleForTest(t, "4K & !A", []moviePilotCustomRule{{ID: "A", Include: "UNWANTED"}})
		assertPatternMatches(t, conversion.Write.Prerequisites.ExcludeKeywordPattern, "UNWANTED")
	})
	t.Run("negated exclude becomes include", func(t *testing.T) {
		conversion := convertRuleForTest(t, "!BLOCK", []moviePilotCustomRule{{ID: "BLOCK", Exclude: "REQUIRED"}})
		assertPatternMatches(t, conversion.Write.Prerequisites.IncludeKeywordPattern, "REQUIRED")
	})
	t.Run("negated composite is explicitly degraded", func(t *testing.T) {
		conversion := convertRuleForTest(t, "!A", custom)
		if !warningsContain(conversion.Warnings, "取反包含复合条件") || conversion.Write.Status != "disabled" {
			t.Fatalf("conversion = %+v warnings=%v", conversion.Write, conversion.Warnings)
		}
	})
}

func TestConvertMoviePilotUserRuleOverridesNativeRuleID(t *testing.T) {
	conversion := convertRuleForTest(t, "4K", []moviePilotCustomRule{{ID: "4K", Include: "CUSTOM_QUALITY"}})
	assertSelected(t, conversion.Write, "resolution", nil)
	assertPrerequisiteSelected(t, conversion.Write, "resolution", nil)
	assertPatternMatches(t, conversion.Write.Prerequisites.IncludeKeywordPattern, "CUSTOM_QUALITY")
}

func TestConvertMoviePilotRegexCompatibilityAndSpecialValues(t *testing.T) {
	tests := []struct {
		name        string
		custom      moviePilotCustomRule
		wantEnabled bool
		warning     string
	}{
		{name: "comma stays inside one regex", custom: moviePilotCustomRule{ID: "R", Include: `A{1,3}`}, wantEnabled: true},
		{name: "array patterns accepted defensively", custom: moviePilotCustomRule{ID: "R", Include: []any{"AAA", "BBB"}}, wantEnabled: true},
		{name: "python lookbehind rejected by re2", custom: moviePilotCustomRule{ID: "R", Include: `(?<!WEB-)RIP`}, warning: "不兼容宿主 RE2"},
		{name: "empty custom rule disabled", custom: moviePilotCustomRule{ID: "R"}, warning: "没有可安全映射"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			conversion := convertRuleForTest(t, "R", []moviePilotCustomRule{test.custom})
			if (conversion.Write.Status == "enabled") != test.wantEnabled {
				t.Fatalf("status = %q warnings=%v", conversion.Write.Status, conversion.Warnings)
			}
			if test.warning != "" && !warningsContain(conversion.Warnings, test.warning) {
				t.Fatalf("warnings = %v", conversion.Warnings)
			}
			if test.name == "comma stays inside one regex" {
				assertPatternMatches(t, conversion.Write.Prerequisites.IncludeKeywordPattern, "AAA")
			}
		})
	}
}

func TestConvertMoviePilotInvalidAndExtremeExpressions(t *testing.T) {
	tests := []struct {
		name       string
		expression string
		warning    string
	}{
		{name: "empty", expression: "", warning: "规则串为空"},
		{name: "unbalanced parentheses", expression: "(4K | 1080P", warning: "括号不匹配"},
		{name: "missing operand", expression: "4K &", warning: "缺少规则名"},
		{name: "unknown only", expression: "UNKNOWN", warning: "未识别规则"},
		{name: "unsupported character", expression: "RULE-NAME", warning: "无法识别的字符"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			conversion := convertRuleForTest(t, test.expression, nil)
			if conversion.Write.Status != "disabled" || !warningsContain(conversion.Warnings, test.warning) {
				t.Fatalf("status=%q warnings=%v", conversion.Write.Status, conversion.Warnings)
			}
		})
	}

	// Nine independent OR pairs expand to 512 DNF branches and must hit the
	// safety cap instead of allocating exponentially.
	extreme := "(A|B)&(C|D)&(E|F)&(G|H)&(I|J)&(K|L)&(M|N)"
	conversion := convertRuleForTest(t, extreme, nil)
	if !warningsContain(conversion.Warnings, "超过 64 个分支") {
		t.Fatalf("extreme warnings = %v", conversion.Warnings)
	}
}

func TestConvertMoviePilotMultipleRuleGroupsAndScope(t *testing.T) {
	bundle := moviePilotRuleBundle{
		Kind: "profile", ProfileKey: "moviepilot:rule-profile:multi", Name: "组合规则", Priority: 3,
		Groups: []moviePilotRuleGroup{
			{Name: "电影", RuleString: "4K", MediaType: "电影", Category: "动作片"},
			{Name: "剧集", RuleString: "CNSUB & !3D", MediaType: "电视剧", Category: "国产剧"},
		},
	}
	conversion, err := convertMoviePilotRuleBundle(bundle, testRuleCatalog())
	if err != nil {
		t.Fatal(err)
	}
	if conversion.Write.MediaType != "all" || conversion.Write.MediaCategory != "全部" || conversion.Write.Priority != 3 {
		t.Fatalf("scope/priority = %+v", conversion.Write)
	}
	assertSelected(t, conversion.Write, "resolution", []string{"4K"})
	assertSelected(t, conversion.Write, "subtitle", []string{"中文字幕"})
	assertSelected(t, conversion.Write, "negativeFilter", []string{"排除: 3D"})
	assertPrerequisiteSelected(t, conversion.Write, "resolution", []string{"4K"})
	assertPrerequisiteSelected(t, conversion.Write, "subtitle", []string{"中文字幕"})
}

func TestConvertMoviePilotContradictoryCombinedGroupsAreDisabled(t *testing.T) {
	bundle := moviePilotRuleBundle{
		Kind: "profile", ProfileKey: "moviepilot:rule-profile:contradictory", Name: "冲突组合",
		Groups: []moviePilotRuleGroup{
			{Name: "只要4K", RuleString: "4K"},
			{Name: "只要1080P", RuleString: "1080P"},
		},
	}
	conversion, err := convertMoviePilotRuleBundle(bundle, testRuleCatalog())
	if err != nil {
		t.Fatal(err)
	}
	if conversion.Write.Status != "disabled" || !warningsContain(conversion.Warnings, "没有共同可选值") {
		t.Fatalf("conversion = %+v warnings=%v", conversion.Write, conversion.Warnings)
	}
}

func TestFilterConvertedRulesAgainstHostCatalog(t *testing.T) {
	catalog := pluginsdk.RuleCatalog{Dimensions: []pluginsdk.RuleDimensionDefinition{
		{ID: "resolution", Options: []string{"1080P"}},
	}}
	bundle := moviePilotRuleBundle{
		Kind: "profile", ProfileKey: "moviepilot:rule-profile:limited", Name: "受限宿主",
		Groups: []moviePilotRuleGroup{{Name: "受限宿主", RuleString: "4K & !3D > 1080P & !3D"}},
	}
	conversion, err := convertMoviePilotRuleBundle(bundle, catalog)
	if err != nil {
		t.Fatal(err)
	}
	assertSelected(t, conversion.Write, "resolution", []string{"1080P"})
	assertSelected(t, conversion.Write, "negativeFilter", nil)
	if !warningsContain(conversion.Warnings, "不支持取值 4K") || !warningsContain(conversion.Warnings, "不支持规则维度 negativeFilter") {
		t.Fatalf("warnings = %v", conversion.Warnings)
	}
}

func TestMapMoviePilotSort(t *testing.T) {
	selected, warnings := mapMoviePilotSort([]string{"torrent", "site", "upload", "seeder", "unknown", "torrent"}, testRuleCatalog())
	want := []string{"资源优先级", "站点优先级", "站点上传量", "资源做种数"}
	if strings.Join(selected, ",") != strings.Join(want, ",") || !warningsContain(warnings, "未识别排序项 unknown") {
		t.Fatalf("sort = %v warnings=%v", selected, warnings)
	}
}

func TestRegexAndCapsLargeCustomConditionSets(t *testing.T) {
	custom := []moviePilotCustomRule{}
	names := []string{}
	for _, id := range []string{"A", "B", "C", "D", "E"} {
		custom = append(custom, moviePilotCustomRule{ID: id, Include: id + id + id})
		names = append(names, id)
	}
	conversion := convertRuleForTest(t, strings.Join(names, " & "), custom)
	if !warningsContain(conversion.Warnings, "超过 4 个包含正则") {
		t.Fatalf("warnings = %v", conversion.Warnings)
	}
	assertPatternMatches(t, conversion.Write.Prerequisites.IncludeKeywordPattern, "AAA BBB CCC DDD")
}

func convertRuleForTest(t *testing.T, expression string, custom []moviePilotCustomRule) ruleConversion {
	t.Helper()
	bundle := moviePilotRuleBundle{
		Kind: "profile", ProfileKey: "moviepilot:rule-profile:test", Name: "测试规则", Priority: 1,
		Groups:      []moviePilotRuleGroup{{Name: "测试规则", RuleString: expression, MediaType: "电视剧"}},
		CustomRules: custom,
	}
	conversion, err := convertMoviePilotRuleBundle(bundle, testRuleCatalog())
	if err != nil {
		t.Fatal(err)
	}
	return conversion
}

func testRuleCatalog() pluginsdk.RuleCatalog {
	byDimension := map[string]map[string]bool{}
	allValues := map[string]bool{}
	for _, mapping := range moviePilotNativeRules {
		if byDimension[mapping.Dimension] == nil {
			byDimension[mapping.Dimension] = map[string]bool{}
		}
		byDimension[mapping.Dimension][mapping.Value] = true
		allValues[mapping.Value] = true
	}
	byDimension["negativeFilter"] = map[string]bool{}
	for value := range allValues {
		byDimension["negativeFilter"]["排除: "+value] = true
	}
	ids := make([]string, 0, len(byDimension))
	for id := range byDimension {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	dimensions := make([]pluginsdk.RuleDimensionDefinition, 0, len(ids))
	for _, id := range ids {
		options := make([]string, 0, len(byDimension[id]))
		for option := range byDimension[id] {
			options = append(options, option)
		}
		sort.Strings(options)
		dimensions = append(dimensions, pluginsdk.RuleDimensionDefinition{ID: id, Label: id, Options: options})
	}
	return pluginsdk.RuleCatalog{
		Dimensions:  dimensions,
		SortOptions: []string{"资源优先级", "站点优先级", "站点上传量", "资源做种数"},
	}
}

func assertSelected(t *testing.T, write pluginsdk.RuleProfileWrite, id string, want []string) {
	t.Helper()
	var got []string
	for _, dimension := range write.Preferences {
		if dimension.ID == id {
			got = dimension.Selected
			break
		}
	}
	if strings.Join(got, "\x1f") != strings.Join(want, "\x1f") {
		t.Fatalf("dimension %s = %v, want %v; all=%+v", id, got, want, write.Preferences)
	}
}

func assertPrerequisiteSelected(t *testing.T, write pluginsdk.RuleProfileWrite, id string, want []string) {
	t.Helper()
	var got []string
	for _, dimension := range write.Prerequisites.Dimensions {
		if dimension.ID == id {
			got = dimension.Selected
			break
		}
	}
	if strings.Join(got, "\x1f") != strings.Join(want, "\x1f") {
		t.Fatalf("prerequisite dimension %s = %v, want %v; all=%+v", id, got, want, write.Prerequisites.Dimensions)
	}
}

func assertPatternMatches(t *testing.T, pattern, value string) {
	t.Helper()
	compiled, err := regexp.Compile("(?i)" + pattern)
	if err != nil {
		t.Fatalf("compile %q: %v", pattern, err)
	}
	if !compiled.MatchString(value) {
		t.Fatalf("pattern %q should match %q", pattern, value)
	}
}

func assertPatternDoesNotMatch(t *testing.T, pattern, value string) {
	t.Helper()
	compiled, err := regexp.Compile("(?i)" + pattern)
	if err != nil {
		t.Fatalf("compile %q: %v", pattern, err)
	}
	if compiled.MatchString(value) {
		t.Fatalf("pattern %q should not match %q", pattern, value)
	}
}

func warningsContain(warnings []string, text string) bool {
	return strings.Contains(strings.Join(warnings, "\n"), text)
}

func assertOptionalFloat(t *testing.T, got, want *float64) {
	t.Helper()
	if got == nil || want == nil {
		if got != nil || want != nil {
			t.Fatalf("float pointer = %v, want %v", got, want)
		}
		return
	}
	if *got != *want {
		t.Fatalf("float = %v, want %v", *got, *want)
	}
}

func assertOptionalInt(t *testing.T, got, want *int) {
	t.Helper()
	if got == nil || want == nil {
		if got != nil || want != nil {
			t.Fatalf("int pointer = %v, want %v", got, want)
		}
		return
	}
	if *got != *want {
		t.Fatalf("int = %v, want %v", *got, *want)
	}
}

func floatPtr(value float64) *float64 { return &value }
func intPtrTest(value int) *int       { return &value }
