// Package hw detects which sensors, drives, mounts, batteries and swap a
// machine has, so healthd only monitors what actually exists.
package hw

import (
	"bufio"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// Roots are variables so tests can point them at a fake tree.
var (
	SysRoot  = "/sys"
	ProcRoot = "/proc"
)

type TempSensor struct {
	Name   string  // short display name: "Package id 0", "nvme0"
	Source string  // where it was found: "hwmon coretemp", "thermal x86_pkg_temp"
	Path   string  // file holding millidegrees Celsius
	Max    float64 // hardware "max" limit in °C, 0 if not reported
	Crit   float64 // hardware "crit" limit in °C, 0 if not reported
}

type Mount struct {
	Path   string
	Device string
	FSType string
}

type Battery struct {
	Name string
	Dir  string
}

type Hardware struct {
	CPUs      int
	CPUTemp   *TempSensor
	Drives    []TempSensor
	Mounts    []Mount
	Batteries []Battery
	SwapTotal uint64 // bytes
}

// Detect scans the machine. cpuSensor/cpuLabel force a specific hwmon
// sensor for CPU temperature; empty means auto-detect.
func Detect(cpuSensor, cpuLabel string) Hardware {
	hwmons := listHwmon()
	return Hardware{
		CPUs:      runtime.NumCPU(),
		CPUTemp:   detectCPUTemp(hwmons, cpuSensor, cpuLabel),
		Drives:    detectDrives(hwmons),
		Mounts:    DetectMounts(),
		Batteries: detectBatteries(),
		SwapTotal: ReadMeminfo()["SwapTotal"],
	}
}

type hwmon struct {
	dir  string
	name string
}

type tempChannel struct {
	index int
	label string
	input string
	max   float64
	crit  float64
}

func listHwmon() []hwmon {
	dirs, _ := filepath.Glob(filepath.Join(SysRoot, "class/hwmon/hwmon*"))
	var out []hwmon
	for _, dir := range dirs {
		out = append(out, hwmon{dir: dir, name: ReadString(filepath.Join(dir, "name"))})
	}
	return out
}

func (h hwmon) temps() []tempChannel {
	inputs, _ := filepath.Glob(filepath.Join(h.dir, "temp*_input"))
	var out []tempChannel
	for _, input := range inputs {
		prefix := strings.TrimSuffix(input, "_input")
		index, _ := strconv.Atoi(strings.TrimPrefix(filepath.Base(prefix), "temp"))
		out = append(out, tempChannel{
			index: index,
			label: ReadString(prefix + "_label"),
			input: input,
			max:   ReadMilli(prefix + "_max"),
			crit:  ReadMilli(prefix + "_crit"),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].index < out[j].index })
	return out
}

// pick returns the channel matching the first label found, else the first channel.
func (h hwmon) pick(labels ...string) *tempChannel {
	temps := h.temps()
	if len(temps) == 0 {
		return nil
	}
	for _, label := range labels {
		for i := range temps {
			if label != "" && temps[i].label == label {
				return &temps[i]
			}
		}
	}
	return &temps[0]
}

func sensorFrom(h hwmon, c *tempChannel) *TempSensor {
	name := c.label
	if name == "" {
		name = h.name
	}
	return &TempSensor{Name: name, Source: "hwmon " + h.name, Path: c.input, Max: c.max, Crit: c.crit}
}

// CPU sensors in priority order, with the preferred channel labels.
var cpuHwmon = []struct {
	name   string
	labels []string
}{
	{"coretemp", []string{"Package id 0"}},
	{"k10temp", []string{"Tctl", "Tdie"}},
	{"zenpower", []string{"Tdie", "Tctl"}},
	{"cpu_thermal", nil},
	{"cpu-thermal", nil},
	{"soc_thermal", nil},
}

var cpuThermalZones = []string{"x86_pkg_temp", "cpu-thermal", "cpu_thermal", "soc_thermal", "acpitz"}

func detectCPUTemp(hwmons []hwmon, forcedName, forcedLabel string) *TempSensor {
	if forcedName != "" {
		for _, h := range hwmons {
			if h.name == forcedName {
				if c := h.pick(forcedLabel); c != nil {
					return sensorFrom(h, c)
				}
			}
		}
		return nil
	}
	for _, want := range cpuHwmon {
		for _, h := range hwmons {
			if h.name != want.name {
				continue
			}
			if c := h.pick(want.labels...); c != nil {
				return sensorFrom(h, c)
			}
		}
	}
	zones, _ := filepath.Glob(filepath.Join(SysRoot, "class/thermal/thermal_zone*"))
	for _, want := range cpuThermalZones {
		for _, zone := range zones {
			if ReadString(filepath.Join(zone, "type")) == want {
				return &TempSensor{
					Name:   want,
					Source: "thermal " + want,
					Path:   filepath.Join(zone, "temp"),
					Crit:   criticalTrip(zone),
				}
			}
		}
	}
	return nil
}

func criticalTrip(zone string) float64 {
	types, _ := filepath.Glob(filepath.Join(zone, "trip_point_*_type"))
	for _, t := range types {
		if ReadString(t) == "critical" {
			return ReadMilli(strings.TrimSuffix(t, "_type") + "_temp")
		}
	}
	return 0
}

func detectDrives(hwmons []hwmon) []TempSensor {
	var out []TempSensor
	for _, h := range hwmons {
		if h.name != "nvme" && h.name != "drivetemp" {
			continue
		}
		c := h.pick("Composite")
		if c == nil {
			continue
		}
		s := sensorFrom(h, c)
		s.Name = driveName(h)
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// driveName finds the kernel device name (nvme0, sda) behind a hwmon entry.
func driveName(h hwmon) string {
	if blocks, _ := filepath.Glob(filepath.Join(h.dir, "device/block/*")); len(blocks) > 0 {
		return filepath.Base(blocks[0])
	}
	if target, err := os.Readlink(filepath.Join(h.dir, "device")); err == nil {
		return filepath.Base(target)
	}
	return filepath.Base(h.dir)
}

var realFilesystems = map[string]bool{
	"ext2": true, "ext3": true, "ext4": true, "xfs": true, "btrfs": true, "f2fs": true,
	"zfs": true, "bcachefs": true, "jfs": true, "reiserfs": true, "nilfs2": true,
}

var skippedMountPrefixes = []string{"/snap", "/run", "/var/lib/docker", "/var/lib/containers", "/proc", "/sys", "/dev"}

// DetectMounts lists real, local filesystems, one entry per device.
func DetectMounts() []Mount {
	f, err := os.Open(filepath.Join(ProcRoot, "self/mounts"))
	if err != nil {
		return nil
	}
	defer f.Close()

	var all []Mount
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 || !realFilesystems[fields[2]] {
			continue
		}
		m := Mount{Device: fields[0], Path: unescapeMount(fields[1]), FSType: fields[2]}
		if hasSkippedPrefix(m.Path) {
			continue
		}
		all = append(all, m)
	}

	// Shortest path first, so a device mounted twice (btrfs subvolumes,
	// bind mounts) is reported at its top-level mount.
	sort.SliceStable(all, func(i, j int) bool { return len(all[i].Path) < len(all[j].Path) })
	seen := map[string]bool{}
	var out []Mount
	for _, m := range all {
		if seen[m.Device] {
			continue
		}
		seen[m.Device] = true
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func hasSkippedPrefix(path string) bool {
	for _, p := range skippedMountPrefixes {
		if path == p || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return false
}

// unescapeMount decodes the octal escapes (\040 for space) used in /proc/mounts.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func detectBatteries() []Battery {
	dirs, _ := filepath.Glob(filepath.Join(SysRoot, "class/power_supply/*"))
	var out []Battery
	for _, dir := range dirs {
		if ReadString(filepath.Join(dir, "type")) != "Battery" {
			continue
		}
		// scope=Device marks peripherals (mouse, keyboard, headset).
		if ReadString(filepath.Join(dir, "scope")) == "Device" {
			continue
		}
		if ReadString(filepath.Join(dir, "present")) == "0" {
			continue
		}
		out = append(out, Battery{Name: filepath.Base(dir), Dir: dir})
	}
	return out
}

// ReadMeminfo returns /proc/meminfo values in bytes.
func ReadMeminfo() map[string]uint64 {
	out := map[string]uint64{}
	f, err := os.Open(filepath.Join(ProcRoot, "meminfo"))
	if err != nil {
		return out
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		v, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		if len(fields) > 2 && fields[2] == "kB" {
			v *= 1024
		}
		out[strings.TrimSuffix(fields[0], ":")] = v
	}
	return out
}

func ReadString(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func ReadMilli(path string) float64 {
	v, err := strconv.ParseFloat(ReadString(path), 64)
	if err != nil {
		return 0
	}
	return v / 1000
}
