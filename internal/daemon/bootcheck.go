package daemon

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/joaovictornsv/healthd/internal/api"
	"github.com/joaovictornsv/healthd/internal/metrics"
	"github.com/joaovictornsv/healthd/internal/notify"
)

// bootCheck tells the user, once per boot, what healthd found in the kernel
// messages logged before it started: the previous boot, which may have
// frozen, and this boot up to a few minutes after login.
//
// These findings don't go through the alert state machine. The kernel
// indicator counts matches in a sliding window, which a finding from before
// login has already left, and keeping an alert firing for it would repeat
// every repeat_interval for the whole boot. Instead each finding is
// delivered exactly once, keyed by boot ID in the history database:
//
//   - with the boot summary on, the summary is the only notification for
//     the backlog; it covers the previous boot and this boot so far;
//   - with it off, errors in the previous boot get one alert as soon as the
//     scan finishes, and errors from before login in this boot are only
//     shown in the TUI.
//
// Either way the finding is recorded as a "boot" event in the alert history.
// Matches after healthd starts still go through the kernel indicator.
type bootCheck struct {
	meta  metaStore
	start time.Time // when healthd started

	mu           sync.Mutex
	loaded       bool
	opts         bootOptions
	summary      *BootSummary // the last summary, possibly from an older boot
	prevReported string       // previous boot already reported, by ID
	attempts     int
	retryAt      time.Time
}

type metaStore interface {
	Meta(key string) (string, error)
	SetMeta(key, value string) error
}

type bootOptions struct {
	summary     bool
	delay       time.Duration
	notifyClean bool
}

const (
	metaBootSummary  = "boot_summary"
	metaPrevReported = "kernel_prev_boot_reported"

	summaryAttempts = 5
	summaryRetry    = time.Minute
)

// BootSummary is the outcome of the boot check, kept for the API and TUI.
type BootSummary struct {
	BootID string    `json:"boot_id"`
	Time   time.Time `json:"time"`
	Found  bool      `json:"found"`
	Title  string    `json:"title"`
	Body   string    `json:"body"`
	Sent   bool      `json:"sent"`
	// Note says why the summary wasn't sent.
	Note string `json:"note,omitempty"`
}

func newBootCheck(meta metaStore, start time.Time) *bootCheck {
	return &bootCheck{meta: meta, start: start}
}

func (b *bootCheck) load() {
	if b.loaded {
		return
	}
	b.loaded = true
	if v, err := b.meta.Meta(metaBootSummary); err != nil {
		slog.Warn("load boot summary state", "err", err)
	} else if v != "" {
		var s BootSummary
		if err := json.Unmarshal([]byte(v), &s); err == nil {
			b.summary = &s
		}
	}
	var err error
	if b.prevReported, err = b.meta.Meta(metaPrevReported); err != nil {
		slog.Warn("load boot check state", "err", err)
	}
}

// check runs after every sample. send is nil when notifications are off.
func (b *bootCheck) check(now time.Time, opts bootOptions, backlog metrics.Backlog,
	send func(notify.Message) error, addEvent func(api.Event) error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.opts = opts
	if !backlog.Done {
		return
	}
	b.load()
	prev := backlog.Previous
	prevFound := prev != nil && prev.Count > 0 && prev.Unavailable == ""

	if !opts.summary {
		if !prevFound || b.prevReported == prev.BootID {
			return
		}
		msg := previousBootMessage(*prev, now)
		if send != nil {
			if err := send(msg); err != nil {
				// Not marked as reported, so the next sample tries again.
				slog.Warn("notification failed", "err", err)
				return
			}
		}
		b.record(api.Event{Time: now, Indicator: "kernel", Kind: "boot", Critical: true,
			Value: float64(prev.Count), Message: "Previous boot: " + describe(*prev, now)}, addEvent)
		b.markPrevReported(prev.BootID)
		return
	}

	current := backlog.Current.BootID
	if b.summary != nil && b.summary.BootID == current {
		return // already done for this boot
	}
	if now.Sub(b.start) < opts.delay || now.Before(b.retryAt) {
		return
	}
	s := summarize(backlog, now)
	switch {
	case !s.Found && !opts.notifyClean:
		s.Note = "nothing found; notify_when_clean is off"
	case send == nil:
		s.Note = "notifications are disabled"
	default:
		if err := send(s.message()); err != nil {
			b.attempts++
			if b.attempts < summaryAttempts {
				slog.Warn("boot summary notification failed, retrying", "err", err, "in", summaryRetry)
				b.retryAt = now.Add(summaryRetry)
				return
			}
			s.Note = "sending failed: " + err.Error()
		} else {
			s.Sent = true
		}
	}
	slog.Info("boot check", "found", s.Found, "sent", s.Sent, "note", s.Note, "summary", strings.ReplaceAll(s.Body, "\n", " | "))
	if s.Found {
		b.record(api.Event{Time: now, Indicator: "kernel", Kind: "boot", Critical: true,
			Value: float64(totalCount(backlog)), Message: s.eventMessage()}, addEvent)
	}
	b.summary = &s
	if data, err := json.Marshal(s); err == nil {
		if err := b.meta.SetMeta(metaBootSummary, string(data)); err != nil {
			slog.Warn("save boot summary state", "err", err)
		}
	}
	// The summary covered the previous boot: don't alert about it again if
	// the summary is turned off later in this boot.
	if prevFound {
		b.markPrevReported(prev.BootID)
	}
}

func (b *bootCheck) record(e api.Event, addEvent func(api.Event) error) {
	if err := addEvent(e); err != nil {
		slog.Warn("save event", "err", err)
	}
}

func (b *bootCheck) markPrevReported(id string) {
	b.prevReported = id
	if err := b.meta.SetMeta(metaPrevReported, id); err != nil {
		slog.Warn("save boot check state", "err", err)
	}
}

// view returns the summary for this boot, if done, else when it is due.
func (b *bootCheck) view(currentBoot string) (summary *BootSummary, due time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.summary != nil && b.summary.BootID == currentBoot {
		s := *b.summary
		return &s, time.Time{}
	}
	if !b.opts.summary {
		return nil, time.Time{}
	}
	return nil, maxTime(b.start.Add(b.opts.delay), b.retryAt)
}

func summarize(bl metrics.Backlog, now time.Time) BootSummary {
	s := BootSummary{BootID: bl.Current.BootID, Time: now}
	var lines []string
	var hint string
	if p := bl.Previous; p != nil {
		lines = append(lines, "Previous boot: "+describe(*p, now))
		if p.Count > 0 && p.Unavailable == "" {
			s.Found, hint = true, journalHint(*p)
		}
	}
	lines = append(lines, "This boot: "+describe(bl.Current, now))
	if bl.Current.Count > 0 {
		s.Found = true
		if hint == "" {
			hint = "journalctl -k -b 0"
		}
	}
	if s.Found {
		s.Title = "🚨 Boot check: kernel errors found"
		lines = append(lines, fmt.Sprintf("Details: the kernel view in health, or %s", hint))
	} else {
		s.Title = "✅ Boot check: all good"
	}
	s.Body = strings.Join(lines, "\n")
	return s
}

func (s BootSummary) message() notify.Message {
	m := notify.Message{Key: "boot_check", Title: s.Title, Body: s.Body,
		Urgency: notify.Low, Icon: "dialog-information"}
	if s.Found {
		m.Urgency, m.Icon, m.Sound = notify.Critical, "dialog-error", notify.SoundCritical
	}
	return m
}

// eventMessage is the body without the "Details:" hint, on one line.
func (s BootSummary) eventMessage() string {
	lines := strings.Split(s.Body, "\n")
	if s.Found {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "; ")
}

func previousBootMessage(f metrics.BootFindings, now time.Time) notify.Message {
	sample := f.Groups[f.Sorted()[0]].Sample
	body := []string{"Previous boot: " + describe(f, now), truncate(sample, 160),
		fmt.Sprintf("Details: the kernel view in health, or %s", journalHint(f))}
	return notify.Message{Key: "boot_check", Title: "🚨 Kernel errors in the previous boot",
		Body: strings.Join(body, "\n"), Urgency: notify.Critical, Icon: "dialog-error", Sound: notify.SoundCritical}
}

// describe is one boot's result: "1,536 PCIe link errors (first at
// 08:43:06)", with one such part per pattern group.
func describe(f metrics.BootFindings, now time.Time) string {
	switch {
	case f.Unavailable != "":
		return "not checked (" + f.Unavailable + ")"
	case f.Count == 0:
		return "no errors"
	}
	var parts []string
	for _, name := range f.Sorted() {
		g := f.Groups[name]
		parts = append(parts, fmt.Sprintf("%s (first at %s)", api.KernelErrors(name, g.Count), clock(g.First, now)))
	}
	return strings.Join(parts, ", ")
}

func journalHint(f metrics.BootFindings) string {
	if f.Offset != 0 {
		return fmt.Sprintf("journalctl -k -b %d", f.Offset)
	}
	return "journalctl -k -b " + f.BootID
}

func totalCount(bl metrics.Backlog) int {
	n := bl.Current.Count
	if bl.Previous != nil {
		n += bl.Previous.Count
	}
	return n
}

// clock formats a time of day, with the weekday if it isn't today.
func clock(t, now time.Time) string {
	y1, m1, d1 := t.Date()
	y2, m2, d2 := now.Date()
	switch {
	case y1 == y2 && m1 == m2 && d1 == d2:
		return t.Format("15:04:05")
	case now.Sub(t) < 6*24*time.Hour:
		return t.Format("Mon 15:04:05")
	}
	return t.Format("Jan 02 15:04:05")
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
