// Package config loads the healthd TOML config. Every threshold field is
// optional: nil means "use the hardware-derived or built-in default".
package config

import (
	"errors"
	"fmt"
	"io/fs"
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
	Enabled       *bool     `toml:"enabled"`
	Window        *Duration `toml:"window"`
	ExtraPatterns []string  `toml:"extra_patterns"`
}

func (k Kernel) IsEnabled() bool {
	return k.Enabled == nil || *k.Enabled
}

type Notify struct {
	Enabled *bool `toml:"enabled"`
	Sound   *bool `toml:"sound"`
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
	return cfg, true, warnings, nil
}
