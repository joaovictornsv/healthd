// Package tui is the interactive dashboard for healthd.
package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/joaovictornsv/healthd/internal/api"
)

const pollInterval = 2 * time.Second

type screen int

const (
	dashboardScreen screen = iota
	detailScreen
	alertsScreen
)

var ranges = []string{"15m", "1h", "24h", "7d"}

type Model struct {
	client *api.Client
	socket string
	width  int
	height int

	status *api.Status
	err    error

	screen   screen
	help     bool
	cursor   int    // selected card on the dashboard
	detailID string // indicator shown on the detail screen
	rng      string
	history  []api.Point
	events   []api.Event
	scroll   int

	snoozing   bool
	flash      string
	flashErr   bool
	flashUntil time.Time
}

func New(client *api.Client, socket string) Model {
	return Model{client: client, socket: socket, rng: "15m"}
}

func Run(client *api.Client, socket string) error {
	_, err := tea.NewProgram(New(client, socket), tea.WithAltScreen()).Run()
	return err
}

type (
	tickMsg   struct{}
	statusMsg struct {
		status *api.Status
		err    error
	}
	historyMsg struct {
		id, rng string
		points  []api.Point
	}
	eventsMsg struct {
		id     string
		events []api.Event
	}
	actionMsg struct {
		text string
		err  error
	}
)

func (m Model) Init() tea.Cmd {
	return tea.Batch(m.fetchStatus(), tick())
}

func tick() tea.Cmd {
	return tea.Tick(pollInterval, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m Model) fetchStatus() tea.Cmd {
	return func() tea.Msg {
		s, err := m.client.Status()
		return statusMsg{s, err}
	}
}

func (m Model) fetchHistory() tea.Cmd {
	id, rng := m.detailID, m.rng
	return func() tea.Msg {
		points, err := m.client.History(id, rng)
		if err != nil {
			return nil
		}
		return historyMsg{id, rng, points}
	}
}

// fetchEvents loads events for the detail indicator, or all of them on the
// alerts screen.
func (m Model) fetchEvents() tea.Cmd {
	id, limit := m.detailID, 6
	if m.screen == alertsScreen {
		id, limit = "", 200
	}
	return func() tea.Msg {
		events, err := m.client.Events(id, limit)
		if err != nil {
			return nil
		}
		return eventsMsg{id, events}
	}
}

// refresh reloads whatever the current screen shows.
func (m Model) refresh() tea.Cmd {
	cmds := []tea.Cmd{m.fetchStatus()}
	switch m.screen {
	case detailScreen:
		cmds = append(cmds, m.fetchHistory(), m.fetchEvents())
	case alertsScreen:
		cmds = append(cmds, m.fetchEvents())
	}
	return tea.Batch(cmds...)
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tickMsg:
		return m, tea.Batch(m.refresh(), tick())
	case statusMsg:
		m.err = msg.err
		if msg.err == nil {
			m.status = msg.status
			m.cursor = min(m.cursor, max(0, len(cards(m.status))-1))
		}
		return m, nil
	case historyMsg:
		if msg.id == m.detailID && msg.rng == m.rng {
			m.history = msg.points
		}
		return m, nil
	case eventsMsg:
		if (m.screen == alertsScreen && msg.id == "") || (m.screen == detailScreen && msg.id == m.detailID) {
			m.events = msg.events
		}
		return m, nil
	case actionMsg:
		m.flash, m.flashErr, m.flashUntil = msg.text, msg.err != nil, time.Now().Add(4*time.Second)
		if msg.err != nil {
			m.flash = msg.err.Error()
		}
		return m, m.refresh()
	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	if key == "ctrl+c" {
		return m, tea.Quit
	}
	if m.snoozing {
		return m.handleSnoozeKey(key)
	}
	if m.help {
		m.help = false
		return m, nil
	}

	switch key {
	case "q":
		return m, tea.Quit
	case "?":
		m.help = true
		return m, nil
	case "r":
		return m, m.reload()
	case "s":
		if len(m.snoozeTargets()) > 0 {
			m.snoozing = true
		}
		return m, nil
	case "a":
		if m.screen != alertsScreen {
			m.screen, m.scroll, m.events = alertsScreen, 0, nil
			return m, m.refresh()
		}
	case "esc", "backspace":
		if m.screen != dashboardScreen {
			m.screen = dashboardScreen
			return m, nil
		}
	}

	switch m.screen {
	case dashboardScreen:
		return m.dashboardKey(key)
	case detailScreen:
		return m.detailKey(key)
	case alertsScreen:
		switch key {
		case "up", "k":
			m.scroll = max(0, m.scroll-1)
		case "down", "j":
			m.scroll++
		}
	}
	return m, nil
}

func (m Model) dashboardKey(key string) (tea.Model, tea.Cmd) {
	cs := cards(m.status)
	if len(cs) == 0 {
		return m, nil
	}
	cols := m.columns()
	switch key {
	case "left", "h":
		m.cursor = max(0, m.cursor-1)
	case "right", "l":
		m.cursor = min(len(cs)-1, m.cursor+1)
	case "up", "k":
		if m.cursor-cols >= 0 {
			m.cursor -= cols
		}
	case "down", "j":
		if m.cursor+cols < len(cs) {
			m.cursor += cols
		}
	case "enter":
		return m.openDetail(worst(cs[m.cursor].inds).ID)
	}
	return m, nil
}

func (m Model) detailKey(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "left", "h", "right", "l":
		ids := m.indicatorIDs()
		i := indexOf(ids, m.detailID)
		if key == "left" || key == "h" {
			i = (i - 1 + len(ids)) % len(ids)
		} else {
			i = (i + 1) % len(ids)
		}
		return m.openDetail(ids[i])
	case "1", "2", "3", "4":
		m.rng = ranges[key[0]-'1']
		m.history = nil
		return m, m.fetchHistory()
	}
	return m, nil
}

func (m Model) openDetail(id string) (tea.Model, tea.Cmd) {
	m.screen, m.detailID, m.history, m.events = detailScreen, id, nil, nil
	return m, m.refresh()
}

func (m Model) indicatorIDs() []string {
	var ids []string
	if m.status != nil {
		for _, ind := range m.status.Indicators {
			ids = append(ids, ind.ID)
		}
	}
	return ids
}

func indexOf(ids []string, id string) int {
	for i, v := range ids {
		if v == id {
			return i
		}
	}
	return 0
}

// snoozeTargets are the indicators a snooze applies to: the whole selected
// card on the dashboard, or the indicator being viewed.
func (m Model) snoozeTargets() []api.Indicator {
	switch m.screen {
	case dashboardScreen:
		if cs := cards(m.status); m.cursor < len(cs) {
			return cs[m.cursor].inds
		}
	case detailScreen:
		if ind, ok := m.indicator(m.detailID); ok {
			return []api.Indicator{ind}
		}
	}
	return nil
}

func (m Model) handleSnoozeKey(key string) (tea.Model, tea.Cmd) {
	m.snoozing = false
	var duration string
	switch key {
	case "1":
		duration = "30m"
	case "2":
		duration = "1h"
	case "3":
		duration = api.UntilResolved
	case "u":
		duration = ""
	default:
		return m, nil
	}
	targets := m.snoozeTargets()
	client := m.client
	return m, func() tea.Msg {
		for _, ind := range targets {
			var err error
			if duration == "" {
				err = client.Unsnooze(ind.ID)
			} else {
				err = client.Snooze(ind.ID, duration)
			}
			if err != nil {
				return actionMsg{err: err}
			}
		}
		name := targets[0].Name
		if duration == "" {
			return actionMsg{text: name + " unsnoozed"}
		}
		return actionMsg{text: fmt.Sprintf("%s snoozed (%s)", name, strings.ReplaceAll(duration, "_", " "))}
	}
}

func (m Model) reload() tea.Cmd {
	client := m.client
	return func() tea.Msg {
		if err := client.Reload(); err != nil {
			return actionMsg{err: err}
		}
		return actionMsg{text: "config reloaded"}
	}
}

func (m Model) indicator(id string) (api.Indicator, bool) {
	if m.status != nil {
		for _, ind := range m.status.Indicators {
			if ind.ID == id {
				return ind, true
			}
		}
	}
	return api.Indicator{}, false
}

func (m Model) View() string {
	if m.width == 0 {
		return ""
	}
	if m.status == nil {
		return m.waitingView()
	}
	var body string
	switch {
	case m.help:
		body = helpView()
	case m.screen == detailScreen:
		body = m.detailView()
	case m.screen == alertsScreen:
		body = m.alertsView()
	default:
		body = m.dashboardView()
	}
	header, footer := m.header(), m.footer()
	bodyHeight := m.height - lipgloss.Height(header) - lipgloss.Height(footer)
	body = lipgloss.NewStyle().Height(bodyHeight).MaxHeight(bodyHeight).Render(body)
	return lipgloss.JoinVertical(lipgloss.Left, header, body, footer)
}

func (m Model) waitingView() string {
	msg := "Connecting to healthd…"
	if m.err != nil {
		msg = fmt.Sprintf("Cannot reach healthd at %s\n\nStart it with:  healthd\nor install it:  healthd install-service\n\n%s",
			m.socket, muted.Render("retrying every 2s · q to quit"))
	}
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colorMuted).Padding(1, 3).Render(msg)
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box)
}

func (m Model) header() string {
	s := m.status
	left := " " + bold.Render("health") + muted.Render(fmt.Sprintf(" · %s · up %s", s.Host, human(time.Duration(s.UptimeSeconds)*time.Second)))

	var firing, pending int
	for _, ind := range s.Indicators {
		switch levelOf(ind) {
		case levelFiring, levelCritical:
			firing++
		case levelPending:
			pending++
		}
	}
	var parts []string
	if firing > 0 {
		parts = append(parts, lipgloss.NewStyle().Foreground(colorFiring).Bold(true).Render(fmt.Sprintf("● %d firing", firing)))
	}
	if pending > 0 {
		parts = append(parts, lipgloss.NewStyle().Foreground(colorPending).Render(fmt.Sprintf("◐ %d pending", pending)))
	}
	if firing == 0 && pending == 0 {
		parts = append(parts, lipgloss.NewStyle().Foreground(colorOK).Render("● all good"))
	}
	updated := muted.Render("updated " + human(time.Since(s.SampledAt)) + " ago")
	if m.err != nil {
		updated = lipgloss.NewStyle().Foreground(colorFiring).Render("healthd unreachable")
	}
	parts = append(parts, updated)
	return spread(left, strings.Join(parts, "  ")+" ", m.width)
}

func (m Model) footer() string {
	if m.snoozing {
		name := m.snoozeTargets()[0].Name
		return " " + bold.Render("Snooze "+name+":") + "  1 30m · 2 1h · 3 until resolved · u unsnooze · esc cancel"
	}
	if m.flash != "" && time.Now().Before(m.flashUntil) {
		style := lipgloss.NewStyle().Foreground(colorOK)
		if m.flashErr {
			style = lipgloss.NewStyle().Foreground(colorFiring)
		}
		return " " + style.Render(m.flash)
	}
	var keys string
	switch m.screen {
	case detailScreen:
		keys = "esc back   1-4 range   ←→ prev/next   s snooze   a alerts   ? help   q quit"
	case alertsScreen:
		keys = "esc back   ↑↓ scroll   r reload   ? help   q quit"
	default:
		keys = "←↑↓→ move   enter details   a alerts   s snooze   r reload   ? help   q quit"
	}
	return muted.Render(" " + truncate(keys, m.width-1))
}

func helpView() string {
	rows := [][2]string{
		{"←↑↓→ / hjkl", "move between cards"},
		{"enter", "open detail"},
		{"esc", "back"},
		{"1 – 4", "chart range: 15m, 1h, 24h, 7d (detail)"},
		{"←→ / hl", "previous / next indicator (detail)"},
		{"a", "alerts screen"},
		{"s", "snooze notifications for the selected card"},
		{"r", "reload config and re-detect hardware"},
		{"?", "this help"},
		{"q", "quit"},
	}
	var b strings.Builder
	b.WriteString(bold.Render("Keys") + "\n\n")
	for _, r := range rows {
		b.WriteString(fmt.Sprintf("  %-14s %s\n", accent.Render(r[0]), r[1]))
	}
	b.WriteString("\n" + muted.Render("press any key to close"))
	return lipgloss.NewStyle().Padding(1, 2).Render(b.String())
}
