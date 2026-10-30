package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSoundPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := t.TempDir()
	for _, f := range []string{filepath.Join(dir, "sounds", "fire.mp3"), filepath.Join(home, "done.ogg")} {
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, "config.toml")
	conf := `[notify]
firing_sound = "sounds/fire.mp3"
critical_sound = "missing.mp3"
resolved_sound = "~/done.ogg"
`
	if err := os.WriteFile(path, []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, _, warnings, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "sounds", "fire.mp3"); cfg.Notify.FiringSound != want {
		t.Errorf("firing_sound = %q, want %q", cfg.Notify.FiringSound, want)
	}
	if want := filepath.Join(home, "done.ogg"); cfg.Notify.ResolvedSound != want {
		t.Errorf("resolved_sound = %q, want %q", cfg.Notify.ResolvedSound, want)
	}
	if cfg.Notify.CriticalSound != "" {
		t.Errorf("missing critical_sound should fall back to built-in, got %q", cfg.Notify.CriticalSound)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "critical_sound") {
		t.Errorf("warnings = %q, want one about critical_sound", warnings)
	}
}

func TestCriticalForParses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	conf := "[cpu_temp]\ncritical_at = 92\ncritical_for = \"3m\"\n"
	if err := os.WriteFile(path, []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, _, warnings, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %q", warnings)
	}
	if cfg.CPUTemp.CriticalFor == nil || cfg.CPUTemp.CriticalFor.Duration != 3*time.Minute {
		t.Errorf("critical_for = %v, want 3m", cfg.CPUTemp.CriticalFor)
	}
}

func TestKernelOptions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	conf := `[kernel]
pcie_errors = false
extra_patterns_at = { warning = ["i915.*underrun"] }

[kernel.boot_summary]
delay = "30s"
notify_when_clean = false
`
	if err := os.WriteFile(path, []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, _, warnings, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %q", warnings)
	}
	k := cfg.Kernel
	if k.PCIeEnabled() || len(k.ExtraPatternsAt["warning"]) != 1 {
		t.Errorf("kernel = %+v", k)
	}
	if !k.BootSummary.IsEnabled() || k.BootSummary.DelayOrDefault() != 30*time.Second || k.BootSummary.NotifyClean() {
		t.Errorf("boot_summary = %+v", k.BootSummary)
	}

	// Defaults.
	d := Default().Kernel
	if !d.PCIeEnabled() || !d.BootSummary.IsEnabled() || !d.BootSummary.NotifyClean() || d.BootSummary.DelayOrDefault() != 3*time.Minute {
		t.Errorf("defaults = %+v", d)
	}
}
