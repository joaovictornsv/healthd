package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseJournalLine(t *testing.T) {
	line := []byte(`{"MESSAGE":"nvme nvme0: I/O 12 QID 3 timeout, reset controller","__REALTIME_TIMESTAMP":"1792942954349008","PRIORITY":"3","__CURSOR":"s=1;i=2"}`)
	e, ok := parseJournalLine(line)
	if !ok || e.Message != "nvme nvme0: I/O 12 QID 3 timeout, reset controller" || e.Time.UnixMicro() != 1792942954349008 ||
		e.Priority != 3 || e.Cursor != "s=1;i=2" {
		t.Fatalf("got %+v %v", e, ok)
	}
	// Non-UTF-8 messages are encoded as byte arrays; they are skipped.
	if _, ok := parseJournalLine([]byte(`{"MESSAGE":[104,105]}`)); ok {
		t.Fatal("expected byte-array MESSAGE to be skipped")
	}
}

func TestParsePriority(t *testing.T) {
	for in, want := range map[string]int{"err": 3, "warning": 4, "info": 6, "7": 7} {
		if got, err := ParsePriority(in); err != nil || got != want {
			t.Errorf("ParsePriority(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	if _, err := ParsePriority("warn"); err == nil {
		t.Error("expected an error for an unknown priority name")
	}
}

// testPatterns mirrors the daemon's defaults: PCIe down to info, the rest
// at err.
func testPatterns() []KernelPattern {
	var out []KernelPattern
	for _, e := range PCIePatterns {
		out = append(out, KernelPattern{Expr: e, Priority: PrioInfo, Group: GroupPCIe})
	}
	for _, e := range DefaultKernelPatterns {
		out = append(out, KernelPattern{Expr: e, Priority: PrioErr, Group: GroupKernel})
	}
	return out
}

func newTestWatcher(t *testing.T, j Journal) *KernelWatcher {
	t.Helper()
	k := NewKernelWatcher(j)
	k.bootID = "current"
	if err := k.SetPatterns(testPatterns(), DefaultKernelIgnore); err != nil {
		t.Fatal(err)
	}
	return k
}

func TestKernelWatcherMatchesPatterns(t *testing.T) {
	k := newTestWatcher(t, nil)
	now := time.Now()
	// Messages arrive in time order, oldest first.
	k.handle(now.Add(-10*time.Minute), PrioErr, "EXT4-fs error (device nvme0n1p2): old")
	for _, msg := range []string{
		"Out of memory: Killed process 1234 (node)",
		"ata1.00: failed command: READ FPDMA QUEUED",
		"BTRFS error (device sda1): bdev /dev/sda1 errs: wr 0, rd 1",
		"usb 1-2: device descriptor read/64, error -71",   // not a pattern
		"ACPI BIOS Error (bug): Could not resolve symbol", // not a pattern
	} {
		// Spaced out so each is its own burst.
		now = now.Add(burstGap)
		k.handle(now, PrioErr, msg)
	}

	if got := k.Count(5 * time.Minute); got != 3 {
		t.Fatalf("Count(5m) = %d, want 3", got)
	}
	if got := k.Count(time.Hour); got != 4 {
		t.Fatalf("Count(1h) = %d, want 4", got)
	}
	if recent := k.Recent(time.Hour, 2); len(recent) != 2 || recent[0].Message != "BTRFS error (device sda1): bdev /dev/sda1 errs: wr 0, rd 1" {
		t.Fatalf("Recent = %+v", recent)
	}
}

func TestPriorityFiltering(t *testing.T) {
	k := newTestWatcher(t, nil)
	now := time.Now()
	cases := []struct {
		prio  int
		msg   string
		group string // "" = not matched
	}{
		// PCIe patterns apply down to info.
		{PrioWarning, "nvme 0000:03:00.0: PCIe Bus Error: severity=Correctable, type=Physical Layer, (Receiver ID)", GroupPCIe},
		{PrioInfo, "pcieport 0000:00:06.2: AER: Multiple Correctable error message received from 0000:03:00.0", GroupPCIe},
		{PrioDebug, "pcieport 0000:00:06.2: AER: Correctable error message received from 0000:03:00.0", ""},
		// The built-in patterns stay at err: the same text at warning or
		// info doesn't match.
		{PrioErr, "EXT4-fs error (device sda1): bad block", GroupKernel},
		{PrioWarning, "EXT4-fs error (device sda1): bad block", ""},
		{PrioInfo, "thermal_sys: Registered thermal governor 'step_wise'", ""},
		// An AER line that also matches the nvme pattern counts as PCIe.
		{PrioErr, "nvme 0000:03:00.0: AER:   Error of this Agent is reported first", GroupPCIe},
	}
	for _, c := range cases {
		m, _ := k.snapshot()
		group, ok := m.match(c.prio, c.msg)
		if (c.group != "") != ok || group != c.group {
			t.Errorf("prio %d %q: got group %q (matched %v), want %q", c.prio, c.msg, group, ok, c.group)
		}
		k.handle(now, c.prio, c.msg)
	}
	if _, maxPrio := k.snapshot(); maxPrio != PrioInfo {
		t.Errorf("maxPrio = %d, want info (%d)", maxPrio, PrioInfo)
	}
}

func TestPCIePatternScope(t *testing.T) {
	k := newTestWatcher(t, nil)
	m, _ := k.snapshot()
	matches := []string{
		"nvme 0000:03:00.0: PCIe Bus Error: severity=Correctable, type=Physical Layer, (Receiver ID)",
		"nvme 0000:03:00.0:    [ 0] RxErr                  (First)",
		"pcieport 0000:00:06.2:    [12] Timeout               ",
		"pcieport 0000:00:1c.0:    [ 6] BadTLP                ",
		"pcieport 0000:00:1c.0:    [ 7] BadDLLP               ",
		"pcieport 0000:00:06.2: AER: Correctable error message received from 0000:03:00.0",
		"pcieport 0000:00:06.2: AER: Multiple Correctable error message received from 0000:03:00.0",
		"pcieport 0000:00:1c.0: AER: Uncorrected (Non-Fatal) error message received from 0000:00:1c.0",
	}
	for _, msg := range matches {
		if g, ok := m.match(PrioWarning, msg); !ok || g != GroupPCIe {
			t.Errorf("%q: not matched as PCIe (group %q)", msg, g)
		}
	}
	// Lines seen on every normal boot, and lookalikes.
	noise := []string{
		"pci 0000:00:06.0: retraining failed",
		"pcieport 0000:00:06.2: AER: enabled with IRQ 122",
		"acpi PNP0A08:00: _OSC: OS now controls [PCIeHotplug SHPCHotplug PME AER PCIeCapability LTR DPC]",
		"pci 0000:00:06.0: [8086:464d] type 01 class 0x060400 PCIe Root Port",
		"usb 3-2: Timeout while waiting for setup device command",
		"ata2: SATA link down (SStatus 4 SControl 300)",
	}
	for _, msg := range noise {
		if g, ok := m.match(PrioInfo, msg); ok {
			t.Errorf("%q: matched as %q, want no match", msg, g)
		}
	}
}

func TestIgnorePatterns(t *testing.T) {
	k := newTestWatcher(t, nil)
	if err := k.SetPatterns(testPatterns(), append(DefaultKernelIgnore, `amdgpu.*benign`)); err != nil {
		t.Fatal(err)
	}
	m, _ := k.snapshot()
	for _, msg := range []string{
		"proc_thermal_pci 0000:00:04.0: error: proc_thermal_add, will continue",
		"amdgpu 0000:05:00.0: thermal benign notice",
	} {
		if g, ok := m.match(PrioErr, msg); ok {
			t.Errorf("%q matched as %q, want it ignored", msg, g)
		}
	}
	if _, ok := m.match(PrioErr, "thermal thermal_zone0: critical temperature reached"); !ok {
		t.Error("ignore list hid an unrelated thermal error")
	}
}

func TestBurstGrouping(t *testing.T) {
	k := newTestWatcher(t, nil)
	start := time.Now().Add(-time.Minute)
	// 1,500 lines in about a second, alternating like a real AER burst,
	// with an unrelated error in the middle.
	for i := 0; i < 1500; i++ {
		ts := start.Add(time.Duration(i) * time.Millisecond)
		switch i % 3 {
		case 0:
			k.handle(ts, PrioInfo, "pcieport 0000:00:06.2: AER: Correctable error message received from 0000:03:00.0")
		case 1:
			k.handle(ts, PrioWarning, "nvme 0000:03:00.0: PCIe Bus Error: severity=Correctable, type=Physical Layer, (Receiver ID)")
		case 2:
			k.handle(ts, PrioWarning, "nvme 0000:03:00.0:    [ 0] RxErr                  (First)")
		}
		if i == 700 {
			k.handle(ts, PrioErr, "Out of memory: Killed process 1234 (node)")
		}
	}
	// A second burst after a quiet gap.
	k.handle(start.Add(10*time.Second), PrioWarning, "nvme 0000:03:00.0:    [ 0] RxErr                  (First)")

	recent := k.Recent(time.Hour, 10)
	if len(recent) != 3 {
		t.Fatalf("got %d bursts, want 3: %+v", len(recent), recent)
	}
	first := recent[2]
	if first.Count != 1500 || first.Group != GroupPCIe || !strings.Contains(first.Message, "AER: Correctable") {
		t.Errorf("first burst = %+v", first)
	}
	if recent[1].Group != GroupKernel || recent[1].Count != 1 || recent[0].Count != 1 {
		t.Errorf("bursts = %+v", recent)
	}
	// The count isn't capped by maxKernelEvents: that limits bursts.
	if got := k.Count(time.Hour); got != 1502 {
		t.Errorf("Count = %d, want 1502", got)
	}
	if b := k.Backlog(); b.Current.Count != 1502 || b.Current.Groups[GroupPCIe].Count != 1501 {
		t.Errorf("current boot = %+v", b.Current)
	}
}

func TestBurstIsBounded(t *testing.T) {
	var events []KernelEvent
	start := time.Now()
	// One line a second for two minutes: two bursts, not one.
	for i := 0; i < 120; i++ {
		events = addToBurst(events, start.Add(time.Duration(i)*time.Second), GroupPCIe, "x")
	}
	if len(events) != 2 || events[0].Count != 60 || events[1].Count != 60 {
		t.Fatalf("got %+v", events)
	}
}

func TestParseKmsg(t *testing.T) {
	rec, ok := parseKmsg([]byte("4,1234,5678901,-;nvme 0000:03:00.0: PCIe Bus Error: severity=Correctable\n SUBSYSTEM=pci\n"))
	if !ok || rec.prio != 4 || rec.seq != 1234 || rec.usec != 5678901 || rec.msg != "nvme 0000:03:00.0: PCIe Bus Error: severity=Correctable" {
		t.Fatalf("got %+v %v", rec, ok)
	}
	// The facility is in the upper bits of the priority field.
	if rec, ok := parseKmsg([]byte("30,1,2,-;systemd[1]: hi")); !ok || rec.prio != 6 {
		t.Fatalf("got %+v %v", rec, ok)
	}
}

func TestParseBoots(t *testing.T) {
	text := []byte(" -1 bc61a82bec394363b6bac359e8f1c1d4 Fri 2026-10-30 08:42:11 -03 Fri 2026-10-30 08:43:31 -03\n" +
		"  0 0c6f410926124b8c8b3edf2174220fb1 Fri 2026-10-30 08:47:49 -03 Fri 2026-10-30 09:29:20 -03\n")
	boots := parseBoots(text)
	if len(boots) != 2 || boots[0].ID != "bc61a82bec394363b6bac359e8f1c1d4" {
		t.Fatalf("text: %+v", boots)
	}
	boots = parseBoots([]byte(`[{"index":-1,"boot_id":"a","first_entry":1000000,"last_entry":2000000},{"index":0,"boot_id":"b","first_entry":3000000,"last_entry":4000000}]`))
	if len(boots) != 2 || boots[1].ID != "b" || boots[0].Last.Unix() != 2 {
		t.Fatalf("json: %+v", boots)
	}
	if prev, ok := previousBoot(boots, "b"); !ok || prev.ID != "a" {
		t.Errorf("previousBoot = %+v %v", prev, ok)
	}
	if _, ok := previousBoot(boots, "a"); ok {
		t.Error("the first boot has no previous boot")
	}
}

// fakeJournal answers journalctl calls from canned JSON lines. Follow calls
// ("-f") stream from a pipe the test writes to.
type fakeJournal struct {
	boots []Boot
	// entries per boot argument ("0" or an ID)
	entries map[string][]string

	mu     sync.Mutex
	calls  [][]string
	follow *io.PipeWriter
	ready  chan struct{}
}

func (f *fakeJournal) Open(ctx context.Context, args ...string) (io.ReadCloser, error) {
	f.mu.Lock()
	f.calls = append(f.calls, args)
	f.mu.Unlock()
	switch {
	case args[0] == "--list-boots":
		type boot struct {
			ID    string `json:"boot_id"`
			First int64  `json:"first_entry"`
			Last  int64  `json:"last_entry"`
		}
		var out []boot
		for _, b := range f.boots {
			out = append(out, boot{b.ID, b.First.UnixMicro(), b.Last.UnixMicro()})
		}
		data, _ := json.Marshal(out)
		return io.NopCloser(strings.NewReader(string(data))), nil
	case hasArg(args, "-f"):
		r, w := io.Pipe()
		go func() {
			<-ctx.Done()
			w.Close()
		}()
		f.mu.Lock()
		f.follow = w
		f.mu.Unlock()
		close(f.ready)
		return r, nil
	}
	boot := argAfter(args, "-b")
	return io.NopCloser(strings.NewReader(strings.Join(f.entries[boot], "\n"))), nil
}

func hasArg(args []string, a string) bool { return slices.Contains(args, a) }

func argAfter(args []string, flag string) string {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

var cursorN int

func entry(t time.Time, prio int, msg string) string {
	cursorN++
	data, _ := json.Marshal(map[string]string{
		"MESSAGE": msg, "PRIORITY": fmt.Sprint(prio),
		"__REALTIME_TIMESTAMP": fmt.Sprint(t.UnixMicro()), "__CURSOR": fmt.Sprintf("c%d", cursorN),
	})
	return string(data)
}

// crashedBoot is the failing boot from the journal: a PCIe burst after
// the usual boot noise.
func crashedBoot(start time.Time) []string {
	lines := []string{
		entry(start, PrioInfo, "pci 0000:00:06.0: retraining failed"),
		entry(start, PrioInfo, "pcieport 0000:00:06.2: AER: enabled with IRQ 122"),
		entry(start, PrioErr, "ucsi_acpi USBC000:00: UCSI_GET_PDOS failed (-95)"),
		// Matches "thermal" at err, but is ignored by default.
		entry(start, PrioErr, "proc_thermal_pci 0000:00:04.0: error: proc_thermal_add, will continue"),
	}
	burst := start.Add(55 * time.Second)
	for i := 0; i < 512; i++ {
		ts := burst.Add(time.Duration(i) * time.Millisecond)
		lines = append(lines,
			entry(ts, PrioInfo, "pcieport 0000:00:06.2: AER: Correctable error message received from 0000:03:00.0"),
			entry(ts, PrioWarning, "nvme 0000:03:00.0: PCIe Bus Error: severity=Correctable, type=Physical Layer, (Receiver ID)"),
			entry(ts, PrioWarning, "nvme 0000:03:00.0:    [ 0] RxErr                  (First)"))
	}
	return lines
}

func TestBacklogScan(t *testing.T) {
	prevStart := time.Date(2026, 10, 30, 8, 42, 11, 0, time.Local)
	curStart := prevStart.Add(5 * time.Minute)
	j := &fakeJournal{
		boots: []Boot{{ID: "older"}, {ID: "crashed", First: prevStart, Last: prevStart.Add(80 * time.Second)}, {ID: "current", First: curStart}},
		entries: map[string][]string{
			"crashed": crashedBoot(prevStart),
			"0": {
				entry(curStart, PrioInfo, "pci 0000:00:06.0: retraining failed"),
				entry(curStart.Add(time.Second), PrioWarning, "nvme 0000:03:00.0:    [ 0] RxErr                  (First)"),
			},
		},
		ready: make(chan struct{}),
	}
	k := newTestWatcher(t, j)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	k.runJournal(ctx)

	b := k.Backlog()
	if !b.Done || b.Previous == nil {
		t.Fatalf("backlog = %+v", b)
	}
	prev := *b.Previous
	if prev.BootID != "crashed" || prev.Offset != -1 || prev.Count != 1536 || len(prev.Groups) != 1 || !prev.Scanned {
		t.Errorf("previous boot = %+v", prev)
	}
	g := prev.Groups[GroupPCIe]
	if g.Count != 1536 || !g.First.Equal(prevStart.Add(55*time.Second)) || !g.Last.Equal(prevStart.Add(55*time.Second+511*time.Millisecond)) ||
		!strings.Contains(g.Sample, "AER: Correctable error") {
		t.Errorf("previous boot PCIe group = %+v", g)
	}
	if b.Current.Count != 1 || b.Current.BootID != "current" {
		t.Errorf("current boot = %+v", b.Current)
	}
	// Backlog matches don't feed the live window.
	if n := k.Count(24 * 365 * time.Hour); n != 0 {
		t.Errorf("live count = %d, want 0", n)
	}
	// Scans ask journalctl for info and worse, since the PCIe patterns need it.
	if p := argAfter(j.calls[0], "-p"); p != "6" {
		t.Errorf("scan priority = %q, want 6", p)
	}

	// The follower picks up after the last scanned entry, and live matches
	// count both in the window and towards this boot.
	<-j.ready
	j.mu.Lock()
	follow := j.follow
	var args []string
	for _, a := range j.calls {
		if hasArg(a, "-f") {
			args = a
		}
	}
	j.mu.Unlock()
	if argAfter(args, "--after-cursor") == "" {
		t.Errorf("follow args = %q, want --after-cursor", args)
	}
	fmt.Fprintln(follow, entry(time.Now(), PrioWarning, "nvme 0000:03:00.0: PCIe Bus Error: severity=Correctable"))
	deadline := time.Now().Add(2 * time.Second)
	for k.Count(time.Minute) != 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := k.Count(time.Minute); n != 1 {
		t.Errorf("live count = %d, want 1", n)
	}
	if b := k.Backlog(); b.Current.Count != 2 {
		t.Errorf("current boot count = %d, want 2", b.Current.Count)
	}
}

func TestBacklogWithoutPreviousBoot(t *testing.T) {
	// A volatile journal only has this boot.
	j := &fakeJournal{boots: []Boot{{ID: "current"}}, entries: map[string][]string{}, ready: make(chan struct{})}
	k := newTestWatcher(t, j)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	k.runJournal(ctx)
	if b := k.Backlog(); !b.Done || b.Previous != nil || !b.Current.Scanned {
		t.Errorf("backlog = %+v", b)
	}
}

func TestBacklogPreviousBootOverride(t *testing.T) {
	start := time.Now().Add(-48 * time.Hour)
	j := &fakeJournal{
		boots:   []Boot{{ID: "crashed", First: start}, {ID: "clean"}, {ID: "current"}},
		entries: map[string][]string{"crashed": crashedBoot(start)},
		ready:   make(chan struct{}),
	}
	k := newTestWatcher(t, j)
	k.PreviousBoot = "crashed"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	k.runJournal(ctx)
	if b := k.Backlog(); b.Previous == nil || b.Previous.BootID != "crashed" || b.Previous.Offset != -2 || b.Previous.Count != 1536 {
		t.Errorf("previous = %+v", b.Previous)
	}
}
