package daemon

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/joaovictornsv/healthd/internal/api"
	"github.com/joaovictornsv/healthd/internal/config"
	"github.com/joaovictornsv/healthd/internal/metrics"
	"github.com/joaovictornsv/healthd/internal/notify"
)

type fakeMeta map[string]string

func (m fakeMeta) Meta(key string) (string, error) { return m[key], nil }
func (m fakeMeta) SetMeta(key, value string) error { m[key] = value; return nil }

// sink records what the boot check delivers.
type sink struct {
	sent   []notify.Message
	events []api.Event
	fail   error
}

func (s *sink) send(m notify.Message) error {
	if s.fail != nil {
		return s.fail
	}
	s.sent = append(s.sent, m)
	return nil
}

func (s *sink) addEvent(e api.Event) error {
	s.events = append(s.events, e)
	return nil
}

var (
	bootStart = time.Date(2026, 10, 30, 8, 48, 35, 0, time.Local)
	burstAt   = time.Date(2026, 10, 30, 8, 43, 6, 0, time.Local)
	summaryOn = bootOptions{summary: true, delay: 3 * time.Minute, notifyClean: true}
)

func crashedBacklog() metrics.Backlog {
	return metrics.Backlog{
		Done:    true,
		Current: metrics.BootFindings{BootID: "current", Offset: 0, Scanned: true},
		Previous: &metrics.BootFindings{
			BootID: "crashed", Offset: -1, Scanned: true, Count: 1536,
			Groups: map[string]metrics.GroupFindings{metrics.GroupPCIe: {
				Count: 1536, First: burstAt, Last: burstAt.Add(time.Second),
				Sample: "pcieport 0000:00:06.2: AER: Correctable error message received from 0000:03:00.0",
			}},
		},
	}
}

func cleanBacklog() metrics.Backlog {
	return metrics.Backlog{
		Done:     true,
		Current:  metrics.BootFindings{BootID: "current", Scanned: true},
		Previous: &metrics.BootFindings{BootID: "previous", Offset: -1, Scanned: true},
	}
}

func TestBootSummaryWaitsForDelay(t *testing.T) {
	b, s := newBootCheck(fakeMeta{}, bootStart), &sink{}
	b.check(bootStart.Add(10*time.Second), summaryOn, crashedBacklog(), s.send, s.addEvent)
	b.check(bootStart.Add(2*time.Minute+59*time.Second), summaryOn, crashedBacklog(), s.send, s.addEvent)
	if len(s.sent) != 0 {
		t.Fatalf("sent before the delay: %+v", s.sent)
	}
	if _, due := b.view("current"); !due.Equal(bootStart.Add(3 * time.Minute)) {
		t.Errorf("due = %v", due)
	}
	b.check(bootStart.Add(3*time.Minute), summaryOn, crashedBacklog(), s.send, s.addEvent)
	if len(s.sent) != 1 {
		t.Fatalf("sent %d after the delay, want 1", len(s.sent))
	}
}

func TestBootSummaryWaitsForScan(t *testing.T) {
	b, s := newBootCheck(fakeMeta{}, bootStart), &sink{}
	bl := crashedBacklog()
	bl.Done = false
	b.check(bootStart.Add(time.Hour), summaryOn, bl, s.send, s.addEvent)
	if len(s.sent) != 0 {
		t.Fatalf("sent before the scan finished: %+v", s.sent)
	}
}

func TestBootSummaryFound(t *testing.T) {
	b, s := newBootCheck(fakeMeta{}, bootStart), &sink{}
	b.check(bootStart.Add(3*time.Minute), summaryOn, crashedBacklog(), s.send, s.addEvent)
	if len(s.sent) != 1 {
		t.Fatalf("sent %d, want 1", len(s.sent))
	}
	m := s.sent[0]
	if m.Urgency != notify.Critical || m.Sound != notify.SoundCritical || !strings.Contains(m.Title, "kernel errors found") {
		t.Errorf("message = %+v", m)
	}
	for _, want := range []string{
		"Previous boot: 1,536 PCIe link errors (first at 08:43:06)",
		"This boot: no errors",
		"journalctl -k -b -1",
		"kernel view in health",
	} {
		if !strings.Contains(m.Body, want) {
			t.Errorf("body missing %q:\n%s", want, m.Body)
		}
	}
	if len(s.events) != 1 || s.events[0].Kind != "boot" || !s.events[0].Critical || s.events[0].Value != 1536 ||
		strings.Contains(s.events[0].Message, "Details") {
		t.Errorf("events = %+v", s.events)
	}
	if got, _ := b.view("current"); got == nil || !got.Sent || !got.Found {
		t.Errorf("view = %+v", got)
	}
}

func TestBootSummaryClean(t *testing.T) {
	b, s := newBootCheck(fakeMeta{}, bootStart), &sink{}
	b.check(bootStart.Add(3*time.Minute), summaryOn, cleanBacklog(), s.send, s.addEvent)
	if len(s.sent) != 1 {
		t.Fatalf("sent %d, want 1", len(s.sent))
	}
	m := s.sent[0]
	if m.Urgency != notify.Low || m.Sound != "" || !strings.Contains(m.Title, "all good") ||
		!strings.Contains(m.Body, "Previous boot: no errors") || !strings.Contains(m.Body, "This boot: no errors") {
		t.Errorf("message = %+v", m)
	}
	if len(s.events) != 0 {
		t.Errorf("a clean check shouldn't add an alert event: %+v", s.events)
	}
}

func TestBootSummaryCleanSilenced(t *testing.T) {
	meta := fakeMeta{}
	b, s := newBootCheck(meta, bootStart), &sink{}
	opts := summaryOn
	opts.notifyClean = false
	b.check(bootStart.Add(3*time.Minute), opts, cleanBacklog(), s.send, s.addEvent)
	if len(s.sent) != 0 {
		t.Fatalf("sent %+v with notify_when_clean off", s.sent)
	}
	// Still done for this boot, and visible in the API.
	got, _ := b.view("current")
	if got == nil || got.Sent || got.Found || !strings.Contains(got.Note, "notify_when_clean") {
		t.Errorf("view = %+v", got)
	}
	// Errors are still reported with notify_when_clean off.
	b2, s2 := newBootCheck(fakeMeta{}, bootStart), &sink{}
	b2.check(bootStart.Add(3*time.Minute), opts, crashedBacklog(), s2.send, s2.addEvent)
	if len(s2.sent) != 1 {
		t.Errorf("sent %d, want 1", len(s2.sent))
	}
}

func TestBootSummaryOncePerBoot(t *testing.T) {
	meta := fakeMeta{}
	s := &sink{}
	b := newBootCheck(meta, bootStart)
	b.check(bootStart.Add(3*time.Minute), summaryOn, crashedBacklog(), s.send, s.addEvent)
	b.check(bootStart.Add(10*time.Minute), summaryOn, crashedBacklog(), s.send, s.addEvent)
	if len(s.sent) != 1 {
		t.Fatalf("sent %d in one run, want 1", len(s.sent))
	}

	// A daemon restart in the same boot loads the state from the database.
	restarted := bootStart.Add(time.Hour)
	b = newBootCheck(meta, restarted)
	b.check(restarted.Add(5*time.Minute), summaryOn, crashedBacklog(), s.send, s.addEvent)
	if len(s.sent) != 1 || len(s.events) != 1 {
		t.Fatalf("restart re-sent: %d sent, %d events", len(s.sent), len(s.events))
	}
	if got, _ := b.view("current"); got == nil || !got.Sent {
		t.Errorf("restarted view = %+v", got)
	}

	// The next boot gets its own summary.
	next := crashedBacklog()
	next.Current.BootID = "next"
	next.Previous = &metrics.BootFindings{BootID: "current", Offset: -1, Scanned: true}
	b = newBootCheck(meta, restarted)
	b.check(restarted.Add(5*time.Minute), summaryOn, next, s.send, s.addEvent)
	if len(s.sent) != 2 || !strings.Contains(s.sent[1].Title, "all good") {
		t.Fatalf("next boot: %+v", s.sent)
	}
}

func TestBootSummaryNotifyDisabled(t *testing.T) {
	meta := fakeMeta{}
	s := &sink{}
	b := newBootCheck(meta, bootStart)
	b.check(bootStart.Add(3*time.Minute), summaryOn, crashedBacklog(), nil, s.addEvent)
	got, _ := b.view("current")
	if got == nil || got.Sent || got.Note != "notifications are disabled" {
		t.Fatalf("view = %+v", got)
	}
	// The finding is still recorded in the history.
	if len(s.events) != 1 {
		t.Errorf("events = %+v", s.events)
	}
	// Turning notifications back on later in the boot doesn't send it.
	b.check(bootStart.Add(5*time.Minute), summaryOn, crashedBacklog(), s.send, s.addEvent)
	if len(s.sent) != 0 {
		t.Errorf("sent after re-enabling: %+v", s.sent)
	}
}

func TestBootSummaryRetriesFailedSend(t *testing.T) {
	b, s := newBootCheck(fakeMeta{}, bootStart), &sink{fail: errors.New("no notification service")}
	at := bootStart.Add(3 * time.Minute)
	b.check(at, summaryOn, crashedBacklog(), s.send, s.addEvent)
	s.fail = nil
	b.check(at.Add(30*time.Second), summaryOn, crashedBacklog(), s.send, s.addEvent)
	if len(s.sent) != 0 {
		t.Fatal("retried too soon")
	}
	b.check(at.Add(time.Minute), summaryOn, crashedBacklog(), s.send, s.addEvent)
	if len(s.sent) != 1 || len(s.events) != 1 {
		t.Fatalf("after retry: %d sent, %d events", len(s.sent), len(s.events))
	}
}

func TestBootSummaryFromKmsg(t *testing.T) {
	b, s := newBootCheck(fakeMeta{}, bootStart), &sink{}
	bl := cleanBacklog()
	bl.Previous = &metrics.BootFindings{Unavailable: "reading /dev/kmsg, which only has the current boot"}
	b.check(bootStart.Add(3*time.Minute), summaryOn, bl, s.send, s.addEvent)
	if len(s.sent) != 1 || !strings.Contains(s.sent[0].Body, "Previous boot: not checked (reading /dev/kmsg") ||
		!strings.Contains(s.sent[0].Title, "all good") {
		t.Fatalf("sent = %+v", s.sent)
	}
}

func TestBootSummaryNoPreviousBoot(t *testing.T) {
	b, s := newBootCheck(fakeMeta{}, bootStart), &sink{}
	bl := cleanBacklog()
	bl.Previous = nil
	bl.Current.Count = 3
	bl.Current.Groups = map[string]metrics.GroupFindings{metrics.GroupKernel: {Count: 3, First: bootStart.Add(-time.Minute)}}
	b.check(bootStart.Add(3*time.Minute), summaryOn, bl, s.send, s.addEvent)
	if len(s.sent) != 1 || strings.Contains(s.sent[0].Body, "Previous boot") ||
		!strings.Contains(s.sent[0].Body, "This boot: 3 kernel errors (first at 08:47:35)") ||
		!strings.Contains(s.sent[0].Body, "journalctl -k -b 0") {
		t.Fatalf("sent = %+v", s.sent)
	}
}

func TestBootSummaryGroups(t *testing.T) {
	b, s := newBootCheck(fakeMeta{}, bootStart), &sink{}
	bl := crashedBacklog()
	bl.Previous.Count++
	bl.Previous.Groups[metrics.GroupKernel] = metrics.GroupFindings{Count: 1, First: burstAt.Add(-time.Minute), Sample: "x"}
	b.check(bootStart.Add(3*time.Minute), summaryOn, bl, s.send, s.addEvent)
	want := "Previous boot: 1,536 PCIe link errors (first at 08:43:06), 1 kernel error (first at 08:42:06)"
	if len(s.sent) != 1 || !strings.Contains(s.sent[0].Body, want) {
		t.Fatalf("want %q in %+v", want, s.sent)
	}
}

func TestPreviousBootAlertWithoutSummary(t *testing.T) {
	meta := fakeMeta{}
	s := &sink{}
	off := bootOptions{summary: false}
	b := newBootCheck(meta, bootStart)
	// Sent as soon as the scan is done, without waiting for a delay.
	b.check(bootStart.Add(10*time.Second), off, crashedBacklog(), s.send, s.addEvent)
	b.check(bootStart.Add(20*time.Minute), off, crashedBacklog(), s.send, s.addEvent)
	if len(s.sent) != 1 || !strings.Contains(s.sent[0].Body, "Previous boot: 1,536 PCIe link errors (first at 08:43:06)") ||
		s.sent[0].Urgency != notify.Critical {
		t.Fatalf("sent = %+v", s.sent)
	}
	if len(s.events) != 1 {
		t.Errorf("events = %+v", s.events)
	}
	// Not again after a restart in the same boot.
	b = newBootCheck(meta, bootStart.Add(time.Hour))
	b.check(bootStart.Add(time.Hour), off, crashedBacklog(), s.send, s.addEvent)
	if len(s.sent) != 1 {
		t.Fatalf("re-sent after restart: %+v", s.sent)
	}
	// Errors from before login in this boot don't alert without the summary.
	b2, s2 := newBootCheck(fakeMeta{}, bootStart), &sink{}
	bl := cleanBacklog()
	bl.Current.Count = 5
	bl.Current.Groups = map[string]metrics.GroupFindings{metrics.GroupKernel: {Count: 5, First: bootStart}}
	b2.check(bootStart.Add(time.Hour), off, bl, s2.send, s2.addEvent)
	if len(s2.sent) != 0 {
		t.Errorf("sent = %+v", s2.sent)
	}
}

func TestSummaryCountsAsPreviousBootReport(t *testing.T) {
	meta := fakeMeta{}
	s := &sink{}
	b := newBootCheck(meta, bootStart)
	b.check(bootStart.Add(3*time.Minute), summaryOn, crashedBacklog(), s.send, s.addEvent)
	// Turning the summary off and reloading must not alert again about the
	// boot the summary already covered.
	b.check(bootStart.Add(4*time.Minute), bootOptions{}, crashedBacklog(), s.send, s.addEvent)
	if len(s.sent) != 1 || len(s.events) != 1 {
		t.Fatalf("%d sent, %d events; want 1 each", len(s.sent), len(s.events))
	}
}

func TestKernelPatterns(t *testing.T) {
	k := config.Kernel{
		ExtraPatterns:   []string{"amdgpu.*hang"},
		ExtraPatternsAt: map[string][]string{"warning": {"i915.*underrun"}},
	}
	patterns, err := kernelPatterns(k)
	if err != nil {
		t.Fatal(err)
	}
	if patterns[0].Group != metrics.GroupPCIe || patterns[0].Priority != metrics.PrioInfo {
		t.Errorf("PCIe patterns should come first, at info: %+v", patterns[0])
	}
	last := patterns[len(patterns)-1]
	if last.Expr != "i915.*underrun" || last.Priority != metrics.PrioWarning {
		t.Errorf("extra_patterns_at = %+v", last)
	}
	for _, p := range patterns {
		if p.Group == metrics.GroupKernel && p.Expr != "i915.*underrun" && p.Priority != metrics.PrioErr {
			t.Errorf("%q at priority %d, want err", p.Expr, p.Priority)
		}
	}

	off := false
	k.PCIeErrors = &off
	patterns, _ = kernelPatterns(k)
	for _, p := range patterns {
		if p.Group == metrics.GroupPCIe {
			t.Errorf("pcie_errors = false still has %q", p.Expr)
		}
	}

	k.ExtraPatternsAt = map[string][]string{"warn": {"x"}}
	if _, err := kernelPatterns(k); err == nil || !strings.Contains(err.Error(), "extra_patterns_at") {
		t.Errorf("err = %v, want an extra_patterns_at error", err)
	}
}
