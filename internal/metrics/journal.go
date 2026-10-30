package metrics

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Journal runs journalctl. Tests replace it with a fake.
type Journal interface {
	// Open starts journalctl with args and returns its output. Close waits
	// for it to exit and returns its error, with stderr, if it failed.
	Open(ctx context.Context, args ...string) (io.ReadCloser, error)
}

// Journalctl is the real Journal.
type Journalctl struct{}

func (Journalctl) Open(ctx context.Context, args ...string) (io.ReadCloser, error) {
	cmd := exec.CommandContext(ctx, "journalctl", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	p := &process{cmd: cmd, stdout: stdout}
	cmd.Stderr = &p.stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return p, nil
}

type process struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
	stderr bytes.Buffer
}

func (p *process) Read(b []byte) (int, error) { return p.stdout.Read(b) }

func (p *process) Close() error {
	p.stdout.Close()
	if err := p.cmd.Wait(); err != nil {
		if msg := strings.TrimSpace(p.stderr.String()); msg != "" {
			return fmt.Errorf("journalctl: %s", msg)
		}
		return fmt.Errorf("journalctl: %w", err)
	}
	return nil
}

// journalEntry is the part of a "journalctl -o json" line healthd uses.
type journalEntry struct {
	Time     time.Time
	Priority int
	Message  string
	Cursor   string
}

func parseJournalLine(line []byte) (journalEntry, bool) {
	var raw struct {
		Message  any    `json:"MESSAGE"`
		Realtime string `json:"__REALTIME_TIMESTAMP"`
		Priority string `json:"PRIORITY"`
		Cursor   string `json:"__CURSOR"`
	}
	if err := json.Unmarshal(line, &raw); err != nil {
		return journalEntry{}, false
	}
	// MESSAGE is a byte array instead of a string when it isn't valid UTF-8.
	msg, ok := raw.Message.(string)
	if !ok {
		return journalEntry{}, false
	}
	e := journalEntry{Time: time.Now(), Priority: PrioInfo, Message: msg, Cursor: raw.Cursor}
	if us, err := strconv.ParseInt(raw.Realtime, 10, 64); err == nil {
		e.Time = time.UnixMicro(us)
	}
	if p, err := strconv.Atoi(raw.Priority); err == nil {
		e.Priority = p
	}
	return e, true
}

// readJournal calls fn for each entry journalctl prints and returns the
// cursor of the last one.
func readJournal(r io.Reader, fn func(journalEntry)) (cursor string, err error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		if e, ok := parseJournalLine(scanner.Bytes()); ok {
			if e.Cursor != "" {
				cursor = e.Cursor
			}
			fn(e)
		}
	}
	return cursor, scanner.Err()
}

// Boot is one boot recorded in the journal.
type Boot struct {
	ID    string
	First time.Time // first journal entry
	Last  time.Time // last journal entry
}

// listBoots returns the boots in the journal, oldest first.
func listBoots(ctx context.Context, j Journal) ([]Boot, error) {
	out, err := j.Open(ctx, "--list-boots", "-o", "json", "-q", "--no-pager")
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(out)
	if err := errors.Join(out.Close(), readErr); err != nil {
		// A volatile journal with a single boot is not an error worth
		// reporting: there just is no previous boot.
		if strings.Contains(err.Error(), "No journal boot entry found") {
			return nil, nil
		}
		return nil, err
	}
	return parseBoots(data), nil
}

// parseBoots reads "journalctl --list-boots" output: JSON on systemd 251
// and later, a text table before that.
func parseBoots(data []byte) []Boot {
	var entries []struct {
		ID    string `json:"boot_id"`
		First int64  `json:"first_entry"`
		Last  int64  `json:"last_entry"`
	}
	if err := json.Unmarshal(data, &entries); err == nil {
		boots := make([]Boot, 0, len(entries))
		for _, e := range entries {
			boots = append(boots, Boot{ID: e.ID, First: time.UnixMicro(e.First), Last: time.UnixMicro(e.Last)})
		}
		return boots
	}
	// Text: " -1 bc61a82bec394363b6bac359e8f1c1d4 Fri 2026-10-30 08:42:11 -03 Fri ..."
	var boots []Boot
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || len(fields[1]) != 32 {
			continue
		}
		if _, err := strconv.Atoi(fields[0]); err != nil {
			continue
		}
		boots = append(boots, Boot{ID: fields[1]})
	}
	return boots
}

// previousBoot returns the boot before current in the list, or false.
func previousBoot(boots []Boot, current string) (Boot, bool) {
	for i, b := range boots {
		if b.ID == current && i > 0 {
			return boots[i-1], true
		}
	}
	return Boot{}, false
}

// bootOffset returns id's position relative to current ("-1" is the boot
// before), or 0 if either isn't in the list.
func bootOffset(boots []Boot, id, current string) int {
	i, c := -1, -1
	for n, b := range boots {
		switch b.ID {
		case id:
			i = n
		case current:
			c = n
		}
	}
	if i < 0 || c < 0 {
		return 0
	}
	return i - c
}

// CurrentBootID returns this boot's ID in the journal's format (no dashes).
func CurrentBootID() string {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.ReplaceAll(strings.TrimSpace(string(b)), "-", "")
}
