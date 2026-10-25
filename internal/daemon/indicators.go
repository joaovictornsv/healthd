package daemon

import (
	"fmt"
	"math"
	"time"

	"github.com/joaovictornsv/healthd/internal/alert"
	"github.com/joaovictornsv/healthd/internal/config"
	"github.com/joaovictornsv/healthd/internal/hw"
	"github.com/joaovictornsv/healthd/internal/metrics"
)

// spec describes one monitored indicator: how to read it and when to alert.
type spec struct {
	id     string
	kind   string
	name   string
	device string
	unit   string
	rule   alert.Rule
	// source says where the thresholds came from: hardware, default or config.
	source string
	// note explains a hardware-derived default, for the starter config.
	note string
	read func() (metrics.Reading, error)
}

func fptr(v float64) *float64 { return &v }

const defaultKernelWindow = 5 * time.Minute

// buildSpecs turns detected hardware plus config into the indicator list.
// Indicators for hardware that isn't present are left out.
func buildSpecs(cfg *config.Config, h hw.Hardware, kernel *metrics.KernelWatcher) []spec {
	repeat := cfg.RepeatInterval.Duration
	var specs []spec
	add := func(s spec, t config.Threshold) {
		if !t.IsEnabled() {
			return
		}
		s.rule, s.source = applyConfig(s.rule, s.source, t)
		s.rule.Repeat = repeat
		specs = append(specs, s)
	}

	if len(h.Batteries) > 0 {
		bats := h.Batteries
		add(spec{
			id: "battery", kind: "battery", name: "Battery", unit: "%",
			rule:   alert.Rule{Below: true, FireAt: 20, ResolveAt: 25, CriticalAt: fptr(10)},
			source: "default",
			read:   func() (metrics.Reading, error) { return metrics.Battery(bats) },
		}, cfg.Battery)
	}

	if s := h.CPUTemp; s != nil {
		rule, source, note := alert.Rule{FireAt: 80, ResolveAt: 70, CriticalAt: fptr(92)}, "default", ""
		if ref := firstPositive(s.Crit, s.Max); ref > 0 {
			fire := math.Round(ref - 20)
			rule = alert.Rule{FireAt: fire, ResolveAt: fire - 10, CriticalAt: fptr(math.Round(ref - 8))}
			source, note = "hardware", fmt.Sprintf("hardware limit %.0f°C", ref)
		}
		rule.For = 5 * time.Minute
		sensor := *s
		add(spec{
			id: "cpu_temp", kind: "cpu_temp", name: "CPU temp", unit: "°C",
			rule: rule, source: source, note: note,
			read: func() (metrics.Reading, error) { return metrics.Temp(sensor) },
		}, cfg.CPUTemp.Threshold)
	}

	for _, d := range h.Drives {
		rule, source, note := alert.Rule{FireAt: 60, ResolveAt: 53, CriticalAt: fptr(75)}, "default", ""
		if ref := firstPositive(d.Max, d.Crit-7); ref > 0 {
			fire := math.Round(ref - 20)
			rule = alert.Rule{FireAt: fire, ResolveAt: fire - 7, CriticalAt: fptr(math.Round(ref - 5))}
			source, note = "hardware", fmt.Sprintf("hardware limit %.0f°C", ref)
		}
		rule.For = 3 * time.Minute
		drive := d
		add(spec{
			id: "disk_temp:" + d.Name, kind: "disk_temp", name: "Disk temp", device: d.Name, unit: "°C",
			rule: rule, source: source, note: note,
			read: func() (metrics.Reading, error) { return metrics.Temp(drive) },
		}, cfg.DiskTemp)
	}

	cpus := h.CPUs
	add(spec{
		id: "cpu_load", kind: "cpu_load", name: "CPU load", unit: "%",
		rule:   alert.Rule{FireAt: 80, For: 5 * time.Minute, ResolveAt: 60},
		source: "default",
		read:   func() (metrics.Reading, error) { return metrics.CPULoad(cpus) },
	}, cfg.CPULoad)

	add(spec{
		id: "memory", kind: "memory", name: "Memory", unit: "%",
		rule:   alert.Rule{FireAt: 90, For: 2 * time.Minute, ResolveAt: 80, CriticalAt: fptr(97)},
		source: "default",
		read:   metrics.Memory,
	}, cfg.Memory)

	if h.SwapTotal > 0 {
		add(spec{
			id: "swap", kind: "swap", name: "Swap", unit: "%",
			rule:   alert.Rule{FireAt: 80, For: 5 * time.Minute, ResolveAt: 60},
			source: "default",
			read:   metrics.Swap,
		}, cfg.Swap)
	}

	mounts := h.Mounts
	if len(cfg.DiskUsage.Mounts) > 0 {
		mounts = nil
		for _, path := range cfg.DiskUsage.Mounts {
			mounts = append(mounts, hw.Mount{Path: path})
		}
	}
	for _, m := range mounts {
		path := m.Path
		add(spec{
			id: "disk_usage:" + path, kind: "disk_usage", name: "Disk usage", device: path, unit: "%",
			rule:   alert.Rule{FireAt: 85, For: 5 * time.Minute, ResolveAt: 80, CriticalAt: fptr(95)},
			source: "default",
			read:   func() (metrics.Reading, error) { return metrics.DiskUsage(path) },
		}, cfg.DiskUsage.Threshold)
	}

	if cfg.Kernel.IsEnabled() {
		window := defaultKernelWindow
		if cfg.Kernel.Window != nil {
			window = cfg.Kernel.Window.Duration
		}
		specs = append(specs, spec{
			id: "kernel", kind: "kernel", name: "Kernel", unit: "errors",
			// Any matching error fires straight away as critical, and resolves
			// once no new one has arrived for the whole window.
			rule:   alert.Rule{FireAt: 1, ResolveAt: 1, CriticalAt: fptr(1), Repeat: repeat},
			source: "default",
			read:   func() (metrics.Reading, error) { return kernelReading(kernel, window) },
		})
	}
	return specs
}

func kernelReading(k *metrics.KernelWatcher, window time.Duration) (metrics.Reading, error) {
	source, problem := k.Source()
	if problem != "" {
		return metrics.Reading{}, fmt.Errorf("%s", problem)
	}
	n := k.Count(window)
	status := fmt.Sprintf("no errors in the last %s", metrics.Duration(window))
	if n > 0 {
		status = fmt.Sprintf("%d in the last %s", n, metrics.Duration(window))
	}
	recent := k.Recent(24*time.Hour, 5)
	return metrics.Reading{
		Value:  float64(n),
		Active: true,
		Status: status,
		Detail: map[string]any{"source": source, "recent": recent},
	}, nil
}

// applyConfig overrides default thresholds with any values set in config.
// When fire_at is overridden but resolve_at isn't, the default gap between
// them is kept so hysteresis still works.
func applyConfig(r alert.Rule, source string, t config.Threshold) (alert.Rule, string) {
	overridden := false
	if t.FireAt != nil {
		gap := r.FireAt - r.ResolveAt
		r.FireAt = *t.FireAt
		r.ResolveAt = r.FireAt - gap
		overridden = true
	}
	if t.ResolveAt != nil {
		r.ResolveAt = *t.ResolveAt
		overridden = true
	}
	if t.For != nil {
		r.For = t.For.Duration
		overridden = true
	}
	if t.CriticalAt != nil {
		r.CriticalAt = fptr(*t.CriticalAt)
		overridden = true
	}
	if overridden {
		source = "config"
	}
	return r, source
}

func firstPositive(vals ...float64) float64 {
	for _, v := range vals {
		if v > 0 {
			return v
		}
	}
	return 0
}
