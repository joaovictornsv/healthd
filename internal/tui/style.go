package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/joaovictornsv/healthd/internal/api"
)

var (
	colorOK       = lipgloss.Color("42")
	colorPending  = lipgloss.Color("214")
	colorFiring   = lipgloss.Color("203")
	colorCritical = lipgloss.Color("196")
	colorMuted    = lipgloss.Color("245")
	colorAccent   = lipgloss.Color("39")

	bold   = lipgloss.NewStyle().Bold(true)
	muted  = lipgloss.NewStyle().Foreground(colorMuted)
	accent = lipgloss.NewStyle().Foreground(colorAccent)
)

// level orders states by severity so a card can show its worst indicator.
type level int

const (
	levelOK level = iota
	levelError
	levelPending
	levelFiring
	levelCritical
)

func levelOf(ind api.Indicator) level {
	switch {
	case ind.State == "firing" && ind.Critical:
		return levelCritical
	case ind.State == "firing":
		return levelFiring
	case ind.State == "pending":
		return levelPending
	case ind.Error != "":
		return levelError
	}
	return levelOK
}

func (l level) color() lipgloss.Color {
	switch l {
	case levelCritical:
		return colorCritical
	case levelFiring:
		return colorFiring
	case levelPending:
		return colorPending
	case levelError:
		return colorMuted
	}
	return colorOK
}

func (l level) label() string {
	switch l {
	case levelCritical:
		return "CRITICAL"
	case levelFiring:
		return "FIRING"
	case levelPending:
		return "PENDING"
	case levelError:
		return "ERROR"
	}
	return "OK"
}

func stateLabel(ind api.Indicator) string {
	l := levelOf(ind)
	s := lipgloss.NewStyle().Foreground(l.color())
	if l == levelCritical {
		s = s.Bold(true)
	}
	return s.Render(l.label())
}

func snoozed(ind api.Indicator) bool {
	return ind.SnoozeUntilResolved || (ind.SnoozedUntil != nil && time.Now().Before(*ind.SnoozedUntil))
}

func value(ind api.Indicator) string {
	if ind.Value == nil {
		return "—"
	}
	return api.FormatValue(ind.Unit, *ind.Value)
}

// spread places left and right text at the edges of a line of width w.
func spread(left, right string, w int) string {
	gap := w - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		return truncate(left, w-lipgloss.Width(right)-1) + " " + right
	}
	return left + strings.Repeat(" ", gap) + right
}

func truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	return ansi.Truncate(s, w, "…")
}

var sparkChars = []rune("▁▂▃▄▅▆▇█")

// sparkline draws values scaled between lo and hi, resampled to width w.
func sparkline(values []float64, w int, lo, hi float64) string {
	if w <= 0 || len(values) == 0 {
		return ""
	}
	values = resample(values, w)
	var b strings.Builder
	for _, v := range values {
		f := 0.0
		if hi > lo {
			f = (v - lo) / (hi - lo)
		}
		i := int(f*float64(len(sparkChars)-1) + 0.5)
		b.WriteRune(sparkChars[max(0, min(i, len(sparkChars)-1))])
	}
	return b.String()
}

// resample averages values into at most w buckets, keeping the newest at the end.
func resample(values []float64, w int) []float64 {
	if len(values) <= w {
		return values
	}
	out := make([]float64, w)
	for i := range out {
		start, end := i*len(values)/w, (i+1)*len(values)/w
		sum := 0.0
		for _, v := range values[start:end] {
			sum += v
		}
		out[i] = sum / float64(end-start)
	}
	return out
}

// bar draws a horizontal fill bar for a 0–100 value.
func bar(pct float64, w int, color lipgloss.Color) string {
	if w <= 0 {
		return ""
	}
	filled := int(pct/100*float64(w) + 0.5)
	filled = max(0, min(filled, w))
	return lipgloss.NewStyle().Foreground(color).Render(strings.Repeat("█", filled)) +
		muted.Render(strings.Repeat("░", w-filled))
}

// scale returns a sensible fixed range for drawing an indicator, so the
// height of a sparkline means the same thing over time.
func scale(ind api.Indicator, values []float64) (lo, hi float64) {
	switch ind.Unit {
	case "%":
		return 0, 100
	case "errors":
		hi = 2
		for _, v := range values {
			hi = max(hi, v+1)
		}
		return 0, hi
	}
	lo, hi = ind.Thresholds.FireAt-30, ind.Thresholds.FireAt
	if ind.Thresholds.CriticalAt != nil {
		hi = *ind.Thresholds.CriticalAt
	}
	for _, v := range values {
		lo, hi = min(lo, v), max(hi, v)
	}
	return max(0, lo), hi
}

func human(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
}

func when(t time.Time) string {
	now := time.Now()
	y1, m1, d1 := t.Date()
	y2, m2, d2 := now.Date()
	switch {
	case y1 == y2 && m1 == m2 && d1 == d2:
		return "today " + t.Format("15:04")
	case now.Sub(t) < 48*time.Hour && now.AddDate(0, 0, -1).Day() == d1:
		return "yesterday " + t.Format("15:04")
	}
	return t.Format("Jan 02 15:04")
}

type proc struct {
	name string
	cpu  float64
	mem  float64
}

// procs reads the top_processes detail, which arrives as generic JSON.
func procs(ind api.Indicator) []proc {
	raw, _ := ind.Detail["top_processes"].([]any)
	var out []proc
	for _, r := range raw {
		m, _ := r.(map[string]any)
		name, _ := m["name"].(string)
		cpu, _ := m["cpu"].(float64)
		mem, _ := m["mem"].(float64)
		out = append(out, proc{name, cpu, mem})
	}
	return out
}

func detailFloat(ind api.Indicator, key string) (float64, bool) {
	v, ok := ind.Detail[key].(float64)
	return v, ok
}

func bytes(b float64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%.0f B", b)
	}
	div, exp := float64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", b/div, "KMGTPE"[exp])
}
