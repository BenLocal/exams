package notify

import (
	"fmt"
	"strings"
	"time"
)

// maxEventsPerMessage caps how many announcements one message lists.
//
// A source's first crawl can surface hundreds of items at once. Chat platforms
// rate limit aggressively and nobody reads a 300-line message, so the tail is
// summarised rather than dropped silently.
const maxEventsPerMessage = 20

func actionLabel(action string) string {
	if action == "updated" {
		return "更新"
	}
	return "新增"
}

func formatTimePtr(t *time.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return t.Format("2006-01-02")
}

// renderPlainText builds the email body.
func renderPlainText(events []Event) string {
	shown := events
	overflow := 0
	if len(shown) > maxEventsPerMessage {
		overflow = len(shown) - maxEventsPerMessage
		shown = shown[:maxEventsPerMessage]
	}

	var b strings.Builder
	fmt.Fprintf(&b, "发现 %d 条考试信息变动：\n\n", len(events))

	for i, ev := range shown {
		fmt.Fprintf(&b, "【%d】[%s] %s\n", i+1, actionLabel(ev.Action), ev.Title)
		if meta := metaLine(ev); meta != "" {
			fmt.Fprintf(&b, "     %s\n", meta)
		}
		if dl := formatTimePtr(ev.DeadlineAt); dl != "" {
			fmt.Fprintf(&b, "     报名截止：%s\n", dl)
		}
		if ev.URL != "" {
			fmt.Fprintf(&b, "     %s\n", ev.URL)
		}
		b.WriteString("\n")
	}

	if overflow > 0 {
		fmt.Fprintf(&b, "…… 另有 %d 条未在此列出，请登录页面查看。\n\n", overflow)
	}
	b.WriteString("---\n由 exams 自动发送\n")
	return b.String()
}

// renderMarkdown builds the body for the chat platforms.
func renderMarkdown(events []Event) string {
	shown := events
	overflow := 0
	if len(shown) > maxEventsPerMessage {
		overflow = len(shown) - maxEventsPerMessage
		shown = shown[:maxEventsPerMessage]
	}

	var b strings.Builder
	fmt.Fprintf(&b, "**考试信息更新：%d 条**\n\n", len(events))

	for i, ev := range shown {
		title := ev.Title
		if ev.URL != "" {
			fmt.Fprintf(&b, "%d. [%s](%s) `%s`\n", i+1, title, ev.URL, actionLabel(ev.Action))
		} else {
			fmt.Fprintf(&b, "%d. %s `%s`\n", i+1, title, actionLabel(ev.Action))
		}
		if meta := metaLine(ev); meta != "" {
			fmt.Fprintf(&b, "   %s\n", meta)
		}
		if dl := formatTimePtr(ev.DeadlineAt); dl != "" {
			fmt.Fprintf(&b, "   报名截止 %s\n", dl)
		}
	}

	if overflow > 0 {
		fmt.Fprintf(&b, "\n…… 另有 %d 条未列出\n", overflow)
	}
	return b.String()
}

// metaLine renders the source / category / region / date summary.
func metaLine(ev Event) string {
	var parts []string
	if ev.SourceName != "" {
		parts = append(parts, ev.SourceName)
	} else if ev.SourceKey != "" {
		parts = append(parts, ev.SourceKey)
	}
	if ev.Category != "" {
		parts = append(parts, ev.Category)
	}
	if ev.Region != "" {
		parts = append(parts, ev.Region)
	}
	if p := formatTimePtr(ev.PublishedAt); p != "" {
		parts = append(parts, "发布 "+p)
	}
	return strings.Join(parts, " · ")
}
