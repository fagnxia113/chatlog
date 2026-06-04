package temporalgraph

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	jsonBlockRe     = regexp.MustCompile("(?s)```(?:json)?\\s*(.*?)\\s*```")
	fullDateCNRe    = regexp.MustCompile(`(\d{4})年(\d{1,2})月(\d{1,2})日?`)
	monthDayCNRe    = regexp.MustCompile(`(\d{1,2})月(\d{1,2})日?`)
	isoDateRe       = regexp.MustCompile(`(\d{4})-(\d{1,2})-(\d{1,2})`)
	phoneLikeRe     = regexp.MustCompile(`1[3-9]\d{9}`)
	wxIDLikeRe      = regexp.MustCompile(`(?i)^wxid_[a-z0-9]+$`)
	spaceCollapseRe = regexp.MustCompile(`\s+`)
)

func parseTimeFlexible(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Now()
	}
	layouts := []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02"}
	for _, layout := range layouts {
		if t, err := time.ParseInLocation(layout, raw, time.Local); err == nil {
			return t
		}
	}
	return time.Now()
}

func cleanName(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, " \t\r\n\"'`[]()（）<>《》")
	if utf8.RuneCountInString(s) > 80 {
		s = truncateRunes(s, 80)
	}
	return s
}

func cleanType(s, fallback string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, " ", "_")
	if s == "" {
		return fallback
	}
	if utf8.RuneCountInString(s) > 64 {
		return truncateRunes(s, 64)
	}
	return s
}

func clampConfidence(v float64) float64 {
	if v <= 0 {
		return 0.5
	}
	if v > 1 {
		return 1
	}
	return v
}

func clampInt(v, min, max int) int {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

func factKey(statement string) string {
	sum := sha1.Sum([]byte(strings.ToLower(strings.TrimSpace(statement))))
	return hex.EncodeToString(sum[:])
}

func entityKey(name, typ string) string {
	return cleanType(typ, "unknown") + ":" + strings.ToLower(cleanName(name))
}

func canonicalEntityName(name string) string {
	name = strings.ToLower(cleanName(name))
	name = strings.TrimPrefix(name, "@")
	name = strings.TrimSpace(strings.ReplaceAll(name, "\u2005", ""))
	name = phoneLikeRe.ReplaceAllString(name, "")
	if wxIDLikeRe.MatchString(name) {
		return name
	}
	for _, suffix := range []string{"总", "老师", "同学", "老板", "经理", "先生", "女士"} {
		name = strings.TrimSuffix(name, suffix)
	}
	name = spaceCollapseRe.ReplaceAllString(name, "")
	return cleanName(name)
}

func canonicalPredicate(raw string) string {
	p := cleanType(raw, "related_to")
	p = strings.ReplaceAll(p, "-", "_")
	p = strings.ReplaceAll(p, "/", "_")
	aliases := map[string]string{
		"负责":          "responsible_for",
		"负责处理":        "responsible_for",
		"处理":          "handles",
		"跟进":          "follows_up",
		"对接":          "coordinates_with",
		"确认":          "confirmed",
		"通知":          "notified",
		"要求":          "requires",
		"延期":          "delayed_to",
		"完成":          "completed",
		"属于":          "belongs_to",
		"客户":          "customer_of",
		"供应":          "supplies",
		"采购":          "purchases",
		"销售":          "sells",
		"报价":          "quoted",
		"付款":          "paid",
		"收款":          "received_payment",
		"冲突":          "conflicts_with",
		"related":     "related_to",
		"related_to":  "related_to",
		"responsible": "responsible_for",
		"owner":       "responsible_for",
		"follow_up":   "follows_up",
		"coordinate":  "coordinates_with",
		"confirmed":   "confirmed",
		"notify":      "notified",
		"requires":    "requires",
		"completed":   "completed",
	}
	if v, ok := aliases[p]; ok {
		return v
	}
	for key, value := range aliases {
		if strings.Contains(p, key) {
			return value
		}
	}
	if utf8.RuneCountInString(p) > 32 {
		return "related_to"
	}
	return p
}

func canonicalFactStatement(statement string) string {
	s := strings.ToLower(strings.TrimSpace(statement))
	replacements := map[string]string{
		"负责人是": "负责",
		"由":    "",
		"进行处理": "处理",
		"负责处理": "负责",
	}
	for old, newValue := range replacements {
		s = strings.ReplaceAll(s, old, newValue)
	}
	return strings.Join(strings.Fields(s), "")
}

func relationCanonicalKey(subjID, objID int64, predicate string) string {
	return fmt.Sprintf("%d:%s:%d", subjID, canonicalPredicate(predicate), objID)
}

func resolveRelativeTime(base time.Time, texts ...string) time.Time {
	if base.IsZero() {
		base = time.Now()
	}
	raw := strings.Join(texts, " ")
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return base
	}
	if t, ok := parseExplicitDate(raw, base); ok {
		return t
	}
	day := startOfDay(base)
	switch {
	case strings.Contains(raw, "昨天"):
		return day.AddDate(0, 0, -1)
	case strings.Contains(raw, "今天") || strings.Contains(raw, "今日"):
		return day
	case strings.Contains(raw, "明天") || strings.Contains(raw, "明日"):
		return day.AddDate(0, 0, 1)
	case strings.Contains(raw, "后天"):
		return day.AddDate(0, 0, 2)
	case strings.Contains(raw, "下周"):
		return resolveWeekday(day.AddDate(0, 0, 7), raw)
	case strings.Contains(raw, "本周") || strings.Contains(raw, "这周"):
		return resolveWeekday(day, raw)
	case strings.Contains(raw, "月底") || strings.Contains(raw, "本月底"):
		return time.Date(base.Year(), base.Month()+1, 0, 0, 0, 0, 0, base.Location())
	case strings.Contains(raw, "下月底"):
		return time.Date(base.Year(), base.Month()+2, 0, 0, 0, 0, 0, base.Location())
	}
	return base
}

func parseExplicitDate(raw string, base time.Time) (time.Time, bool) {
	loc := base.Location()
	parseInt := func(s string) int {
		v, _ := strconv.Atoi(s)
		return v
	}
	if m := fullDateCNRe.FindStringSubmatch(raw); len(m) == 4 {
		return time.Date(parseInt(m[1]), time.Month(parseInt(m[2])), parseInt(m[3]), 0, 0, 0, 0, loc), true
	}
	if m := isoDateRe.FindStringSubmatch(raw); len(m) == 4 {
		return time.Date(parseInt(m[1]), time.Month(parseInt(m[2])), parseInt(m[3]), 0, 0, 0, 0, loc), true
	}
	if m := monthDayCNRe.FindStringSubmatch(raw); len(m) == 3 {
		month := time.Month(parseInt(m[1]))
		day := parseInt(m[2])
		year := base.Year()
		t := time.Date(year, month, day, 0, 0, 0, 0, loc)
		if t.Before(base.AddDate(0, -6, 0)) {
			t = t.AddDate(1, 0, 0)
		}
		return t, true
	}
	return time.Time{}, false
}

func startOfDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

func resolveWeekday(base time.Time, raw string) time.Time {
	weekdayMap := map[string]time.Weekday{
		"周一": time.Monday, "星期一": time.Monday,
		"周二": time.Tuesday, "星期二": time.Tuesday,
		"周三": time.Wednesday, "星期三": time.Wednesday,
		"周四": time.Thursday, "星期四": time.Thursday,
		"周五": time.Friday, "星期五": time.Friday,
		"周六": time.Saturday, "星期六": time.Saturday,
		"周日": time.Sunday, "周天": time.Sunday, "星期日": time.Sunday, "星期天": time.Sunday,
	}
	for key, weekday := range weekdayMap {
		if strings.Contains(raw, key) {
			day := startOfDay(base)
			for i := 0; i < 7; i++ {
				if day.Weekday() == weekday {
					return day
				}
				day = day.AddDate(0, 0, 1)
			}
		}
	}
	return startOfDay(base)
}

func firstLine(parts ...string) string {
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		p = strings.ReplaceAll(p, "\r\n", "\n")
		p = strings.Split(p, "\n")[0]
		return truncateRunes(p, 80)
	}
	return "事件"
}

func truncateRunes(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n])
}

func toJSONString(v any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}

func isDatabaseNotReady(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "database not ready")
}

func extractJSONObject(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if m := jsonBlockRe.FindStringSubmatch(raw); len(m) == 2 {
		raw = strings.TrimSpace(m[1])
	}
	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start >= 0 && end > start {
		return raw[start : end+1]
	}
	return raw
}

func decodeExtraction(raw string) (Extraction, error) {
	candidates := jsonObjectCandidates(raw)
	if len(candidates) == 0 {
		candidates = []string{extractJSONObject(raw)}
	}
	var lastErr error
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if ext, ok, err := unmarshalExtraction(candidate); err == nil {
			if ok {
				return ext, nil
			}
			lastErr = fmt.Errorf("json object does not match extraction schema")
		} else {
			lastErr = err
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no json object found")
	}
	return Extraction{}, lastErr
}

func unmarshalExtraction(raw string) (Extraction, bool, error) {
	var wrapped struct {
		Extraction Extraction `json:"extraction"`
	}
	if err := json.Unmarshal([]byte(raw), &wrapped); err == nil && extractionHasContent(wrapped.Extraction) {
		return wrapped.Extraction, true, nil
	}
	var ext Extraction
	if err := json.Unmarshal([]byte(raw), &ext); err != nil {
		return Extraction{}, false, err
	}
	if extractionHasContent(ext) || strings.TrimSpace(raw) == "{}" {
		return ext, true, nil
	}
	return Extraction{}, false, nil
}

func extractionHasContent(ext Extraction) bool {
	return len(ext.Entities) > 0 || len(ext.Relations) > 0 || len(ext.Events) > 0 || len(ext.Facts) > 0
}

func jsonObjectCandidates(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	candidates := []string{}
	if matches := jsonBlockRe.FindAllStringSubmatch(raw, -1); len(matches) > 0 {
		for _, m := range matches {
			if len(m) == 2 {
				candidates = append(candidates, jsonObjectCandidatesFromText(m[1])...)
			}
		}
	}
	candidates = append(candidates, jsonObjectCandidatesFromText(raw)...)
	seen := map[string]struct{}{}
	out := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if _, ok := seen[candidate]; ok {
			continue
		}
		seen[candidate] = struct{}{}
		out = append(out, candidate)
	}
	return out
}

func jsonObjectCandidatesFromText(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	out := []string{}
	inString := false
	escaped := false
	depth := 0
	start := -1
	for i := 0; i < len(raw); i++ {
		ch := raw[i]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			switch ch {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
			continue
		}
		switch ch {
		case '"':
			inString = true
		case '{':
			if depth == 0 {
				start = i
			}
			depth++
		case '}':
			if depth == 0 {
				continue
			}
			depth--
			if depth == 0 && start >= 0 {
				out = append(out, raw[start:i+1])
				start = -1
			}
		}
	}
	return out
}

func entityHintsFromSource(src SourceRecord) []ExtractedEntity {
	seen := map[string]struct{}{}
	out := []ExtractedEntity{}
	add := func(name, typ string, aliases ...string) {
		name = cleanName(name)
		typ = cleanType(typ, "unknown")
		if name == "" {
			return
		}
		key := entityKey(name, typ)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, ExtractedEntity{Name: name, Type: typ, Aliases: aliases, Confidence: 1})
	}
	add(src.TalkerName, "conversation", src.Talker)
	add(src.Talker, "conversation", src.TalkerName)
	add(src.SenderName, "person", src.Sender)
	add(src.Sender, "person", src.SenderName)
	for _, p := range src.Participants {
		add(p.DisplayName, p.Kind, append([]string{p.UserName}, p.Aliases...)...)
		add(p.UserName, p.Kind, append([]string{p.DisplayName}, p.Aliases...)...)
	}
	for _, hint := range src.EntityHints {
		add(hint, "hint")
	}
	if src.Metadata != nil {
		for _, key := range []string{"entities", "actors", "targets"} {
			for _, item := range splitHintList(src.Metadata[key]) {
				add(item, "hint")
			}
		}
	}
	return out
}

func splitHintList(raw string) []string {
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '，' || r == '、' || r == ';' || r == '；' || r == '|' || r == '\n' || r == '\t'
	})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = cleanName(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func sourcePromptPayload(rec SourceRecord) map[string]any {
	return map[string]any{
		"source_type":  rec.SourceType,
		"event_type":   rec.EventType,
		"time":         rec.EventTime.Format(time.RFC3339),
		"talker":       rec.TalkerName,
		"talker_id":    rec.Talker,
		"sender":       rec.SenderName,
		"sender_id":    rec.Sender,
		"title":        rec.Title,
		"content":      truncateRunes(rec.Content, 4000),
		"context":      rec.Context,
		"participants": rec.Participants,
		"entity_hints": rec.EntityHints,
		"metadata":     rec.Metadata,
	}
}
