package daemon

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/joaovictornsv/healthd/internal/config"
	"github.com/joaovictornsv/healthd/internal/hw"
)

// The kernel options in the starter config must be real keys with the real
// defaults: uncommenting them changes nothing.
func TestStarterConfigKernelOptions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := writeStarterConfig(path, hw.Hardware{CPUs: 4}, nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	kernel := text[strings.Index(text, "[kernel]"):]
	for _, want := range []string{"pcie_errors = true", "ignore_patterns = []", "extra_patterns_at", "[kernel.boot_summary]",
		"enabled = true", `delay = "3m"`, "notify_when_clean = true"} {
		if !strings.Contains(kernel, want) {
			t.Errorf("starter config missing %q:\n%s", want, kernel)
		}
	}

	setting := regexp.MustCompile(`(?m)^# ([a-z_]+ = .*)$`)
	uncommented := setting.ReplaceAllString(kernel, "$1")
	// The inline extra_patterns_at example isn't a default.
	uncommented = regexp.MustCompile(`(?m)^extra_patterns_at = .*$`).ReplaceAllString(uncommented, "")
	if err := os.WriteFile(path, []byte(uncommented), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, _, warnings, err := config.Load(path)
	if err != nil {
		t.Fatalf("%v\n%s", err, uncommented)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings: %q", warnings)
	}
	k := cfg.Kernel
	if !k.IsEnabled() || !k.PCIeEnabled() || k.Window.Duration != defaultKernelWindow ||
		!k.BootSummary.IsEnabled() || !k.BootSummary.NotifyClean() || k.BootSummary.DelayOrDefault() != 3*time.Minute {
		t.Errorf("uncommented kernel config = %+v", k)
	}
}
