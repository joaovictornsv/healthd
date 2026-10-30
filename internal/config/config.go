// Package config loads the healthd TOML config. Every threshold field is
// optional: nil means "use the hardware-derived or built-in default".
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

type Duration struct{ time.Duration }

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

func (d Duration) MarshalText() ([]byte, error) {
	return []byte(d.String()), nil
}

type Threshold struct {
	Enabled    *bool     `toml:"enabled"`
	FireAt     *float64  `toml:"fire_at"`
	For        *Duration `toml:"for"`
	ResolveAt  *float64  `toml:"resolve_at"`
	CriticalAt *float64  `toml:"critical_at"`
	// CriticalFor is how long the critical level must hold. Unset = same as for.
	CriticalFor *Duration `toml:"critical_for"`
}

// IsEnabled reports whether the indicator is on; indicators default to on.
func (t Threshold) IsEnabled() bool {
	return t.Enabled == nil || *t.Enabled
}

type CPUTemp struct {
	Threshold
	Sensor string `toml:"sensor"`
	Label  string `toml:"label"`
}

type DiskUsage struct {
	Threshold
	Mounts []string `toml:"mounts"`
}

type Kernel struct {
	Enabled *bool     `toml:"enabled"`
	Window  *Duration `toml:"window"`
	// ExtraPatterns are matched against error-level messages (err or worse).
	ExtraPatterns []string `toml:"extra_patterns"`
	// ExtraPatternsAt maps a priority name ("warning", "info") to patterns
	// matched down to that priority.
	ExtraPatternsAt map[string][]string `toml:"extra_patterns_at"`
	// IgnorePatterns skip messages that would otherwise match.
	IgnorePatterns []string `toml:"ignore_patterns"`
	// PCIeErrors turns on the PCIe link (AER) error patterns.
	PCIeErrors  *bool       `toml:"pcie_errors"`
	BootSummary BootSummary `toml:"boot_summary"`
}

func (k Kernel) IsEnabled() bool {
	return k.Enabled == nil || *k.Enabled
}

func (k Kernel) PCIeEnabled() bool {
	return k.PCIeErrors == nil || *k.PCIeErrors
}

// BootSummary is the one notification per boot, sent a while after healthd
// starts, about kernel errors in the previous boot and this boot so far.
type BootSummary struct {
	Enabled *bool     `toml:"enabled"`
	Delay   *Duration `toml:"delay"`
	// NotifyWhenClean also sends a short "all good" when nothing was found.
	NotifyWhenClean *bool `toml:"notify_when_clean"`
}

const DefaultBootSummaryDelay = 3 * time.Minute

func (b BootSummary) IsEnabled() bool { return b.Enabled == nil || *b.Enabled }

func (b BootSummary) DelayOrDefault() time.Duration {
	if b.Delay == nil {
		return DefaultBootSummaryDelay
	}
	return b.Delay.Duration
}

func (b BootSummary) NotifyClean() bool { return b.NotifyWhenClean == nil || *b.NotifyWhenClean }

type Notify struct {
	Enabled *bool `toml:"enabled"`
	Sound   *bool `toml:"sound"`

	// Audio files that replace the built-in sounds. Load resolves "~" and
	// paths relative to the config file's directory.
	FiringSound   string `toml:"firing_sound"`
	CriticalSound string `toml:"critical_sound"`
	ResolvedSound string `toml:"resolved_sound"`
}

type Config struct {
	SampleInterval Duration `toml:"sample_interval"`
	RepeatInterval Duration `toml:"repeat_interval"`
	Notify         Notify   `toml:"notify"`

	Battery   Threshold `toml:"battery"`
	CPUTemp   CPUTemp   `toml:"cpu_temp"`
	DiskTemp  Threshold `toml:"disk_temp"`
	CPULoad   Threshold `toml:"cpu_load"`
	Memory    Threshold `toml:"memory"`
	Swap      Threshold `toml:"swap"`
	DiskUsage DiskUsage `toml:"disk_usage"`
	Kernel    Kernel    `toml:"kernel"`
}

func Default() *Config {
	return &Config{
		SampleInterval: Duration{10 * time.Second},
		RepeatInterval: Duration{10 * time.Minute},
	}
}

func (c *Config) NotifyEnabled() bool { return c.Notify.Enabled == nil || *c.Notify.Enabled }
func (c *Config) SoundEnabled() bool  { return c.Notify.Sound == nil || *c.Notify.Sound }

// Load reads the config at path on top of the defaults. A missing file is not
// an error: it returns the defaults with exists=false. Unknown keys are
// returned as warnings so typos don't silently do nothing.
func Load(path string) (cfg *Config, exists bool, warnings []string, err error) {
	cfg = Default()
	md, err := toml.DecodeFile(path, cfg)
	if errors.Is(err, fs.ErrNotExist) {
		return cfg, false, nil, nil
	}
	if err != nil {
		return nil, true, nil, fmt.Errorf("read config %s: %w", path, err)
	}
	for _, key := range md.Undecoded() {
		warnings = append(warnings, fmt.Sprintf("unknown config key %q", key.String()))
	}
	if cfg.SampleInterval.Duration < time.Second {
		return nil, true, nil, fmt.Errorf("sample_interval must be at least 1s")
	}
	if cfg.Kernel.BootSummary.DelayOrDefault() < 0 {
		return nil, true, nil, fmt.Errorf("kernel.boot_summary.delay must not be negative")
	}
	for key, file := range map[string]*string{
		"firing_sound":   &cfg.Notify.FiringSound,
		"critical_sound": &cfg.Notify.CriticalSound,
		"resolved_sound": &cfg.Notify.ResolvedSound,
	} {
		if *file == "" {
			continue
		}
		*file = resolvePath(*file, filepath.Dir(path))
		if _, err := os.Stat(*file); err != nil {
			warnings = append(warnings, fmt.Sprintf("notify.%s: %v; using the built-in sound", key, err))
			*file = ""
		}
	}
	return cfg, true, warnings, nil
}

// resolvePath expands a leading "~" and makes relative paths relative to dir.
func resolvePath(p, dir string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, p[1:])
		}
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	return p
}
