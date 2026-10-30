package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/joaovictornsv/healthd/internal/api"
)

// card groups the indicators of one kind: all drives in "Disk temp", all
// mounts in "Disk usage", one indicator for everything else.
type card struct {
	kind string
	inds []api.Indicator
}

func cards(s *api.Status) []card {
	if s == nil {
		return nil
	}
	var out []card
	index := map[string]int{}
	for _, ind := range s.Indicators {
		i, ok := index[ind.Kind]
		if !ok {
			i = len(out)
			index[ind.Kind] = i
			out = append(out, card{kind: ind.Kind})
		}
		out[i].inds = append(out[i].inds, ind)
	}
	return out
}

func worst(inds []api.Indicator) api.Indicator {
	w := inds[0]
	for _, ind := range inds[1:] {
		if levelOf(ind) > levelOf(w) {
			w = ind
		}
	}
	return w
}

const (
	minCardWidth  = 30
	compactBelow  = 60
	cardBodyLines = 3
)

func (m Model) columns() int {
	switch {
	case m.width < compactBelow:
		return 1
	case m.width >= 3*minCardWidth:
		return 3
	}
	return 2
}

func (m Model) dashboardView() string {
	cs := cards(m.status)
	if m.width < compactBelow {
		return m.compactView(cs)
	}
	cols := m.columns()
	cardWidth := m.width / cols
	var rows []string
	for start := 0; start < len(cs); start += cols {
		end := min(start+cols, len(cs))
		height := cardBodyLines
		for _, c := range cs[start:end] {
			height = max(height, len(c.inds))
		}
		var row []string
		for i := start; i < end; i++ {
			row = append(row, renderCard(cs[i], cardWidth, height, i == m.cursor))
		}
		rows = append(rows, lipgloss.JoinHorizontal(lipgloss.Top, row...))
	}
	warnings := ""
	for _, w := range m.status.Warnings {
		warnings += "\n" + lipgloss.NewStyle().Foreground(colorPending).Render(" warning: "+w)
	}
	return lipgloss.JoinVertical(lipgloss.Left, rows...) + warnings
}

func renderCard(c card, width, height int, selected bool) string {
	inner := width - 4 // border + padding on both sides
	w := worst(c.inds)
	lvl := levelOf(w)

	var lines []string
	if len(c.inds) == 1 {
		lines = singleCard(w, inner)
	} else {
		for _, ind := range c.inds {
			lines = append(lines, deviceLine(ind, inner))
		}
	}

	title := " " + w.Name + " "
	if len(c.inds) == 1 && w.Device != "" {
		title = " " + w.DisplayName() + " "
	}
	for _, ind := range c.inds {
		if snoozed(ind) {
			title += "zz "
			break
		}
	}

	border := lipgloss.RoundedBorder()
	if selected {
		border = lipgloss.ThickBorder()
	}
	box := lipgloss.NewStyle().
		Border(border).BorderForeground(lvl.color()).
		Padding(0, 1).Width(width - 2).Height(height).
		Render(strings.Join(lines, "\n"))

	// Put the title into the top border.
	titleStyle := lipgloss.NewStyle().Foreground(lvl.color())
	if selected {
		titleStyle = titleStyle.Bold(true).Reverse(true)
	}
	boxLines := strings.Split(box, "\n")
	top := lipgloss.NewStyle().Foreground(lvl.color()).Render(string([]rune(border.TopLeft)[0]) + string([]rune(border.Top)[0]))
	rest := width - 2 - lipgloss.Width(title)
	boxLines[0] = top + titleStyle.Render(title) +
		lipgloss.NewStyle().Foreground(lvl.color()).Render(strings.Repeat(border.Top, max(0, rest-1))+border.TopRight)
	return strings.Join(boxLines, "\n")
}

func singleCard(ind api.Indicator, w int) []string {
	if ind.Error != "" {
		return []string{
			spread(bold.Render("—"), stateLabel(ind), w),
			"",
			muted.Render(truncate(ind.Error, w)),
		}
	}

	head := bold.Render(value(ind))
	switch ind.Kind {
	case "memory", "swap", "disk_usage":
		head += "  " + muted.Render(ind.Status)
	case "battery":
		head += "  " + muted.Render(firstPart(ind.Status))
	}
	lines := []string{spread(head, stateLabel(ind), w)}

	switch ind.Kind {
	case "battery", "disk_usage":
		v := 0.0
		if ind.Value != nil {
			v = *ind.Value
		}
		lines = append(lines, bar(v, w, levelOf(ind).color()))
	case "kernel":
		lines = append(lines, muted.Render(truncate(ind.Status, w)))
	default:
		lo, hi := scale(ind, ind.Spark)
		lines = append(lines, accent.Render(sparkline(ind.Spark, w, lo, hi)))
	}
	lines = append(lines, muted.Render(truncate(context(ind), w)))
	return lines
}

// context is the third line of a card: the most useful extra fact.
func context(ind api.Indicator) string {
	t := ind.Thresholds
	op := ""
	if t.Direction == "below" {
		op = "≤"
	}
	limits := "fire " + op + api.FormatValue(ind.Unit, t.FireAt)
	if t.CriticalAt != nil {
		limits += " · crit " + op + api.FormatValue(ind.Unit, *t.CriticalAt)
	}
	switch ind.Kind {
	case "battery":
		if _, rest, ok := strings.Cut(ind.Status, ", "); ok {
			return rest
		}
		return limits
	case "cpu_load":
		var parts []string
		for _, p := range procs(ind) {
			if len(parts) == 2 {
				break
			}
			parts = append(parts, fmt.Sprintf("%s %.0f%%", p.name, p.cpu))
		}
		if len(parts) > 0 {
			return strings.Join(parts, " · ")
		}
	case "kernel":
		if recent, ok := ind.Detail["recent"].([]any); ok && len(recent) > 0 {
			if e, ok := recent[0].(map[string]any); ok {
				return "last: " + kernelEventLine(e)
			}
		}
		if line := BootFindingLine(ind); line != "" {
			return line
		}
		source, _ := ind.Detail["source"].(string)
		return "source: " + source
	}
	if ind.State != "ok" {
		return "for " + human(timeSince(ind.Since)) + " · " + limits
	}
	return limits
}

// deviceLine is one row in a multi-device card.
func deviceLine(ind api.Indicator, w int) string {
	name := fmt.Sprintf("%-7s", ind.Device)
	val := fmt.Sprintf("%5s", value(ind))
	state := stateLabel(ind)
	if ind.Error != "" {
		return spread(name+" "+muted.Render(truncate(ind.Error, w-18)), state, w)
	}
	left := name + " " + bold.Render(val)
	if ind.Kind == "disk_usage" && ind.Value != nil {
		barWidth := w - lipgloss.Width(left) - lipgloss.Width(state) - 3
		if barWidth >= 4 {
			left += " " + bar(*ind.Value, barWidth, levelOf(ind).color())
		}
	}
	return spread(left, state, w)
}

func firstPart(s string) string {
	first, _, _ := strings.Cut(s, ", ")
	return first
}

// compactView is the narrow-terminal layout: one line per indicator.
func (m Model) compactView(cs []card) string {
	var lines []string
	for ci, c := range cs {
		for _, ind := range c.inds {
			name := ind.DisplayName()
			if ci == m.cursor {
				name = lipgloss.NewStyle().Reverse(true).Render(name)
			}
			lo, hi := scale(ind, ind.Spark)
			line := fmt.Sprintf(" %s %s %s %s",
				lipgloss.NewStyle().Width(18).Render(name),
				lipgloss.NewStyle().Width(9).Render(value(ind)),
				lipgloss.NewStyle().Width(9).Render(stateLabel(ind)),
				accent.Render(sparkline(ind.Spark, max(0, m.width-42), lo, hi)))
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}
