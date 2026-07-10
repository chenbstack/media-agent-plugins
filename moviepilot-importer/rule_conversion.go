package main

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/chenbstack/media-agent-plugin-sdk-go"
)

type nativeRuleMapping struct {
	Dimension string
	Value     string
}

var moviePilotNativeRules = map[string]nativeRuleMapping{
	"BLU":     {Dimension: "quality", Value: "蓝光原盘"},
	"4K":      {Dimension: "resolution", Value: "4K"},
	"1080P":   {Dimension: "resolution", Value: "1080P"},
	"720P":    {Dimension: "resolution", Value: "720P"},
	"CNSUB":   {Dimension: "subtitle", Value: "中文字幕"},
	"GZ":      {Dimension: "source", Value: "官种"},
	"SPECSUB": {Dimension: "subtitle", Value: "特效字幕"},
	"BLURAY":  {Dimension: "quality", Value: "BLURAY"},
	"UHD":     {Dimension: "quality", Value: "UHD"},
	"H265":    {Dimension: "codec", Value: "H265"},
	"H264":    {Dimension: "codec", Value: "H264"},
	"DOLBY":   {Dimension: "visualEffect", Value: "杜比视界"},
	"ATMOS":   {Dimension: "audioSpec", Value: "杜比全景声"},
	"HDR":     {Dimension: "visualEffect", Value: "HDR"},
	"SDR":     {Dimension: "visualEffect", Value: "SDR"},
	"REMUX":   {Dimension: "quality", Value: "REMUX"},
	"WEBDL":   {Dimension: "quality", Value: "WEB-DL"},
	"FREE":    {Dimension: "promotion", Value: "免费"},
	"CNVOI":   {Dimension: "dubbing", Value: "国语"},
	"HKVOI":   {Dimension: "dubbing", Value: "粤语"},
	"60FPS":   {Dimension: "visualEffect", Value: "60fps"},
	"3D":      {Dimension: "visualEffect", Value: "3D"},
}

var moviePilotSortMappings = map[string]string{
	"torrent": "资源优先级",
	"site":    "站点优先级",
	"upload":  "站点上传量",
	"seeder":  "资源做种数",
}

type ruleConversion struct {
	Write    pluginsdk.RuleProfileWrite
	Warnings []string
}

type ruleProfileParts struct {
	preferences   []pluginsdk.RuleDimension
	prerequisites pluginsdk.RulePrerequisites
	warnings      []string
	mapped        bool
	unsafe        bool
}

type ruleLiteral struct {
	Name    string
	Negated bool
}

type ruleNode struct {
	kind        byte
	name        string
	left, right *ruleNode
}

type expressionParser struct {
	input string
	pos   int
}

type termConstraint struct {
	includePatterns []string
	excludePatterns []string
	negativeValues  []string
	positiveValues  map[string][]string
	minGB, maxGB    *float64
	minSeeders      *int
	minAge, maxAge  *int
	valid           bool
}

func convertMoviePilotRuleBundle(bundle moviePilotRuleBundle, catalog pluginsdk.RuleCatalog) (ruleConversion, error) {
	if bundle.Kind != "profile" || strings.TrimSpace(bundle.ProfileKey) == "" || len(bundle.Groups) == 0 {
		return ruleConversion{}, fmt.Errorf("MoviePilot 规则集数据不完整")
	}
	customRules := map[string]moviePilotCustomRule{}
	for _, rule := range bundle.CustomRules {
		id := strings.TrimSpace(rule.ID)
		if id != "" {
			customRules[id] = rule // MoviePilot user rules override native rules with the same id.
		}
	}
	merged := ruleProfileParts{}
	for _, group := range bundle.Groups {
		parts := convertMoviePilotRuleGroup(group, customRules)
		mergeRuleProfileParts(&merged, parts)
	}
	mediaType, mediaCategory := commonRuleScope(bundle.Groups)
	warnings := append([]string(nil), merged.warnings...)
	write := pluginsdk.RuleProfileWrite{
		IdempotencyKey: bundle.ProfileKey,
		Name:           firstNonEmpty(bundle.Name, bundle.Groups[0].Name),
		Status:         "enabled",
		Priority:       bundle.Priority,
		MediaType:      mediaType,
		MediaCategory:  mediaCategory,
		MatchConditions: []string{
			"MoviePilot 规则组：" + joinedRuleGroupNames(bundle.Groups),
			"原始规则：" + joinedRuleStrings(bundle.Groups),
		},
		Prerequisites: merged.prerequisites,
		Preferences:   merged.preferences,
		Fallback:      "MoviePilot 的 > 优先级已折叠为宿主维度内的偏好顺序。",
	}
	filterRuleWriteByCatalog(&write, catalog, &warnings)
	if merged.unsafe || !merged.mapped || len(write.Preferences) == 0 && rulePrerequisitesEmpty(write.Prerequisites) {
		write.Status = "disabled"
		warnings = append(warnings, "没有可安全映射的有效条件，已停用以避免规则意外匹配全部资源")
	}
	warnings = uniqueStrings(warnings)
	write.Description = "从 MoviePilot 迁移。"
	if len(warnings) > 0 {
		write.Description += " 迁移提示：" + strings.Join(warnings, "；") + "。"
	}
	return ruleConversion{Write: write, Warnings: warnings}, nil
}

func convertMoviePilotRuleGroup(group moviePilotRuleGroup, customRules map[string]moviePilotCustomRule) ruleProfileParts {
	parts := ruleProfileParts{}
	levels, err := splitRuleLevels(group.RuleString)
	if err != nil || len(levels) == 0 {
		parts.warnings = append(parts.warnings, fmt.Sprintf("规则组 %s 的表达式无效：%v", group.Name, err))
		return parts
	}
	var allTerms [][]ruleLiteral
	preferenceSeen := map[string]map[string]bool{}
	for _, level := range levels {
		parser := expressionParser{input: level}
		node, parseErr := parser.parse()
		if parseErr != nil {
			parts.warnings = append(parts.warnings, fmt.Sprintf("规则组 %s 的优先级表达式 %q 无法解析：%v", group.Name, level, parseErr))
			continue
		}
		for _, literal := range orderedLiterals(node, false) {
			if literal.Negated {
				continue
			}
			if _, overridden := customRules[literal.Name]; overridden {
				continue
			}
			mapping, ok := moviePilotNativeRules[strings.ToUpper(literal.Name)]
			if !ok {
				continue
			}
			appendPreference(&parts.preferences, preferenceSeen, mapping.Dimension, mapping.Value)
			parts.mapped = true
		}
		terms, dnfErr := expressionDNF(node, false, 64)
		if dnfErr != nil {
			parts.warnings = append(parts.warnings, fmt.Sprintf("规则组 %s 的表达式过于复杂：%v", group.Name, dnfErr))
			continue
		}
		allTerms = append(allTerms, terms...)
	}
	if len(levels) > 1 {
		parts.warnings = append(parts.warnings, "多级优先级已按各维度首次出现顺序折叠")
	}
	constraints := make([]termConstraint, 0, len(allTerms))
	for _, term := range allTerms {
		constraint := termConstraint{valid: true, positiveValues: map[string][]string{}}
		for _, literal := range term {
			if custom, ok := customRules[literal.Name]; ok {
				applyCustomRuleLiteral(&constraint, custom, literal.Negated, &parts.warnings)
				continue
			}
			mapping, ok := moviePilotNativeRules[strings.ToUpper(literal.Name)]
			if !ok {
				parts.warnings = append(parts.warnings, "未识别规则 "+literal.Name)
				continue
			}
			parts.mapped = true
			if literal.Negated {
				constraint.negativeValues = append(constraint.negativeValues, mapping.Value)
			} else {
				constraint.positiveValues[mapping.Dimension] = appendUnique(constraint.positiveValues[mapping.Dimension], mapping.Value)
			}
		}
		if constraint.valid {
			constraints = append(constraints, constraint)
		}
	}
	mergeTermConstraints(&parts, constraints)
	return parts
}

func splitRuleLevels(value string) ([]string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, fmt.Errorf("规则串为空")
	}
	depth := 0
	start := 0
	levels := []string{}
	for index, char := range value {
		switch char {
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return nil, fmt.Errorf("括号不匹配")
			}
		case '>':
			if depth == 0 {
				level := strings.TrimSpace(value[start:index])
				if level == "" {
					return nil, fmt.Errorf("优先级层级为空")
				}
				levels = append(levels, level)
				start = index + 1
			}
		}
	}
	if depth != 0 {
		return nil, fmt.Errorf("括号不匹配")
	}
	last := strings.TrimSpace(value[start:])
	if last == "" {
		return nil, fmt.Errorf("优先级层级为空")
	}
	return append(levels, last), nil
}

func (p *expressionParser) parse() (*ruleNode, error) {
	node, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	p.skipSpace()
	if p.pos != len(p.input) {
		return nil, fmt.Errorf("位置 %d 存在无法识别的字符 %q", p.pos+1, p.input[p.pos:])
	}
	return node, nil
}

func (p *expressionParser) parseOr() (*ruleNode, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.consume('|') {
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = &ruleNode{kind: '|', left: left, right: right}
	}
	return left, nil
}

func (p *expressionParser) parseAnd() (*ruleNode, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for p.consume('&') {
		right, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		left = &ruleNode{kind: '&', left: left, right: right}
	}
	return left, nil
}

func (p *expressionParser) parseUnary() (*ruleNode, error) {
	if p.consume('!') {
		child, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &ruleNode{kind: '!', left: child}, nil
	}
	return p.parsePrimary()
}

func (p *expressionParser) parsePrimary() (*ruleNode, error) {
	if p.consume('(') {
		node, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		if !p.consume(')') {
			return nil, fmt.Errorf("缺少右括号")
		}
		return node, nil
	}
	p.skipSpace()
	start := p.pos
	for p.pos < len(p.input) {
		char := p.input[p.pos]
		if char >= 'A' && char <= 'Z' || char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '_' {
			p.pos++
			continue
		}
		break
	}
	if start == p.pos {
		return nil, fmt.Errorf("位置 %d 缺少规则名", p.pos+1)
	}
	return &ruleNode{kind: 'n', name: p.input[start:p.pos]}, nil
}

func (p *expressionParser) consume(expected byte) bool {
	p.skipSpace()
	if p.pos < len(p.input) && p.input[p.pos] == expected {
		p.pos++
		return true
	}
	return false
}

func (p *expressionParser) skipSpace() {
	for p.pos < len(p.input) && (p.input[p.pos] == ' ' || p.input[p.pos] == '\t' || p.input[p.pos] == '\r' || p.input[p.pos] == '\n') {
		p.pos++
	}
}

func orderedLiterals(node *ruleNode, negated bool) []ruleLiteral {
	if node == nil {
		return nil
	}
	switch node.kind {
	case 'n':
		return []ruleLiteral{{Name: node.name, Negated: negated}}
	case '!':
		return orderedLiterals(node.left, !negated)
	default:
		return append(orderedLiterals(node.left, negated), orderedLiterals(node.right, negated)...)
	}
}

func expressionDNF(node *ruleNode, negated bool, limit int) ([][]ruleLiteral, error) {
	if node == nil {
		return nil, fmt.Errorf("空表达式")
	}
	switch node.kind {
	case 'n':
		return [][]ruleLiteral{{{Name: node.name, Negated: negated}}}, nil
	case '!':
		return expressionDNF(node.left, !negated, limit)
	case '&', '|':
		operator := node.kind
		if negated {
			if operator == '&' {
				operator = '|'
			} else {
				operator = '&'
			}
		}
		left, err := expressionDNF(node.left, negated, limit)
		if err != nil {
			return nil, err
		}
		right, err := expressionDNF(node.right, negated, limit)
		if err != nil {
			return nil, err
		}
		if operator == '|' {
			if len(left)+len(right) > limit {
				return nil, fmt.Errorf("展开后超过 %d 个分支", limit)
			}
			return append(left, right...), nil
		}
		if len(left)*len(right) > limit {
			return nil, fmt.Errorf("展开后超过 %d 个分支", limit)
		}
		out := make([][]ruleLiteral, 0, len(left)*len(right))
		for _, leftTerm := range left {
			for _, rightTerm := range right {
				term := append(append([]ruleLiteral(nil), leftTerm...), rightTerm...)
				out = append(out, term)
			}
		}
		return out, nil
	default:
		return nil, fmt.Errorf("未知表达式节点")
	}
}

func applyCustomRuleLiteral(target *termConstraint, rule moviePilotCustomRule, negated bool, warnings *[]string) {
	include := validPatterns(rule.Include, rule.ID+" 包含", warnings)
	exclude := validPatterns(rule.Exclude, rule.ID+" 排除", warnings)
	minGB, maxGB, hasSize := parseMoviePilotSize(rule.SizeRange, warnings, rule.ID)
	minSeeders, hasSeeders := parsePositiveInt(rule.Seeders, warnings, rule.ID+" 最少做种数")
	minAge, maxAge, hasAge := parseMoviePilotAge(rule.PublishTime, warnings, rule.ID)
	if negated {
		hasNumeric := hasSize || hasSeeders || hasAge
		switch {
		case !hasNumeric && len(include) > 0 && len(exclude) == 0:
			target.excludePatterns = append(target.excludePatterns, include...)
		case !hasNumeric && len(include) == 0 && len(exclude) > 0:
			target.includePatterns = append(target.includePatterns, exclude...)
		default:
			*warnings = append(*warnings, "自定义规则 "+rule.ID+" 的取反包含复合条件，宿主无法无损表达，已跳过该取反条件")
		}
		return
	}
	target.includePatterns = append(target.includePatterns, include...)
	target.excludePatterns = append(target.excludePatterns, exclude...)
	if hasSize {
		target.minGB = maxFloatPtr(target.minGB, minGB)
		target.maxGB = minFloatPtr(target.maxGB, maxGB)
	}
	if hasSeeders {
		target.minSeeders = maxIntPtr(target.minSeeders, minSeeders)
	}
	if hasAge {
		target.minAge = maxIntPtr(target.minAge, minAge)
		target.maxAge = minIntPtr(target.maxAge, maxAge)
	}
	if target.minGB != nil && target.maxGB != nil && *target.minGB > *target.maxGB ||
		target.minAge != nil && target.maxAge != nil && *target.minAge > *target.maxAge {
		target.valid = false
		*warnings = append(*warnings, "自定义规则 "+rule.ID+" 的范围条件互相冲突，该表达式分支已跳过")
	}
}

func mergeTermConstraints(parts *ruleProfileParts, terms []termConstraint) {
	if len(terms) == 0 {
		return
	}
	for _, term := range terms {
		if len(term.includePatterns) == 0 {
			parts.prerequisites.IncludeKeywordPattern = ""
			break
		}
		pattern, truncated := regexAnd(term.includePatterns, 4)
		if truncated {
			parts.warnings = append(parts.warnings, "单个表达式分支超过 4 个包含正则，只保留前 4 个条件")
		}
		if parts.prerequisites.IncludeKeywordPattern == "" {
			parts.prerequisites.IncludeKeywordPattern = pattern
		} else {
			parts.prerequisites.IncludeKeywordPattern += "|" + pattern
		}
	}
	if parts.prerequisites.IncludeKeywordPattern != "" {
		parts.prerequisites.IncludeKeywordPattern = "(?:" + parts.prerequisites.IncludeKeywordPattern + ")"
		parts.prerequisites.IncludePatternAdvanced = true
		parts.mapped = true
	}
	commonExcludes := intersectTermStrings(terms, func(term termConstraint) []string { return term.excludePatterns })
	if len(commonExcludes) > 0 {
		parts.prerequisites.ExcludeKeywordPattern = regexOr(commonExcludes)
		parts.prerequisites.ExcludePatternAdvanced = true
		parts.mapped = true
	}
	commonNegatives := intersectTermStrings(terms, func(term termConstraint) []string { return term.negativeValues })
	if len(commonNegatives) > 0 {
		selected := make([]string, 0, len(commonNegatives))
		for _, value := range commonNegatives {
			selected = append(selected, "排除: "+value)
		}
		parts.preferences = append(parts.preferences, pluginsdk.RuleDimension{ID: "negativeFilter", Selected: selected})
		parts.mapped = true
	}
	parts.prerequisites.Dimensions = commonPositiveDimensions(terms)
	if len(parts.prerequisites.Dimensions) > 0 {
		parts.mapped = true
	}
	parts.prerequisites.Size.MinGB = commonMinFloat(terms, func(term termConstraint) *float64 { return term.minGB })
	parts.prerequisites.Size.MaxGB = commonMaxFloat(terms, func(term termConstraint) *float64 { return term.maxGB })
	parts.prerequisites.MinSeeders = commonMinInt(terms, func(term termConstraint) *int { return term.minSeeders })
	parts.prerequisites.MinAgeMinutes = commonMinInt(terms, func(term termConstraint) *int { return term.minAge })
	parts.prerequisites.MaxAgeMinutes = commonMaxInt(terms, func(term termConstraint) *int { return term.maxAge })
	if parts.prerequisites.Size.MinGB != nil || parts.prerequisites.Size.MaxGB != nil ||
		parts.prerequisites.MinSeeders != nil || parts.prerequisites.MinAgeMinutes != nil || parts.prerequisites.MaxAgeMinutes != nil {
		parts.mapped = true
	}
}

func mergeRuleProfileParts(target *ruleProfileParts, source ruleProfileParts) {
	seen := map[string]map[string]bool{}
	for _, dimension := range target.preferences {
		for _, value := range dimension.Selected {
			if seen[dimension.ID] == nil {
				seen[dimension.ID] = map[string]bool{}
			}
			seen[dimension.ID][value] = true
		}
	}
	for _, dimension := range source.preferences {
		for _, value := range dimension.Selected {
			appendPreference(&target.preferences, seen, dimension.ID, value)
		}
	}
	for _, sourceDimension := range source.prerequisites.Dimensions {
		merged := false
		for index := range target.prerequisites.Dimensions {
			if target.prerequisites.Dimensions[index].ID != sourceDimension.ID {
				continue
			}
			intersection := intersectStrings(target.prerequisites.Dimensions[index].Selected, sourceDimension.Selected)
			if len(intersection) == 0 {
				target.warnings = append(target.warnings, "组合规则在维度 "+sourceDimension.ID+" 上没有共同可选值")
				target.unsafe = true
			} else {
				target.prerequisites.Dimensions[index].Selected = intersection
			}
			merged = true
			break
		}
		if !merged {
			target.prerequisites.Dimensions = append(target.prerequisites.Dimensions, sourceDimension)
		}
	}
	target.prerequisites.Size.MinGB = maxFloatPtr(target.prerequisites.Size.MinGB, source.prerequisites.Size.MinGB)
	target.prerequisites.Size.MaxGB = minFloatPtr(target.prerequisites.Size.MaxGB, source.prerequisites.Size.MaxGB)
	target.prerequisites.MinSeeders = maxIntPtr(target.prerequisites.MinSeeders, source.prerequisites.MinSeeders)
	target.prerequisites.MinAgeMinutes = maxIntPtr(target.prerequisites.MinAgeMinutes, source.prerequisites.MinAgeMinutes)
	target.prerequisites.MaxAgeMinutes = minIntPtr(target.prerequisites.MaxAgeMinutes, source.prerequisites.MaxAgeMinutes)
	if source.prerequisites.IncludeKeywordPattern != "" {
		if target.prerequisites.IncludeKeywordPattern == "" {
			target.prerequisites.IncludeKeywordPattern = source.prerequisites.IncludeKeywordPattern
		} else {
			combined, _ := regexAnd([]string{target.prerequisites.IncludeKeywordPattern, source.prerequisites.IncludeKeywordPattern}, 4)
			target.prerequisites.IncludeKeywordPattern = combined
		}
		target.prerequisites.IncludePatternAdvanced = true
	}
	if source.prerequisites.ExcludeKeywordPattern != "" {
		if target.prerequisites.ExcludeKeywordPattern == "" {
			target.prerequisites.ExcludeKeywordPattern = source.prerequisites.ExcludeKeywordPattern
		} else {
			target.prerequisites.ExcludeKeywordPattern = regexOr([]string{target.prerequisites.ExcludeKeywordPattern, source.prerequisites.ExcludeKeywordPattern})
		}
		target.prerequisites.ExcludePatternAdvanced = true
	}
	target.warnings = append(target.warnings, source.warnings...)
	target.mapped = target.mapped || source.mapped
	target.unsafe = target.unsafe || source.unsafe
}

func appendPreference(dimensions *[]pluginsdk.RuleDimension, seen map[string]map[string]bool, id, value string) {
	if seen[id] == nil {
		seen[id] = map[string]bool{}
	}
	if seen[id][value] {
		return
	}
	seen[id][value] = true
	for index := range *dimensions {
		if (*dimensions)[index].ID == id {
			(*dimensions)[index].Selected = append((*dimensions)[index].Selected, value)
			return
		}
	}
	*dimensions = append(*dimensions, pluginsdk.RuleDimension{ID: id, Selected: []string{value}})
}

func commonPositiveDimensions(terms []termConstraint) []pluginsdk.RuleDimension {
	if len(terms) == 0 {
		return nil
	}
	ids := make([]string, 0, len(terms[0].positiveValues))
	for id := range terms[0].positiveValues {
		common := true
		for _, term := range terms[1:] {
			if len(term.positiveValues[id]) == 0 {
				common = false
				break
			}
		}
		if common {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	out := make([]pluginsdk.RuleDimension, 0, len(ids))
	for _, id := range ids {
		selected := []string{}
		for _, term := range terms {
			for _, value := range term.positiveValues[id] {
				selected = appendUnique(selected, value)
			}
		}
		out = append(out, pluginsdk.RuleDimension{ID: id, Selected: selected})
	}
	return out
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func intersectStrings(left, right []string) []string {
	set := map[string]bool{}
	for _, value := range right {
		set[value] = true
	}
	out := make([]string, 0, len(left))
	for _, value := range left {
		if set[value] {
			out = append(out, value)
		}
	}
	return uniqueStrings(out)
}

func validPatterns(value any, label string, warnings *[]string) []string {
	patterns := patternStrings(value)
	out := make([]string, 0, len(patterns))
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		if _, err := regexp.Compile("(?i)(?:" + pattern + ")"); err != nil {
			*warnings = append(*warnings, fmt.Sprintf("%s正则 %q 不兼容宿主 RE2，已跳过", label, pattern))
			continue
		}
		out = append(out, pattern)
	}
	return uniqueStrings(out)
}

func patternStrings(value any) []string {
	switch typed := value.(type) {
	case string:
		if strings.TrimSpace(typed) == "" {
			return nil
		}
		return []string{strings.TrimSpace(typed)}
	case []string:
		return uniqueStrings(typed)
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			out = append(out, strings.TrimSpace(fmt.Sprint(item)))
		}
		return uniqueStrings(out)
	default:
		return nil
	}
}

func parseMoviePilotSize(value any, warnings *[]string, id string) (*float64, *float64, bool) {
	text := strings.TrimSpace(fmt.Sprint(value))
	if text == "" || text == "<nil>" {
		return nil, nil, false
	}
	parse := func(raw string) (*float64, bool) {
		number, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		if err != nil || number < 0 {
			return nil, false
		}
		gb := number / 1024
		return &gb, true
	}
	if strings.HasPrefix(text, ">") {
		minimum, ok := parse(text[1:])
		if ok {
			return minimum, nil, true
		}
	} else if strings.HasPrefix(text, "<") {
		maximum, ok := parse(text[1:])
		if ok {
			return nil, maximum, true
		}
	} else if parts := strings.Split(text, "-"); len(parts) == 2 {
		minimum, minOK := parse(parts[0])
		maximum, maxOK := parse(parts[1])
		if minOK && maxOK && *minimum <= *maximum {
			return minimum, maximum, true
		}
	}
	*warnings = append(*warnings, "自定义规则 "+id+" 的大小范围 "+strconv.Quote(text)+" 无效，已跳过")
	return nil, nil, false
}

func parseMoviePilotAge(value any, warnings *[]string, id string) (*int, *int, bool) {
	text := strings.TrimSpace(fmt.Sprint(value))
	if text == "" || text == "<nil>" {
		return nil, nil, false
	}
	parse := func(raw string) (*int, bool) {
		number, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		if err != nil || number < 0 || number > math.MaxInt {
			return nil, false
		}
		value := int(math.Round(number))
		return &value, true
	}
	if parts := strings.Split(text, "-"); len(parts) == 2 {
		minimum, minOK := parse(parts[0])
		maximum, maxOK := parse(parts[1])
		if minOK && maxOK && *minimum <= *maximum {
			return minimum, maximum, true
		}
	} else if minimum, ok := parse(text); ok {
		return minimum, nil, true
	}
	*warnings = append(*warnings, "自定义规则 "+id+" 的发布时间 "+strconv.Quote(text)+" 无效，已跳过")
	return nil, nil, false
}

func parsePositiveInt(value any, warnings *[]string, label string) (*int, bool) {
	text := strings.TrimSpace(fmt.Sprint(value))
	if text == "" || text == "<nil>" || text == "0" {
		return nil, false
	}
	parsed, err := strconv.Atoi(text)
	if err != nil || parsed < 0 {
		*warnings = append(*warnings, label+" "+strconv.Quote(text)+" 无效，已跳过")
		return nil, false
	}
	return &parsed, true
}

func regexOr(patterns []string) string {
	patterns = uniqueStrings(patterns)
	if len(patterns) == 0 {
		return ""
	}
	parts := make([]string, 0, len(patterns))
	for _, pattern := range patterns {
		parts = append(parts, "(?:"+pattern+")")
	}
	return "(?:" + strings.Join(parts, "|") + ")"
}

func regexAnd(patterns []string, limit int) (string, bool) {
	patterns = uniqueStrings(patterns)
	truncated := false
	if len(patterns) > limit {
		patterns = patterns[:limit]
		truncated = true
	}
	if len(patterns) <= 1 {
		if len(patterns) == 0 {
			return "", truncated
		}
		return "(?:" + patterns[0] + ")", truncated
	}
	permutations := permuteStrings(patterns)
	parts := make([]string, 0, len(permutations))
	for _, permutation := range permutations {
		wrapped := make([]string, 0, len(permutation))
		for _, pattern := range permutation {
			wrapped = append(wrapped, "(?:"+pattern+")")
		}
		parts = append(parts, strings.Join(wrapped, ".*?"))
	}
	return "(?:" + strings.Join(parts, "|") + ")", truncated
}

func permuteStrings(values []string) [][]string {
	if len(values) == 1 {
		return [][]string{{values[0]}}
	}
	out := [][]string{}
	for index, value := range values {
		rest := append(append([]string(nil), values[:index]...), values[index+1:]...)
		for _, permutation := range permuteStrings(rest) {
			out = append(out, append([]string{value}, permutation...))
		}
	}
	return out
}

func intersectTermStrings(terms []termConstraint, pick func(termConstraint) []string) []string {
	if len(terms) == 0 {
		return nil
	}
	common := uniqueStrings(pick(terms[0]))
	for _, term := range terms[1:] {
		set := map[string]bool{}
		for _, value := range pick(term) {
			set[value] = true
		}
		filtered := common[:0]
		for _, value := range common {
			if set[value] {
				filtered = append(filtered, value)
			}
		}
		common = filtered
	}
	return common
}

func commonMinInt(terms []termConstraint, pick func(termConstraint) *int) *int {
	var out *int
	for _, term := range terms {
		value := pick(term)
		if value == nil {
			return nil
		}
		if out == nil || *value < *out {
			copy := *value
			out = &copy
		}
	}
	return out
}

func commonMaxInt(terms []termConstraint, pick func(termConstraint) *int) *int {
	var out *int
	for _, term := range terms {
		value := pick(term)
		if value == nil {
			return nil
		}
		if out == nil || *value > *out {
			copy := *value
			out = &copy
		}
	}
	return out
}

func commonMinFloat(terms []termConstraint, pick func(termConstraint) *float64) *float64 {
	var out *float64
	for _, term := range terms {
		value := pick(term)
		if value == nil {
			return nil
		}
		if out == nil || *value < *out {
			copy := *value
			out = &copy
		}
	}
	return out
}

func commonMaxFloat(terms []termConstraint, pick func(termConstraint) *float64) *float64 {
	var out *float64
	for _, term := range terms {
		value := pick(term)
		if value == nil {
			return nil
		}
		if out == nil || *value > *out {
			copy := *value
			out = &copy
		}
	}
	return out
}

func maxIntPtr(left, right *int) *int {
	if left == nil {
		return right
	}
	if right == nil || *left >= *right {
		return left
	}
	return right
}

func minIntPtr(left, right *int) *int {
	if left == nil {
		return right
	}
	if right == nil || *left <= *right {
		return left
	}
	return right
}

func maxFloatPtr(left, right *float64) *float64 {
	if left == nil {
		return right
	}
	if right == nil || *left >= *right {
		return left
	}
	return right
}

func minFloatPtr(left, right *float64) *float64 {
	if left == nil {
		return right
	}
	if right == nil || *left <= *right {
		return left
	}
	return right
}

func commonRuleScope(groups []moviePilotRuleGroup) (string, string) {
	mediaType := ""
	category := ""
	for _, group := range groups {
		currentType := normalizeMoviePilotRuleMediaType(group.MediaType)
		currentCategory := firstNonEmpty(group.Category, "全部")
		if mediaType == "" {
			mediaType = currentType
		} else if mediaType != currentType {
			mediaType = "all"
		}
		if category == "" {
			category = currentCategory
		} else if category != currentCategory {
			category = "全部"
		}
	}
	return firstNonEmpty(mediaType, "all"), firstNonEmpty(category, "全部")
}

func normalizeMoviePilotRuleMediaType(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "电影", "movie":
		return "movie"
	case "电视剧", "剧集", "tv", "series":
		return "series"
	default:
		return "all"
	}
}

func filterRuleWriteByCatalog(write *pluginsdk.RuleProfileWrite, catalog pluginsdk.RuleCatalog, warnings *[]string) {
	allowed := map[string]map[string]bool{}
	for _, dimension := range catalog.Dimensions {
		options := map[string]bool{}
		for _, option := range dimension.Options {
			options[option] = true
		}
		allowed[dimension.ID] = options
	}
	if len(allowed) == 0 {
		return
	}
	write.Preferences = filterRuleDimensions(write.Preferences, allowed, warnings)
	write.Prerequisites.Dimensions = filterRuleDimensions(write.Prerequisites.Dimensions, allowed, warnings)
}

func filterRuleDimensions(input []pluginsdk.RuleDimension, allowed map[string]map[string]bool, warnings *[]string) []pluginsdk.RuleDimension {
	filtered := make([]pluginsdk.RuleDimension, 0, len(input))
	for _, dimension := range input {
		options, exists := allowed[dimension.ID]
		if !exists {
			*warnings = append(*warnings, "宿主不支持规则维度 "+dimension.ID)
			continue
		}
		selected := make([]string, 0, len(dimension.Selected))
		for _, value := range dimension.Selected {
			if options[value] {
				selected = append(selected, value)
			} else {
				*warnings = append(*warnings, fmt.Sprintf("宿主维度 %s 不支持取值 %s", dimension.ID, value))
			}
		}
		if len(selected) > 0 {
			filtered = append(filtered, pluginsdk.RuleDimension{ID: dimension.ID, Selected: selected})
		}
	}
	return filtered
}

func mapMoviePilotSort(input []string, catalog pluginsdk.RuleCatalog) ([]string, []string) {
	allowed := map[string]bool{}
	for _, option := range catalog.SortOptions {
		allowed[option] = true
	}
	selected := []string{}
	warnings := []string{}
	for _, source := range input {
		mapped := moviePilotSortMappings[strings.ToLower(strings.TrimSpace(source))]
		if mapped == "" {
			warnings = append(warnings, "未识别排序项 "+source)
			continue
		}
		if len(allowed) > 0 && !allowed[mapped] {
			warnings = append(warnings, "宿主不支持排序项 "+mapped)
			continue
		}
		selected = append(selected, mapped)
	}
	return uniqueStrings(selected), uniqueStrings(warnings)
}

func joinedRuleGroupNames(groups []moviePilotRuleGroup) string {
	names := make([]string, 0, len(groups))
	for _, group := range groups {
		names = append(names, group.Name)
	}
	return strings.Join(names, " + ")
}

func joinedRuleStrings(groups []moviePilotRuleGroup) string {
	values := make([]string, 0, len(groups))
	for _, group := range groups {
		values = append(values, group.RuleString)
	}
	return strings.Join(values, "；")
}

func rulePrerequisitesEmpty(value pluginsdk.RulePrerequisites) bool {
	return len(value.Dimensions) == 0 && value.Size.MinGB == nil && value.Size.MaxGB == nil &&
		value.MinSeeders == nil && value.MinAgeMinutes == nil && value.MaxAgeMinutes == nil &&
		len(value.IncludeKeywords) == 0 && len(value.ExcludeKeywords) == 0 &&
		strings.TrimSpace(value.IncludeKeywordPattern) == "" && strings.TrimSpace(value.ExcludeKeywordPattern) == ""
}

func uniqueStrings(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}
