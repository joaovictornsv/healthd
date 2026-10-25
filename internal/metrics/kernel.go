package metrics

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

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

type KernelEvent struct {
	Time    time.Time `json:"time"`
	Message string    `json:"message"`
}

// KernelWatcher follows error-level kernel messages and keeps the ones that
// match a pattern. It reads from the journal when it can, else /dev/kmsg.
type KernelWatcher struct {
	mu       sync.Mutex
	patterns []*regexp.Regexp
	events   []KernelEvent
	source   string
	err      string
}

const maxKernelEvents = 200

func NewKernelWatcher(patterns []string) (*KernelWatcher, error) {
	k := &KernelWatcher{}
	if err := k.SetPatterns(patterns); err != nil {
		return nil, err
	}
	return k, nil
}

func (k *KernelWatcher) SetPatterns(patterns []string) error {
	compiled := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		re, err := regexp.Compile("(?i)" + p)
		if err != nil {
			return err
		}
		compiled = append(compiled, re)
	}
	k.mu.Lock()
	k.patterns = compiled
	k.mu.Unlock()
	return nil
}

// Start picks a source and follows it until ctx is cancelled.
func (k *KernelWatcher) Start(ctx context.Context) {
	if reason := journalProblem(); reason == "" {
		k.setSource("journal", "")
		go k.followJournal(ctx)
		return
	} else if f, err := openKmsg(); err == nil {
		k.setSource("kmsg", "")
		slog.Info("kernel journal not usable, reading /dev/kmsg instead", "reason", reason)
		go k.followKmsg(ctx, f)
		return
	} else {
		k.setSource("", reason+"; /dev/kmsg: "+err.Error())
	}
}

// Source returns where messages come from, or why none can be read.
func (k *KernelWatcher) Source() (source, problem string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.source, k.err
}

// Count returns how many matching messages arrived within the window.
func (k *KernelWatcher) Count(window time.Duration) int {
	k.mu.Lock()
	defer k.mu.Unlock()
	cutoff := time.Now().Add(-window)
	n := 0
	for _, e := range k.events {
		if e.Time.After(cutoff) {
			n++
		}
	}
	return n
}

// Recent returns up to n matching messages newer than the window, newest first.
func (k *KernelWatcher) Recent(window time.Duration, n int) []KernelEvent {
	k.mu.Lock()
	defer k.mu.Unlock()
	cutoff := time.Now().Add(-window)
	var out []KernelEvent
	for i := len(k.events) - 1; i >= 0 && len(out) < n; i-- {
		if k.events[i].Time.After(cutoff) {
			out = append(out, k.events[i])
		}
	}
	return out
}

func (k *KernelWatcher) setSource(source, problem string) {
	k.mu.Lock()
	k.source, k.err = source, problem
	k.mu.Unlock()
}

func (k *KernelWatcher) handle(t time.Time, msg string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, re := range k.patterns {
		if re.MatchString(msg) {
			k.events = append(k.events, KernelEvent{Time: t, Message: msg})
			if len(k.events) > maxKernelEvents {
				k.events = k.events[len(k.events)-maxKernelEvents:]
			}
			return
		}
	}
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

func (k *KernelWatcher) followJournal(ctx context.Context) {
	for ctx.Err() == nil {
		cmd := exec.CommandContext(ctx, "journalctl", "-k", "-p", "err", "-f", "-n", "0", "-o", "json", "-q", "--no-pager")
		stdout, err := cmd.StdoutPipe()
		if err == nil {
			err = cmd.Start()
		}
		if err != nil {
			k.setSource("journal", "journalctl: "+err.Error())
		} else {
			k.setSource("journal", "")
			scanner := bufio.NewScanner(stdout)
			scanner.Buffer(make([]byte, 64*1024), 1024*1024)
			for scanner.Scan() {
				if t, msg, ok := parseJournalLine(scanner.Bytes()); ok {
					k.handle(t, msg)
				}
			}
			_ = cmd.Wait()
		}
		if ctx.Err() != nil {
			return
		}
		slog.Warn("journalctl exited, restarting in 30s")
		select {
		case <-ctx.Done():
		case <-time.After(30 * time.Second):
		}
	}
}

func parseJournalLine(line []byte) (time.Time, string, bool) {
	var entry struct {
		Message  any    `json:"MESSAGE"`
		Realtime string `json:"__REALTIME_TIMESTAMP"`
	}
	if err := json.Unmarshal(line, &entry); err != nil {
		return time.Time{}, "", false
	}
	// MESSAGE is a byte array instead of a string when it isn't valid UTF-8.
	msg, ok := entry.Message.(string)
	if !ok {
		return time.Time{}, "", false
	}
	t := time.Now()
	if us, err := strconv.ParseInt(entry.Realtime, 10, 64); err == nil {
		t = time.UnixMicro(us)
	}
	return t, msg, true
}

func openKmsg() (*os.File, error) {
	f, err := os.Open("/dev/kmsg")
	if err != nil {
		return nil, err
	}
	// Seeking to the end skips the boot backlog: only new messages count.
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func (k *KernelWatcher) followKmsg(ctx context.Context, f *os.File) {
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
		// Record format: "<prio>,<seq>,<usec>,<flags>;<message>\n<key=value...>"
		record := string(buf[:n])
		header, msg, found := strings.Cut(record, ";")
		if !found {
			continue
		}
		msg, _, _ = strings.Cut(msg, "\n")
		prioField, _, _ := strings.Cut(header, ",")
		prio, err := strconv.Atoi(prioField)
		if err != nil || prio&7 > 3 { // keep emerg, alert, crit, err
			continue
		}
		k.handle(time.Now(), msg)
	}
}
