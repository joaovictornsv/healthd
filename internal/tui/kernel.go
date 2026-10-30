package tui

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/joaovictornsv/healthd/internal/api"
)

// backlog is the kernel indicator's "backlog" detail: what healthd found
// in the kernel messages logged before it started.
type backlog struct {
	Done     bool  `json:"done"`
	Current  boot  `json:"current"`
	Previous *boot `json:"previous"`
}

type boot struct {
	Unavailable string           `json:"unavailable"`
	Count       int              `json:"count"`
	Groups      map[string]group `json:"groups"`
}

type group struct {
	Count  int       `json:"count"`
	First  time.Time `json:"first"`
	Last   time.Time `json:"last"`
	Sample string    `json:"sample"`
}

// kernelBacklog decodes the backlog detail; false if healthd didn't send it.
func kernelBacklog(ind api.Indicator) (backlog, bool) {
	var b backlog
	raw, ok := ind.Detail["backlog"]
	if !ok {
		return b, false
	}
	data, err := json.Marshal(raw)
	if err != nil || json.Unmarshal(data, &b) != nil {
		return b, false
	}
	return b, true
}

// sorted returns the group names, most matches first.
func (b boot) sorted() []string {
	names := make([]string, 0, len(b.Groups))
	for name := range b.Groups {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return b.Groups[names[i]].Count > b.Groups[names[j]].Count })
	return names
}

// span is when a group's matches were logged: "08:43:06–08:43:07".
func (g group) span() string {
	s := g.First.Format("15:04:05")
	if last := g.Last.Format("15:04:05"); last != s {
		s += "–" + last
	}
	if !sameDay(g.First, time.Now()) {
		s = g.First.Format("Mon ") + s
	}
	return s
}

func sameDay(a, b time.Time) bool {
	y1, m1, d1 := a.Date()
	y2, m2, d2 := b.Date()
	return y1 == y2 && m1 == m2 && d1 == d2
}

// bootCheckLines is the boot check section of the kernel detail panel:
// per boot, each group's count, then when and the first line it matched.
func bootCheckLines(ind api.Indicator, w int) []string {
	b, ok := kernelBacklog(ind)
	if !ok {
		return nil
	}
	header := " " + bold.Render("Boot check")
	if s, ok := ind.Detail["boot_summary"].(map[string]any); ok {
		t := parseTime(s["time"])
		if sent, _ := s["sent"].(bool); sent {
			header += muted.Render(" · summary sent " + when(t))
		} else {
			note, _ := s["note"].(string)
			header += muted.Render(" · summary not sent: " + note)
		}
	} else if due := parseTime(ind.Detail["boot_summary_due"]); !due.IsZero() {
		header += muted.Render(" · summary at " + due.Format("15:04"))
	}
	lines := []string{header}
	if !b.Done {
		return append(lines, muted.Render("   scanning the journal…"))
	}
	row := func(label, text string) {
		lines = append(lines, truncate(fmt.Sprintf("   %-14s %s", label, text), w))
	}
	show := func(label string, f boot) {
		switch {
		case f.Unavailable != "":
			row(label, muted.Render("not checked: "+f.Unavailable))
			return
		case f.Count == 0:
			row(label, "no errors")
			return
		}
		for _, name := range f.sorted() {
			g := f.Groups[name]
			row(label, lipgloss.NewStyle().Foreground(colorCritical).Render(api.KernelErrors(name, g.Count)))
			lines = append(lines, muted.Render(truncate("     "+g.span()+"  "+g.Sample, w)))
			label = ""
		}
	}
	if b.Previous != nil {
		show("previous boot", *b.Previous)
	} else {
		row("previous boot", muted.Render("not in the journal"))
	}
	show("this boot", b.Current)
	return lines
}

func parseTime(v any) time.Time {
	s, _ := v.(string)
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

// BootFindingLine is a short note on errors found from before healthd
// started, for the kernel card and "health status", or "".
func BootFindingLine(ind api.Indicator) string {
	b, ok := kernelBacklog(ind)
	if !ok {
		return ""
	}
	if p := b.Previous; p != nil && p.Count > 0 && p.Unavailable == "" {
		name := p.sorted()[0]
		return "prev boot: " + api.KernelErrors(name, p.Groups[name].Count)
	}
	if b.Current.Count > 0 {
		name := b.Current.sorted()[0]
		return "this boot: " + api.KernelErrors(name, b.Current.Groups[name].Count)
	}
	return ""
}

// kernelEventLine is one live burst: "×1,536 nvme 0000:03:00.0: …".
func kernelEventLine(e map[string]any) string {
	msg, _ := e["message"].(string)
	if n, ok := e["count"].(float64); ok && n > 1 {
		return "×" + api.Thousands(int(n)) + " " + msg
	}
	return msg
}
