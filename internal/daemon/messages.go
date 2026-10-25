package daemon

import (
	"fmt"
	"strings"
	"time"

	"github.com/joaovictornsv/healthd/internal/alert"
	"github.com/joaovictornsv/healthd/internal/api"
	"github.com/joaovictornsv/healthd/internal/metrics"
	"github.com/joaovictornsv/healthd/internal/notify"
)

func (ind *indicator) displayName() string {
	return api.Indicator{Name: ind.name, Device: ind.device}.DisplayName()
}

func (ind *indicator) value() string {
	return api.FormatValue(ind.unit, ind.reading.Value)
}

func (ind *indicator) threshold(v float64) string {
	return api.FormatValue(ind.unit, v)
}

// badWord describes the problem direction: "high" or "low".
func (ind *indicator) badWord() string {
	if ind.rule.Below {
		return "low"
	}
	return "high"
}

// summary is the one-line description stored with an alert event.
func summary(ind *indicator, ev alert.Event) string {
	name := ind.displayName()
	switch ev {
	case alert.Fired:
		return fmt.Sprintf("%s %s: %s (alert at %s)", name, ind.badWord(), ind.value(), ind.threshold(ind.rule.FireAt))
	case alert.Escalated:
		return fmt.Sprintf("%s critical: %s (critical at %s)", name, ind.value(), ind.threshold(*ind.rule.CriticalAt))
	case alert.Resolved:
		return fmt.Sprintf("%s back to normal: %s", name, ind.value())
	case alert.Cleared:
		return fmt.Sprintf("%s alert cleared (%s)", name, ind.reading.Status)
	}
	return name
}

func message(ind *indicator, ev alert.Event, top []metrics.Proc) notify.Message {
	name := ind.displayName()
	m := notify.Message{Key: ind.id, Urgency: notify.Normal, Icon: "dialog-warning", Sound: "dialog-warning"}
	if ind.alert.Critical {
		m.Urgency, m.Icon, m.Sound = notify.Critical, "dialog-error", "dialog-error"
	}

	var body []string
	switch ev {
	case alert.Fired:
		m.Title = fmt.Sprintf("%s %s %s", icon(ind), name, ind.badWord())
		line := fmt.Sprintf("%s is at %s", name, ind.value())
		if ind.rule.For > 0 {
			line += fmt.Sprintf(" for %s", metrics.Duration(ind.rule.For))
		}
		body = append(body, line+fmt.Sprintf(" (alert at %s).", ind.threshold(ind.rule.FireAt)))
	case alert.Escalated:
		m.Title = fmt.Sprintf("🚨 %s critical", name)
		body = append(body, fmt.Sprintf("%s is at %s (critical at %s).", name, ind.value(), ind.threshold(*ind.rule.CriticalAt)))
	case alert.Repeated:
		state := "still " + ind.badWord()
		if ind.alert.Critical {
			state = "still critical"
		}
		m.Title = fmt.Sprintf("%s %s %s", icon(ind), name, state)
		body = append(body, fmt.Sprintf("%s is at %s, for %s now.", name, ind.value(), metrics.Duration(time.Since(ind.alert.Since))))
	case alert.Resolved:
		m.Title = fmt.Sprintf("✅ %s back to normal", name)
		m.Urgency, m.Icon, m.Sound = notify.Low, "dialog-information", "complete"
		body = append(body, fmt.Sprintf("%s is at %s.", name, ind.value()))
	}

	if ev != alert.Resolved {
		body = append(body, advice(ind)...)
		if len(top) > 0 {
			body = append(body, "Top processes:")
			for _, p := range top[:min(3, len(top))] {
				body = append(body, "  "+procLine(ind.kind, p))
			}
		}
	}
	m.Body = strings.Join(body, "\n")
	return m
}

func icon(ind *indicator) string {
	switch {
	case ind.alert.Critical:
		return "🚨"
	case ind.kind == "battery":
		return "🔋"
	case ind.kind == "cpu_temp" || ind.kind == "disk_temp":
		return "🔥"
	}
	return "⚠️"
}

func advice(ind *indicator) []string {
	switch ind.kind {
	case "battery":
		return []string{ind.reading.Status + ". Connect power."}
	case "kernel":
		var lines []string
		if recent, ok := ind.reading.Detail["recent"].([]metrics.KernelEvent); ok {
			for _, e := range recent[:min(3, len(recent))] {
				lines = append(lines, truncate(e.Message, 160))
			}
		}
		return lines
	case "disk_usage":
		return []string{ind.reading.Status + " used."}
	}
	return nil
}

func procLine(kind string, p metrics.Proc) string {
	if kind == "memory" || kind == "swap" {
		return fmt.Sprintf("%s (%s)", p.Name, metrics.Bytes(p.Mem))
	}
	return fmt.Sprintf("%s (%.0f%% CPU)", p.Name, p.CPU)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
