package tui

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/joaovictornsv/healthd/internal/api"
)

var rangeSpans = map[string]time.Duration{
	"15m": 15 * time.Minute,
	"1h":  time.Hour,
	"24h": 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
}

func timeSince(t time.Time) time.Duration {
	if t.IsZero() {
		return 0
	}
	return time.Since(t)
}

const panelLines = 7

func (m Model) detailView() string {
	ind, ok := m.indicator(m.detailID)
	if !ok {
		return muted.Render(" indicator no longer available · esc to go back")
	}
	w := m.width

	// Title: name on the left, state and value on the right.
	state := stateLabel(ind)
	if ind.State != "ok" {
		state += muted.Render(" for " + human(timeSince(ind.Since)))
	}
	if snoozed(ind) {
		state += muted.Render(" · zz")
	}
	title := spread(" "+bold.Render(ind.DisplayName()), state+muted.Render(" · ")+bold.Render(value(ind))+" ", w)

	// Range tabs and stats.
	var tabs []string
	for i, r := range ranges {
		label := fmt.Sprintf("%d %s", i+1, r)
		if r == m.rng {
			tabs = append(tabs, accent.Bold(true).Render("["+label+"]"))
		} else {
			tabs = append(tabs, muted.Render(" "+label+" "))
		}
	}
	stats := ""
	if len(m.history) > 0 {
		lo, hi, sum := math.Inf(1), math.Inf(-1), 0.0
		for _, p := range m.history {
			lo, hi, sum = min(lo, p.V), max(hi, p.V), sum+p.V
		}
		f := func(v float64) string { return api.FormatValue(ind.Unit, v) }
		stats = muted.Render(fmt.Sprintf("min %s · avg %s · max %s ", f(lo), f(sum/float64(len(m.history))), f(hi)))
	}
	tabLine := spread(" "+strings.Join(tabs, " "), stats, w)

	half := w / 2
	info := contextPanel(ind, half-2)
	// The kernel panel also holds the boot check, so it can grow; its chart
	// (mostly zeros) gives up the room.
	panelHeight := max(panelLines, len(info))
	chartHeight := max(4, m.height-2-2-panelHeight-2)
	chart := m.chart(ind, w, chartHeight)

	left := lipgloss.NewStyle().Width(half).Render(strings.Join(info, "\n"))
	right := lipgloss.NewStyle().Width(w - half).Render(strings.Join(m.recentAlerts(ind, w-half-2), "\n"))
	panels := lipgloss.JoinHorizontal(lipgloss.Top, left, right)

	return strings.Join([]string{title, tabLine, chart, "", panels}, "\n")
}

func (m Model) chart(ind api.Indicator, w, h int) string {
	const axis, labels = 6, 9
	plotW := w - axis - labels
	if plotW < 10 {
		return ""
	}
	if len(m.history) == 0 {
		return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, muted.Render("no data for this range yet"))
	}

	values := make([]float64, len(m.history))
	for i, p := range m.history {
		values[i] = p.V
	}
	lo, hi := scale(ind, values)
	if ind.Unit != "%" {
		pad := math.Max(2, (hi-lo)*0.1)
		lo, hi = math.Max(0, lo-pad), hi+pad
	}
	t := ind.Thresholds
	type hline struct {
		v     float64
		label string
		color lipgloss.Color
	}
	var lines []hline
	if ind.Kind != "kernel" {
		lines = append(lines, hline{t.FireAt, "fire", colorPending})
		if t.CriticalAt != nil {
			lines = append(lines, hline{*t.CriticalAt, "critical", colorCritical})
		}
	}

	now := time.Now().Unix()
	from := now - int64(rangeSpans[m.rng].Seconds())
	grid := newBraille(plotW, h)
	rowOf := func(v float64) int { // dot row, 0 = top
		f := (v - lo) / (hi - lo)
		return int(math.Round((1 - f) * float64(grid.dotsH()-1)))
	}
	colOf := func(ts int64) int {
		return int(math.Round(float64(ts-from) / float64(now-from) * float64(grid.dotsW()-1)))
	}
	// Connect neighbouring points unless there is a gap in the data (the
	// service was stopped), so outages show as breaks in the line.
	maxGap := expectedStep(m.history) * 3
	for i, p := range m.history {
		x, y := colOf(p.T), rowOf(p.V)
		if i > 0 && p.T-m.history[i-1].T <= maxGap {
			grid.line(colOf(m.history[i-1].T), rowOf(m.history[i-1].V), x, y)
		} else {
			grid.set(x, y)
		}
	}

	cellRowOf := func(v float64) int { return rowOf(v) / 4 }
	var out []string
	for r := 0; r < h; r++ {
		var label string
		var lineColor lipgloss.Color
		for _, l := range lines {
			if l.v >= lo && l.v <= hi && cellRowOf(l.v) == r {
				label, lineColor = l.label, l.color
			}
		}
		yLabel := ""
		switch {
		case label != "":
			for _, l := range lines {
				if l.label == label {
					yLabel = axisValue(l.v)
				}
			}
		case r == 0:
			yLabel = axisValue(hi)
		case r == h-1:
			yLabel = axisValue(lo)
		}
		var row strings.Builder
		for c := 0; c < plotW; c++ {
			if ch, ok := grid.cell(c, r); ok {
				row.WriteString(accent.Render(string(ch)))
			} else if label != "" {
				row.WriteString(lipgloss.NewStyle().Foreground(lineColor).Render("┈"))
			} else {
				row.WriteByte(' ')
			}
		}
		out = append(out, fmt.Sprintf("%4s ┤", yLabel)+row.String()+" "+lipgloss.NewStyle().Foreground(lineColor).Render(label))
	}
	return strings.Join(out, "\n")
}

func axisValue(v float64) string {
	return fmt.Sprintf("%.0f", v)
}

// expectedStep is the typical spacing between points, in seconds.
func expectedStep(points []api.Point) int64 {
	if len(points) < 2 {
		return 60
	}
	steps := make([]int64, 0, len(points)-1)
	for i := 1; i < len(points); i++ {
		steps = append(steps, points[i].T-points[i-1].T)
	}
	slices.Sort(steps)
	return max(1, steps[len(steps)/2])
}

func contextPanel(ind api.Indicator, w int) []string {
	t := ind.Thresholds
	f := func(v float64) string { return api.FormatValue(ind.Unit, v) }
	op, back := "≥", "<"
	if t.Direction == "below" {
		op, back = "≤", ">"
	}
	lines := []string{" " + bold.Render("Thresholds") + muted.Render(" ("+t.Source+")")}
	if ind.Kind == "kernel" {
		lines = append(lines, "   any matching kernel error · critical")
	} else {
		forText := ""
		if t.For != "0s" {
			forText = " for " + shortDuration(t.For)
		}
		lines = append(lines, fmt.Sprintf("   fire %s %s%s · resolve %s %s", op, f(t.FireAt), forText, back, f(t.ResolveAt)))
		if t.CriticalAt != nil {
			lines = append(lines, fmt.Sprintf("   critical %s %s", op, f(*t.CriticalAt)))
		}
	}
	lines = append(lines, "")

	switch ind.Kind {
	case "cpu_load", "cpu_temp", "memory", "swap":
		byMem := ind.Kind == "memory" || ind.Kind == "swap"
		header := "Top processes (CPU)"
		if byMem {
			header = "Top processes (memory)"
		}
		lines = append(lines, " "+bold.Render(header))
		top := procs(ind)
		for _, p := range top[:min(3, len(top))] {
			amount := fmt.Sprintf("%.0f%%", p.cpu)
			if byMem {
				amount = bytes(p.mem)
			}
			lines = append(lines, fmt.Sprintf("   %-20s %10s", truncate(p.name, 20), amount))
		}
	case "battery":
		lines = append(lines, " "+bold.Render("Battery"), "   "+ind.Status)
		if c, ok := detailFloat(ind, "cycle_count"); ok {
			lines = append(lines, fmt.Sprintf("   %.0f charge cycles", c))
		}
	case "disk_usage":
		used, _ := detailFloat(ind, "used")
		free, _ := detailFloat(ind, "free")
		total, _ := detailFloat(ind, "total")
		lines = append(lines, " "+bold.Render(ind.Device),
			fmt.Sprintf("   used %s · free %s · total %s", bytes(used), bytes(free), bytes(total)))
	case "kernel":
		if boot := bootCheckLines(ind, w); len(boot) > 0 {
			lines = append(append(lines, boot...), "")
		}
		lines = append(lines, " "+bold.Render("Recent kernel errors"))
		recent, _ := ind.Detail["recent"].([]any)
		if len(recent) == 0 {
			lines = append(lines, muted.Render("   none since healthd started"))
		}
		for _, r := range recent[:min(3, len(recent))] {
			e, _ := r.(map[string]any)
			lines = append(lines, "   "+muted.Render(when(parseTime(e["time"])))+" "+kernelEventLine(e))
		}
	}
	if ind.Kind == "disk_temp" || ind.Kind == "cpu_temp" {
		sensor, _ := ind.Detail["sensor"].(string)
		source, _ := ind.Detail["source"].(string)
		lines = append(lines, muted.Render(fmt.Sprintf("   sensor: %s (%s)", sensor, source)))
	}
	for i := range lines {
		lines[i] = truncate(lines[i], w)
	}
	return lines
}

func (m Model) recentAlerts(ind api.Indicator, w int) []string {
	lines := []string{" " + bold.Render("Recent alerts")}
	if len(m.events) == 0 {
		return append(lines, muted.Render("   none"))
	}
	for _, e := range m.events[:min(panelLines-1, len(m.events))] {
		lines = append(lines, truncate(fmt.Sprintf("   %-15s %s  %s",
			when(e.Time), eventLabel(e), api.FormatValue(ind.Unit, e.Value)), w))
	}
	return lines
}

func eventLabel(e api.Event) string {
	label := strings.ToUpper(e.Kind)
	color := colorFiring
	switch {
	case e.Kind == "resolved" || e.Kind == "cleared":
		color = colorOK
	case e.Critical:
		label, color = "CRITICAL", colorCritical
	}
	return lipgloss.NewStyle().Foreground(color).Width(9).Render(label)
}

func (m Model) alertsView() string {
	var active []string
	for _, ind := range m.status.Indicators {
		lvl := levelOf(ind)
		if lvl == levelOK || lvl == levelError {
			continue
		}
		since := "since " + ind.Since.Format("15:04") + " (" + human(timeSince(ind.Since))
		if lvl == levelPending {
			since += " of " + shortDuration(ind.Thresholds.For)
		}
		since += ")"
		dot := lipgloss.NewStyle().Foreground(lvl.color()).Render("●")
		if lvl == levelPending {
			dot = lipgloss.NewStyle().Foreground(lvl.color()).Render("◐")
		}
		zz := ""
		if snoozed(ind) {
			zz = muted.Render(" zz")
		}
		active = append(active, fmt.Sprintf(" %s %s %-18s %-8s %s%s", dot,
			lipgloss.NewStyle().Width(9).Render(stateLabel(ind)), ind.DisplayName(), value(ind), muted.Render(since), zz))
	}
	if len(active) == 0 {
		active = []string{lipgloss.NewStyle().Foreground(colorOK).Render(" ● no active alerts")}
	}

	var history []string
	for _, e := range m.events {
		history = append(history, truncate(fmt.Sprintf(" %-15s %s %-18s %s",
			when(e.Time), eventLabel(e), e.Indicator, muted.Render(e.Message)), m.width))
	}
	if len(history) == 0 {
		history = []string{muted.Render(" no past alerts")}
	}

	head := append([]string{" " + bold.Render("Active"), ""}, active...)
	head = append(head, "", " "+bold.Render("History"), "")
	room := max(1, m.height-2-len(head))
	scroll := min(m.scroll, max(0, len(history)-room))
	end := min(len(history), scroll+room)
	return strings.Join(append(head, history[scroll:end]...), "\n")
}

// braille is a canvas of 2×4 dots per terminal cell.
type braille struct {
	w, h  int
	cells [][]rune
	used  [][]bool
}

func newBraille(w, h int) *braille {
	b := &braille{w: w, h: h, cells: make([][]rune, h), used: make([][]bool, h)}
	for r := range b.cells {
		b.cells[r] = make([]rune, w)
		b.used[r] = make([]bool, w)
	}
	return b
}

func (b *braille) dotsW() int { return b.w * 2 }
func (b *braille) dotsH() int { return b.h * 4 }

var brailleBits = [2][4]rune{{0x01, 0x02, 0x04, 0x40}, {0x08, 0x10, 0x20, 0x80}}

func (b *braille) set(x, y int) {
	if x < 0 || y < 0 || x >= b.dotsW() || y >= b.dotsH() {
		return
	}
	c, r := x/2, y/4
	b.cells[r][c] |= brailleBits[x%2][y%4]
	b.used[r][c] = true
}

// line draws between two dots (Bresenham).
func (b *braille) line(x0, y0, x1, y1 int) {
	dx, dy := abs(x1-x0), -abs(y1-y0)
	sx, sy := sign(x1-x0), sign(y1-y0)
	err := dx + dy
	for {
		b.set(x0, y0)
		if x0 == x1 && y0 == y1 {
			return
		}
		e2 := 2 * err
		if e2 >= dy {
			err += dy
			x0 += sx
		}
		if e2 <= dx {
			err += dx
			y0 += sy
		}
	}
}

func (b *braille) cell(c, r int) (rune, bool) {
	if !b.used[r][c] {
		return 0, false
	}
	return 0x2800 + b.cells[r][c], true
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func sign(v int) int {
	switch {
	case v > 0:
		return 1
	case v < 0:
		return -1
	}
	return 0
}

// shortDuration turns "5m0s" into "5m" and "1h0m0s" into "1h".
func shortDuration(s string) string {
	d, err := time.ParseDuration(s)
	if err != nil {
		return s
	}
	out := d.String()
	if strings.HasSuffix(out, "m0s") {
		out = strings.TrimSuffix(out, "0s")
	}
	if strings.HasSuffix(out, "h0m") {
		out = strings.TrimSuffix(out, "0m")
	}
	return out
}
