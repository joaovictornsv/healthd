package daemon

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/joaovictornsv/healthd/internal/config"
	"github.com/joaovictornsv/healthd/internal/hw"
	"github.com/joaovictornsv/healthd/internal/metrics"
)

// writeStarterConfig writes a config listing the detected hardware and the
// effective defaults as comments, so users can see what applies and
// uncomment only what they want to change.
func writeStarterConfig(path string, h hw.Hardware, specs []spec) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	var b strings.Builder
	b.WriteString(`# healthd configuration
#
# Created on first run. Commented-out values are the current defaults:
# uncomment a line and edit it to override. Apply changes with
# "systemctl --user reload healthd", "kill -HUP <pid>" or "health reload".
#
`)
	writeHardware(&b, h, "# ")
	defaults := config.Default()
	fmt.Fprintf(&b, `
sample_interval = "%s"
repeat_interval = "%s"

[notify]
# enabled = true
# sound = true
# Audio files to use instead of the built-in sounds. "~" and paths relative
# to this file's folder work. Unset = built-in.
# firing_sound = "sounds/firing.mp3"
# critical_sound = "sounds/critical.mp3"
# resolved_sound = "sounds/resolved.mp3"
`, short(defaults.SampleInterval.Duration), short(defaults.RepeatInterval.Duration))

	sections := []struct{ kind, header, extra string }{
		{"battery", "[battery]  # alerts while discharging; values are % left", ""},
		{"cpu_temp", "[cpu_temp]", "# sensor = \"coretemp\"     # hwmon name; unset = auto-detect\n# label = \"Package id 0\"\n"},
		{"disk_temp", "[disk_temp]  # applies to every drive", ""},
		{"cpu_load", "[cpu_load]  # 5-minute load average as % of CPUs", ""},
		{"memory", "[memory]", ""},
		{"swap", "[swap]", ""},
		{"disk_usage", "[disk_usage]", "# mounts = [\"/\"]           # unset = all detected filesystems\n"},
	}
	for _, sec := range sections {
		s := firstOfKind(specs, sec.kind)
		fmt.Fprintf(&b, "\n%s\n# enabled = true\n%s", sec.header, sec.extra)
		if s == nil {
			b.WriteString("# (not detected on this machine)\n")
			continue
		}
		writeRule(&b, s)
	}
	fmt.Fprintf(&b, `
[kernel]
# enabled = true
# window = "%s"
# extra_patterns = []      # regular expressions, matched case-insensitively
`, short(defaultKernelWindow))

	_, err = f.WriteString(b.String())
	return err
}

func writeRule(b *strings.Builder, s *spec) {
	note := ""
	if s.note != "" {
		note = "  # from " + s.note
	}
	fmt.Fprintf(b, "# fire_at = %g%s\n", s.rule.FireAt, note)
	fmt.Fprintf(b, "# for = \"%s\"\n", short(s.rule.For))
	fmt.Fprintf(b, "# resolve_at = %g\n", s.rule.ResolveAt)
	if s.rule.CriticalAt != nil {
		fmt.Fprintf(b, "# critical_at = %g\n", *s.rule.CriticalAt)
	}
}

func firstOfKind(specs []spec, kind string) *spec {
	for i := range specs {
		if specs[i].kind == kind {
			return &specs[i]
		}
	}
	return nil
}

func writeHardware(w io.Writer, h hw.Hardware, prefix string) {
	line := func(label, value string) { fmt.Fprintf(w, "%s%-17s %s\n", prefix, label, value) }
	fmt.Fprintf(w, "%sDetected hardware:\n", prefix)
	line("CPUs:", fmt.Sprint(h.CPUs))
	if s := h.CPUTemp; s != nil {
		line("CPU temperature:", fmt.Sprintf("%s (%s)", s.Name, joinNonEmpty(s.Source, limits(*s))))
	} else {
		line("CPU temperature:", "none found")
	}
	var drives []string
	for _, d := range h.Drives {
		if l := limits(d); l != "" {
			drives = append(drives, fmt.Sprintf("%s (%s)", d.Name, l))
		} else {
			drives = append(drives, d.Name)
		}
	}
	line("Drive temps:", orNone(drives))
	var mounts []string
	for _, m := range h.Mounts {
		mounts = append(mounts, fmt.Sprintf("%s (%s)", m.Path, m.FSType))
	}
	line("Filesystems:", orNone(mounts))
	var bats []string
	for _, bat := range h.Batteries {
		bats = append(bats, bat.Name)
	}
	line("Battery:", orNone(bats))
	if h.SwapTotal > 0 {
		line("Swap:", metrics.Bytes(h.SwapTotal))
	} else {
		line("Swap:", "none")
	}
}

func limits(s hw.TempSensor) string {
	var parts []string
	if s.Max > 0 {
		parts = append(parts, fmt.Sprintf("max %.0f°C", s.Max))
	}
	if s.Crit > 0 {
		parts = append(parts, fmt.Sprintf("crit %.0f°C", s.Crit))
	}
	return strings.Join(parts, ", ")
}

func joinNonEmpty(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, ", ")
}

func orNone(items []string) string {
	if len(items) == 0 {
		return "none"
	}
	return strings.Join(items, ", ")
}

// short formats durations without trailing zero units: "5m", not "5m0s".
func short(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// Detect prints detected hardware and the effective indicators, for
// "healthd detect".
func Detect(w io.Writer, configPath string) error {
	cfg, _, warnings, err := config.Load(configPath)
	if err != nil {
		return err
	}
	h := hw.Detect(cfg.CPUTemp.Sensor, cfg.CPUTemp.Label)
	kernel, err := metrics.NewKernelWatcher(metrics.DefaultKernelPatterns)
	if err != nil {
		return err
	}
	writeHardware(w, h, "")
	fmt.Fprintf(w, "\nIndicators (config: %s):\n", configPath)
	for _, s := range buildSpecs(cfg, h, kernel) {
		r := s.rule
		dir := "≥"
		if r.Below {
			dir = "≤"
		}
		crit := "-"
		if r.CriticalAt != nil {
			crit = fmt.Sprintf("%s %g", dir, *r.CriticalAt)
		}
		fmt.Fprintf(w, "  %-22s fire %s %-5g for %-4s resolve %-5g critical %-7s (%s)\n",
			s.id, dir, r.FireAt, short(r.For), r.ResolveAt, crit, s.source)
	}
	for _, warn := range warnings {
		fmt.Fprintf(w, "warning: %s\n", warn)
	}
	return nil
}
