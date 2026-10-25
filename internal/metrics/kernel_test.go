package metrics

import (
	"testing"
	"time"
)

func TestParseJournalLine(t *testing.T) {
	line := []byte(`{"MESSAGE":"nvme nvme0: I/O 12 QID 3 timeout, reset controller","__REALTIME_TIMESTAMP":"1792942954349008","PRIORITY":"3"}`)
	ts, msg, ok := parseJournalLine(line)
	if !ok || msg != "nvme nvme0: I/O 12 QID 3 timeout, reset controller" || ts.UnixMicro() != 1792942954349008 {
		t.Fatalf("got %v %q %v", ts, msg, ok)
	}
	// Non-UTF-8 messages are encoded as byte arrays; they are skipped.
	if _, _, ok := parseJournalLine([]byte(`{"MESSAGE":[104,105]}`)); ok {
		t.Fatal("expected byte-array MESSAGE to be skipped")
	}
}

func TestKernelWatcherMatchesPatterns(t *testing.T) {
	k, err := NewKernelWatcher(DefaultKernelPatterns)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	// Messages arrive in time order, oldest first.
	k.handle(now.Add(-10*time.Minute), "EXT4-fs error (device nvme0n1p2): old")
	for _, msg := range []string{
		"Out of memory: Killed process 1234 (node)",
		"ata1.00: failed command: READ FPDMA QUEUED",
		"BTRFS error (device sda1): bdev /dev/sda1 errs: wr 0, rd 1",
		"usb 1-2: device descriptor read/64, error -71",   // not a pattern
		"ACPI BIOS Error (bug): Could not resolve symbol", // not a pattern
	} {
		k.handle(now, msg)
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
