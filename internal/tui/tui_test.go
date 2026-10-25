package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/joaovictornsv/healthd/internal/api"
)

func fptr(v float64) *float64 { return &v }

func fixture() *api.Status {
	now := time.Now()
	spark := []float64{50, 55, 70, 84, 86, 85}
	return &api.Status{
		Host: "myhost", UptimeSeconds: 3600, SampledAt: now,
		Indicators: []api.Indicator{
			{ID: "battery", Kind: "battery", Name: "Battery", Unit: "%", Value: fptr(18), Status: "discharging, 0h 40m left",
				State: "firing", Since: now.Add(-time.Minute), Active: true,
				Thresholds: api.Thresholds{Direction: "below", FireAt: 20, ResolveAt: 25, CriticalAt: fptr(10), For: "0s", Source: "default"}},
			{ID: "cpu_temp", Kind: "cpu_temp", Name: "CPU temp", Unit: "°C", Value: fptr(95), State: "firing", Critical: true,
				Since: now.Add(-7 * time.Minute), Spark: spark,
				Thresholds: api.Thresholds{Direction: "above", FireAt: 80, ResolveAt: 70, CriticalAt: fptr(92), For: "5m0s", Source: "hardware"}},
			{ID: "disk_temp:nvme0", Kind: "disk_temp", Name: "Disk temp", Device: "nvme0", Unit: "°C", Value: fptr(41), State: "ok",
				Thresholds: api.Thresholds{Direction: "above", FireAt: 63, ResolveAt: 56, For: "3m0s", Source: "hardware"}},
			{ID: "disk_temp:sda", Kind: "disk_temp", Name: "Disk temp", Device: "sda", Unit: "°C", State: "ok",
				Error:      "read /sys/class/hwmon/hwmon9/temp1_input: no such file",
				Thresholds: api.Thresholds{Direction: "above", FireAt: 63, ResolveAt: 56, For: "3m0s", Source: "default"}},
			{ID: "cpu_load", Kind: "cpu_load", Name: "CPU load", Unit: "%", Value: fptr(83), State: "pending", Since: now.Add(-3 * time.Minute),
				Spark:      spark,
				Detail:     map[string]any{"top_processes": []any{map[string]any{"name": "make", "cpu": 612.0, "mem": 1e9}}},
				Thresholds: api.Thresholds{Direction: "above", FireAt: 80, ResolveAt: 60, For: "5m0s", Source: "default"}},
			{ID: "disk_usage:/", Kind: "disk_usage", Name: "Disk usage", Device: "/", Unit: "%", Value: fptr(72), State: "ok",
				Thresholds: api.Thresholds{Direction: "above", FireAt: 85, ResolveAt: 80, For: "5m0s", Source: "default"}},
			{ID: "kernel", Kind: "kernel", Name: "Kernel", Unit: "errors", Value: fptr(0), State: "ok", Status: "no errors in the last 5m",
				Detail:     map[string]any{"source": "journal"},
				Thresholds: api.Thresholds{Direction: "above", FireAt: 1, ResolveAt: 1, CriticalAt: fptr(1), For: "0s", Source: "default"}},
		},
	}
}

func render(t *testing.T, m tea.Model, w, h int) (tea.Model, string) {
	t.Helper()
	m, _ = m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	out := ansi.Strip(m.View())
	lines := strings.Split(out, "\n")
	if len(lines) != h {
		t.Errorf("%dx%d: view has %d lines, want %d", w, h, len(lines), h)
	}
	for i, l := range lines {
		if ansi.StringWidth(l) > w {
			t.Errorf("%dx%d: line %d is %d wide: %q", w, h, i, ansi.StringWidth(l), l)
		}
	}
	return m, out
}

func TestDashboardLayouts(t *testing.T) {
	var m tea.Model = New(nil, "")
	m, _ = m.Update(statusMsg{status: fixture()})
	for _, size := range [][2]int{{120, 30}, {100, 30}, {70, 30}, {50, 20}} {
		var out string
		m, out = render(t, m, size[0], size[1])
		for _, want := range []string{"CPU temp", "CRITICAL", "PENDING", "1 pending"} {
			if !strings.Contains(out, want) {
				t.Errorf("%dx%d: missing %q\n%s", size[0], size[1], want, out)
			}
		}
	}
	_, out := render(t, m, 100, 30)
	for _, want := range []string{"Disk temp", "nvme0", "sda", "ERROR", "make 612%", "40m left"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q\n%s", want, out)
		}
	}
}

func TestNavigationAndScreens(t *testing.T) {
	var m tea.Model = New(nil, "")
	m, _ = m.Update(statusMsg{status: fixture()})
	m, _ = render(t, m, 100, 30)

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRight})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if got := m.(Model); got.screen != detailScreen || got.detailID != "cpu_temp" {
		t.Fatalf("enter on second card: screen=%v id=%q", got.screen, got.detailID)
	}
	now := time.Now().Unix()
	m, _ = m.Update(historyMsg{id: "cpu_temp", rng: "15m", points: []api.Point{{T: now - 300, V: 60}, {T: now - 200, V: 85}, {T: now, V: 95}}})
	_, out := render(t, m, 100, 30)
	for _, want := range []string{"critical", "fire", "max 95°C", "Top processes"} {
		if !strings.Contains(out, want) {
			t.Errorf("detail missing %q\n%s", want, out)
		}
	}

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	_, out = render(t, m, 100, 30)
	if !strings.Contains(out, "CPU temp") || !strings.Contains(out, "of 5m") {
		t.Errorf("alerts screen missing active alerts\n%s", out)
	}

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("?")})
	_, out = render(t, m, 100, 30)
	if !strings.Contains(out, "Keys") {
		t.Errorf("help not shown\n%s", out)
	}
}
