package metrics

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/joaovictornsv/healthd/internal/hw"
)

// Syslog priorities, as used by the journal and /dev/kmsg. Lower is worse.
const (
	PrioErr     = 3
	PrioWarning = 4
	PrioInfo    = 6
	PrioDebug   = 7
)

var priorityNames = []string{"emerg", "alert", "crit", "err", "warning", "notice", "info", "debug"}

// ParsePriority accepts a syslog priority name ("warning") or number ("4").
func ParsePriority(s string) (int, error) {
	for i, name := range priorityNames {
		if s == name {
			return i, nil
		}
	}
	if n, err := strconv.Atoi(s); err == nil && n >= 0 && n <= PrioDebug {
		return n, nil
	}
	return 0, fmt.Errorf("unknown priority %q: use one of %s", s, strings.Join(priorityNames, ", "))
}

func PriorityName(p int) string {
	if p >= 0 && p < len(priorityNames) {
		return priorityNames[p]
	}
	return strconv.Itoa(p)
}

// Pattern groups name the kind of problem in counts and summaries; see
// api.KernelGroupLabel.
const (
	GroupKernel = "kernel"
	GroupPCIe   = "pcie"
)

// DefaultKernelPatterns are matched against error-level messages (err or
// worse).
var DefaultKernelPatterns = []string{
	`out of memory`,
	`oom-kill`,
	`i/o error`,
	`blk_update_request`,
	`nvme.*(timeout|reset|error)`,
	`ata\d+.*(failed|error|exception|hard resetting)`,
	`ext4-fs error`,
	`btrfs.*(error|corrupt)`,
	`xfs.*(error|corrupt|shutdown)`,
	`thermal`,
	`throttl`,
	`mce:`,
	`hardware error`,
}

// PCIePatterns match PCIe Advanced Error Reporting (AER) messages. The
// kernel logs even corrected link errors at warning or info, so these are
// matched down to info. The AER detail lines ("[ 0] RxErr") are matched by
// their bit-number prefix, so a word like "Timeout" only matches there and
// not in unrelated messages such as "retraining failed".
var PCIePatterns = []string{
	`PCIe Bus Error`,
	`AER: .*error`,
	`\[\s*\d+\]\s+(RxErr|BadTLP|BadDLLP|Rollover|Timeout|NonFatalErr|CorrIntErr|HeaderOF|DLP|SDES|TLP|FCP|CmpltTO|CmpltAbrt|UnxCmplt|RxOF|MalfTLP|ECRC|UnsupReq|ACSViol)\b`,
}

// DefaultKernelIgnore are known-harmless messages that a pattern would
// otherwise match. Intel laptops log this one on every boot, and it matches
// "thermal".
var DefaultKernelIgnore = []string{
	`proc_thermal_add, will continue`,
}

// KernelPattern is a regular expression matched case-insensitively against
// kernel messages at Priority or worse.
type KernelPattern struct {
	Expr     string
	Priority int
	Group    string
}

type kernelPattern struct {
	re       *regexp.Regexp
	priority int
	group    string
}

// KernelEvent is a burst of matching messages: matches of one group that
// arrive less than burstGap apart. Message is the first line of the burst.
type KernelEvent struct {
	Time    time.Time `json:"time"`
	Last    time.Time `json:"last"`
	Count   int       `json:"count"`
	Group   string    `json:"group"`
	Message string    `json:"message"`
}

// BootFindings summarises the matching messages of one boot.
type BootFindings struct {
	BootID string `json:"boot_id"`
	// Offset is the boot relative to this one, as in "journalctl -b -1", or
	// 0 if unknown.
	Offset  int       `json:"offset,omitempty"`
	Started time.Time `json:"started,omitzero"` // first journal entry
	Ended   time.Time `json:"ended,omitzero"`   // last journal entry, for a past boot
	Scanned bool      `json:"scanned"`
	// Unavailable says why the boot could not be scanned.
	Unavailable string `json:"unavailable,omitempty"`

	Count  int                      `json:"count"`
	Groups map[string]GroupFindings `json:"groups,omitempty"`
}

// GroupFindings are the matches of one pattern group in a boot.
type GroupFindings struct {
	Count  int       `json:"count"`
	First  time.Time `json:"first"`
	Last   time.Time `json:"last"`
	Sample string    `json:"sample"` // first matching line
}

func (f *BootFindings) add(t time.Time, group, msg string) {
	if f.Groups == nil {
		f.Groups = map[string]GroupFindings{}
	}
	g, ok := f.Groups[group]
	if !ok {
		g.First, g.Sample = t, msg
	}
	g.Count++
	g.Last = t
	f.Groups[group] = g
	f.Count++
}

// Sorted returns the groups with matches, most matches first.
func (f BootFindings) Sorted() []string {
	names := make([]string, 0, len(f.Groups))
	for name := range f.Groups {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		a, b := f.Groups[names[i]], f.Groups[names[j]]
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		return names[i] < names[j]
	})
	return names
}

func (f BootFindings) clone() BootFindings {
	groups := make(map[string]GroupFindings, len(f.Groups))
	for name, g := range f.Groups {
		groups[name] = g
	}
	f.Groups = groups
	return f
}

// Backlog is what the startup scan found. Current keeps counting live
// matches, so it covers the whole boot so far.
type Backlog struct {
	// Done is false until both boots have been scanned.
	Done    bool         `json:"done"`
	Current BootFindings `json:"current"`
	// Previous is nil when the journal has no previous boot.
	Previous *BootFindings `json:"previous,omitempty"`
}

// KernelWatcher follows kernel messages and keeps the ones that match a
// pattern. It reads from the journal when it can, else /dev/kmsg. On start
// it also scans what was logged before it started: this boot so far and the
// previous boot.
type KernelWatcher struct {
	journal Journal
	bootID  string // this boot
	// PreviousBoot, if set, is scanned as the previous boot instead of the
	// one before this boot. For testing the boot check.
	PreviousBoot string

	// restart tells the journal follower that the priority it reads at has
	// changed.
	restart chan struct{}

	mu       sync.Mutex
	patterns []kernelPattern
	ignore   []*regexp.Regexp
	maxPrio  int // least severe priority any pattern needs
	events   []KernelEvent
	backlog  Backlog
	source   string
	err      string
}

const (
	maxKernelEvents = 200 // bursts, not lines
	burstGap        = 2 * time.Second
	// maxBurst bounds a burst so a steady trickle still shows up in the
	// count window instead of extending one old burst forever.
	maxBurst = time.Minute
)

func NewKernelWatcher(j Journal) *KernelWatcher {
	return &KernelWatcher{journal: j, bootID: CurrentBootID(), restart: make(chan struct{}, 1), maxPrio: PrioErr}
}

// SetPatterns sets what to match, and the messages to skip even if they
// match.
func (k *KernelWatcher) SetPatterns(patterns []KernelPattern, ignore []string) error {
	var skip []*regexp.Regexp
	for _, expr := range ignore {
		re, err := regexp.Compile("(?i)" + expr)
		if err != nil {
			return err
		}
		skip = append(skip, re)
	}
	compiled := make([]kernelPattern, 0, len(patterns))
	maxPrio := PrioErr
	for _, p := range patterns {
		re, err := regexp.Compile("(?i)" + p.Expr)
		if err != nil {
			return err
		}
		compiled = append(compiled, kernelPattern{re: re, priority: p.Priority, group: p.Group})
		maxPrio = max(maxPrio, p.Priority)
	}
	k.mu.Lock()
	changed := k.maxPrio != maxPrio
	k.patterns, k.ignore, k.maxPrio = compiled, skip, maxPrio
	k.mu.Unlock()
	if changed {
		select {
		case k.restart <- struct{}{}:
		default:
		}
	}
	return nil
}

// matcher is a snapshot of the patterns, safe to use without the lock.
type matcher struct {
	patterns []kernelPattern
	ignore   []*regexp.Regexp
}

// match returns the group of the first pattern that matches, if any.
func (m matcher) match(prio int, msg string) (string, bool) {
	for _, p := range m.patterns {
		if prio <= p.priority && p.re.MatchString(msg) {
			for _, re := range m.ignore {
				if re.MatchString(msg) {
					return "", false
				}
			}
			return p.group, true
		}
	}
	return "", false
}

func (k *KernelWatcher) snapshot() (matcher, int) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return matcher{k.patterns, k.ignore}, k.maxPrio
}

// Start picks a source and follows it until ctx is cancelled.
func (k *KernelWatcher) Start(ctx context.Context) {
	if reason := journalProblem(); reason == "" {
		k.setSource("journal", "")
		go k.runJournal(ctx)
		return
	} else if f, err := openKmsg(); err == nil {
		k.setSource("kmsg", "")
		slog.Info("kernel journal not usable, reading /dev/kmsg instead", "reason", reason)
		go k.runKmsg(ctx, f)
		return
	} else {
		k.setSource("", reason+"; /dev/kmsg: "+err.Error())
		k.mu.Lock()
		k.backlog = Backlog{Done: true, Current: BootFindings{BootID: k.bootID, Unavailable: "kernel messages are not readable"}}
		k.mu.Unlock()
	}
}

// Source returns where messages come from, or why none can be read.
func (k *KernelWatcher) Source() (source, problem string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.source, k.err
}

// Count returns how many matching messages arrived within the window.
// Bursts count whole if they ended within it.
func (k *KernelWatcher) Count(window time.Duration) int {
	k.mu.Lock()
	defer k.mu.Unlock()
	cutoff := time.Now().Add(-window)
	n := 0
	for _, e := range k.events {
		if e.Last.After(cutoff) {
			n += e.Count
		}
	}
	return n
}

// Recent returns up to n bursts that ended within the window, newest first.
func (k *KernelWatcher) Recent(window time.Duration, n int) []KernelEvent {
	k.mu.Lock()
	defer k.mu.Unlock()
	cutoff := time.Now().Add(-window)
	var out []KernelEvent
	for i := len(k.events) - 1; i >= 0 && len(out) < n; i-- {
		if k.events[i].Last.After(cutoff) {
			out = append(out, k.events[i])
		}
	}
	return out
}

// Backlog returns what the startup scan found so far.
func (k *KernelWatcher) Backlog() Backlog {
	k.mu.Lock()
	defer k.mu.Unlock()
	b := k.backlog
	b.Current = b.Current.clone()
	if b.Previous != nil {
		prev := b.Previous.clone()
		b.Previous = &prev
	}
	return b
}

func (k *KernelWatcher) setSource(source, problem string) {
	k.mu.Lock()
	k.source, k.err = source, problem
	k.mu.Unlock()
}

// handle records a live message.
func (k *KernelWatcher) handle(t time.Time, prio int, msg string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	group, ok := matcher{k.patterns, k.ignore}.match(prio, msg)
	if !ok {
		return
	}
	k.backlog.Current.add(t, group, msg)
	k.events = addToBurst(k.events, t, group, msg)
	if len(k.events) > maxKernelEvents {
		k.events = k.events[len(k.events)-maxKernelEvents:]
	}
}

// addToBurst adds a match to the last burst of its group if it is recent
// enough, else starts a new burst.
func addToBurst(events []KernelEvent, t time.Time, group, msg string) []KernelEvent {
	for i := len(events) - 1; i >= 0; i-- {
		e := &events[i]
		if t.Sub(e.Last) >= burstGap {
			break
		}
		if e.Group == group {
			if t.Sub(e.Time) < maxBurst {
				e.Count++
				e.Last = t
				return events
			}
			break
		}
	}
	return append(events, KernelEvent{Time: t, Last: t, Count: 1, Group: group, Message: msg})
}

// journalProblem returns why the kernel journal can't be used, or "".
func journalProblem() string {
	if _, err := exec.LookPath("journalctl"); err != nil {
		return "journalctl not found"
	}
	var stderr bytes.Buffer
	cmd := exec.Command("journalctl", "-k", "-n", "0", "--no-pager")
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "journalctl failed: " + strings.TrimSpace(stderr.String())
	}
	out := stderr.String()
	if strings.Contains(out, "not seeing messages") || strings.Contains(out, "insufficient permissions") {
		return "no permission to read the system journal (add the user to the 'adm' or 'systemd-journal' group)"
	}
	return ""
}

// runJournal scans this boot so far, follows new messages from where the
// scan stopped, then scans the previous boot.
func (k *KernelWatcher) runJournal(ctx context.Context) {
	current := BootFindings{BootID: k.bootID, Started: bootTime()}
	cursor, err := k.scanBoot(ctx, "0", &current)
	if err != nil {
		current.Unavailable = err.Error()
		slog.Warn("scan this boot's kernel messages", "err", err)
	}
	k.mu.Lock()
	k.backlog.Current = current
	k.mu.Unlock()
	go k.followJournal(ctx, cursor)

	prev := k.scanPreviousBoot(ctx, current.BootID)
	k.mu.Lock()
	k.backlog.Previous, k.backlog.Done = prev, true
	k.mu.Unlock()
	logBacklog("this boot", &current)
	logBacklog("previous boot", prev)
}

func logBacklog(label string, f *BootFindings) {
	switch {
	case f == nil:
		slog.Debug("no previous boot in the journal")
	case f.Unavailable != "":
		slog.Warn("kernel backlog not scanned", "boot", label, "reason", f.Unavailable)
	case f.Count > 0:
		for _, name := range f.Sorted() {
			g := f.Groups[name]
			slog.Warn("kernel errors found before healthd started", "boot", label, "id", f.BootID, "group", name,
				"count", g.Count, "first", g.First.Format(time.DateTime), "last", g.Last.Format(time.DateTime), "sample", g.Sample)
		}
	default:
		slog.Info("no kernel errors before healthd started", "boot", label, "id", f.BootID)
	}
}

// scanBoot adds the matching messages of one boot ("0", "-1" or an ID) to
// into and returns the journal cursor of the last message read.
func (k *KernelWatcher) scanBoot(ctx context.Context, boot string, into *BootFindings) (string, error) {
	m, maxPrio := k.snapshot()
	out, err := k.journal.Open(ctx, "-k", "-b", boot, "-p", strconv.Itoa(maxPrio), "-o", "json", "-q", "--no-pager")
	if err != nil {
		return "", err
	}
	cursor, readErr := readJournal(out, func(e journalEntry) {
		if group, ok := m.match(e.Priority, e.Message); ok {
			into.add(e.Time, group, e.Message)
		}
	})
	if err := errors.Join(out.Close(), readErr); err != nil {
		return cursor, err
	}
	into.Scanned = true
	return cursor, nil
}

// scanPreviousBoot returns nil when the journal has no previous boot
// (volatile journal, first boot).
func (k *KernelWatcher) scanPreviousBoot(ctx context.Context, current string) *BootFindings {
	boots, err := listBoots(ctx, k.journal)
	if err != nil {
		return &BootFindings{Unavailable: err.Error()}
	}
	prev, ok := previousBoot(boots, current)
	if want := strings.ReplaceAll(k.PreviousBoot, "-", ""); want != "" {
		prev, ok = Boot{ID: want}, true
		for _, b := range boots {
			if b.ID == want {
				prev = b
			}
		}
	}
	if !ok {
		return nil
	}
	f := &BootFindings{BootID: prev.ID, Offset: bootOffset(boots, prev.ID, current), Started: prev.First, Ended: prev.Last}
	if _, err := k.scanBoot(ctx, prev.ID, f); err != nil {
		f.Unavailable = err.Error()
	}
	return f
}

// followJournal streams new kernel messages after cursor ("" = from now).
// journalctl is restarted when it exits, and when the patterns need a
// different priority.
func (k *KernelWatcher) followJournal(ctx context.Context, cursor string) {
	// The scan already used the current priority; drop any pending restart.
	select {
	case <-k.restart:
	default:
	}
	for ctx.Err() == nil {
		_, maxPrio := k.snapshot()
		args := []string{"-k", "-p", strconv.Itoa(maxPrio), "-f", "-o", "json", "-q", "--no-pager"}
		if cursor != "" {
			args = append(args, "--after-cursor", cursor)
		} else {
			args = append(args, "-n", "0")
		}
		run, cancel := context.WithCancel(ctx)
		go func() {
			select {
			case <-k.restart:
				cancel()
			case <-run.Done():
			}
		}()
		out, err := k.journal.Open(run, args...)
		if err != nil {
			k.setSource("journal", "journalctl: "+err.Error())
		} else {
			k.setSource("journal", "")
			last, _ := readJournal(out, func(e journalEntry) { k.handle(e.Time, e.Priority, e.Message) })
			if last != "" {
				cursor = last
			}
			_ = out.Close()
		}
		restarted := run.Err() != nil && ctx.Err() == nil
		cancel()
		if ctx.Err() != nil {
			return
		}
		if restarted {
			slog.Debug("kernel patterns changed, restarting journalctl")
			continue
		}
		slog.Warn("journalctl exited, restarting in 30s")
		select {
		case <-ctx.Done():
		case <-time.After(30 * time.Second):
		}
	}
}

// bootTime is when this boot started, from the uptime.
func bootTime() time.Time {
	fields := strings.Fields(hw.ReadString("/proc/uptime"))
	if len(fields) == 0 {
		return time.Time{}
	}
	up, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return time.Time{}
	}
	return time.Now().Add(-time.Duration(up * float64(time.Second))).Truncate(time.Second)
}

func openKmsg() (*os.File, error) {
	f, err := os.Open("/dev/kmsg")
	if err != nil {
		return nil, err
	}
	// The live reader starts at the end; the backlog is read separately.
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// kmsgRecord is one /dev/kmsg record.
type kmsgRecord struct {
	prio int
	seq  int64
	usec int64 // since boot
	msg  string
}

// parseKmsg reads "<prio>,<seq>,<usec>,<flags>;<message>\n<key=value...>".
func parseKmsg(record []byte) (kmsgRecord, bool) {
	header, msg, found := strings.Cut(string(record), ";")
	if !found {
		return kmsgRecord{}, false
	}
	msg, _, _ = strings.Cut(msg, "\n")
	fields := strings.Split(header, ",")
	if len(fields) < 3 {
		return kmsgRecord{}, false
	}
	prio, err1 := strconv.Atoi(fields[0])
	seq, err2 := strconv.ParseInt(fields[1], 10, 64)
	usec, err3 := strconv.ParseInt(fields[2], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return kmsgRecord{}, false
	}
	return kmsgRecord{prio: prio & 7, seq: seq, usec: usec, msg: msg}, true
}

// runKmsg scans the kernel ring buffer for this boot's backlog, then
// follows live. /dev/kmsg only holds the current boot, and only as far back
// as the ring buffer reaches, so the previous boot can't be checked.
func (k *KernelWatcher) runKmsg(ctx context.Context, live *os.File) {
	current := BootFindings{BootID: k.bootID, Started: bootTime()}
	lastSeq, err := k.scanKmsg(&current)
	if err != nil {
		current.Unavailable = "/dev/kmsg: " + err.Error()
	}
	k.mu.Lock()
	k.backlog = Backlog{
		Done:     true,
		Current:  current,
		Previous: &BootFindings{Unavailable: "reading /dev/kmsg, which only has the current boot"},
	}
	k.mu.Unlock()
	logBacklog("this boot", &current)
	k.followKmsg(ctx, live, lastSeq)
}

// scanKmsg reads the ring buffer without blocking and returns the sequence
// number of the last record read.
func (k *KernelWatcher) scanKmsg(into *BootFindings) (int64, error) {
	fd, err := syscall.Open("/dev/kmsg", syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return -1, err
	}
	defer syscall.Close(fd)
	m, _ := k.snapshot()
	booted := into.Started
	lastSeq := int64(-1)
	buf := make([]byte, 16*1024)
	for {
		n, err := syscall.Read(fd, buf)
		switch {
		case errors.Is(err, syscall.EAGAIN):
			into.Scanned = true
			return lastSeq, nil
		case errors.Is(err, syscall.EPIPE):
			continue // ring buffer overwrote unread records; keep going
		case err != nil:
			return lastSeq, err
		}
		rec, ok := parseKmsg(buf[:n])
		if !ok {
			continue
		}
		lastSeq = rec.seq
		if group, ok := m.match(rec.prio, rec.msg); ok {
			// Approximate: the kmsg clock stops during suspend.
			into.add(booted.Add(time.Duration(rec.usec)*time.Microsecond), group, rec.msg)
		}
	}
}

// followKmsg reads live records, skipping those the backlog scan already
// read (seq ≤ skipThrough).
func (k *KernelWatcher) followKmsg(ctx context.Context, f *os.File, skipThrough int64) {
	go func() {
		<-ctx.Done()
		f.Close()
	}()
	buf := make([]byte, 16*1024)
	for {
		n, err := f.Read(buf)
		if errors.Is(err, syscall.EPIPE) {
			continue // ring buffer overwrote unread records; keep going
		}
		if err != nil {
			if ctx.Err() == nil {
				k.setSource("kmsg", "/dev/kmsg: "+err.Error())
			}
			return
		}
		rec, ok := parseKmsg(buf[:n])
		if !ok || rec.seq <= skipThrough {
			continue
		}
		k.handle(time.Now(), rec.prio, rec.msg)
	}
}
