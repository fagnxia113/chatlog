package conf

import "strings"

const (
	HookNotifyMCP    = "mcp"
	HookNotifyPost   = "post"
	HookNotifyBoth   = "both"
	HookNotifyWeixin = "weixin"
	HookNotifyQQ     = "qq"
	HookNotifyAll    = "all"
)

type MessageHook struct {
	Keywords         string `mapstructure:"keywords" json:"keywords"`
	NotifyMode       string `mapstructure:"notify_mode" json:"notify_mode"`
	PostURL          string `mapstructure:"post_url" json:"post_url"`
	BeforeCount      int    `mapstructure:"before_count" json:"before_count"`
	AfterCount       int    `mapstructure:"after_count" json:"after_count"`
	ForwardAll       bool   `mapstructure:"forward_all" json:"forward_all"`
	ForwardContacts  string `mapstructure:"forward_contacts" json:"forward_contacts"`
	ForwardChatRooms string `mapstructure:"forward_chatrooms" json:"forward_chatrooms"`
}

type HookNotifyTargets struct {
	MCP    bool
	Post   bool
	Weixin bool
	QQ     bool
}

func (t HookNotifyTargets) HasAny() bool {
	return t.MCP || t.Post || t.Weixin || t.QQ
}

func ParseHookNotifyTargets(raw string) (HookNotifyTargets, bool) {
	raw = strings.TrimSpace(strings.ToLower(raw))
	if raw == "" {
		return HookNotifyTargets{MCP: true}, true
	}

	var targets HookNotifyTargets
	for _, token := range splitHookNotifyMode(raw) {
		switch token {
		case HookNotifyMCP:
			targets.MCP = true
		case HookNotifyPost:
			targets.Post = true
		case HookNotifyWeixin:
			targets.Weixin = true
		case HookNotifyQQ:
			targets.QQ = true
		case HookNotifyBoth:
			targets.MCP = true
			targets.Post = true
		case HookNotifyAll:
			targets.MCP = true
			targets.Post = true
			targets.Weixin = true
			targets.QQ = true
		default:
			return HookNotifyTargets{}, false
		}
	}
	if !targets.HasAny() {
		return HookNotifyTargets{}, false
	}
	return targets, true
}

func CanonicalHookNotifyMode(raw string) string {
	targets, ok := ParseHookNotifyTargets(raw)
	if !ok {
		return HookNotifyMCP
	}
	if targets.MCP && targets.Post && targets.Weixin && targets.QQ {
		return HookNotifyAll
	}
	if targets.MCP && targets.Post && !targets.Weixin && !targets.QQ {
		return HookNotifyBoth
	}
	parts := make([]string, 0, 4)
	if targets.MCP {
		parts = append(parts, HookNotifyMCP)
	}
	if targets.Post {
		parts = append(parts, HookNotifyPost)
	}
	if targets.Weixin {
		parts = append(parts, HookNotifyWeixin)
	}
	if targets.QQ {
		parts = append(parts, HookNotifyQQ)
	}
	if len(parts) == 0 {
		return HookNotifyMCP
	}
	return strings.Join(parts, ",")
}

func splitHookNotifyMode(raw string) []string {
	raw = strings.NewReplacer("|", ",", ";", ",", "+", ",", " ", ",").Replace(raw)
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		out = append(out, part)
	}
	return out
}
