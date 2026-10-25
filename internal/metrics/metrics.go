// Package metrics reads the current value of each indicator from /proc and /sys.
package metrics

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/joaovictornsv/healthd/internal/hw"
)

type Reading struct {
	Value float64
	// Active is false when the alert doesn't apply right now, e.g. battery
	// while charging. Inactive readings clear the alert silently.
	Active bool
	// Status is a short human-readable note shown next to the value.
	Status string
	Detail map[string]any
}

func Temp(s hw.TempSensor) (Reading, error) {
	b, err := os.ReadFile(s.Path)
	if err != nil {
		return Reading{}, err
	}
	milli, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
	if err != nil {
		return Reading{}, fmt.Errorf("parse %s: %w", s.Path, err)
	}
	detail := map[string]any{"sensor": s.Name, "source": s.Source}
	if s.Max > 0 {
		detail["hw_max"] = s.Max
	}
	if s.Crit > 0 {
		detail["hw_crit"] = s.Crit
	}
	return Reading{Value: milli / 1000, Active: true, Detail: detail}, nil
}

// CPULoad is the 5-minute load average as a percentage of available CPUs.
func CPULoad(cpus int) (Reading, error) {
	fields := strings.Fields(hw.ReadString(filepath.Join(hw.ProcRoot, "loadavg")))
	if len(fields) < 3 {
		return Reading{}, fmt.Errorf("unexpected /proc/loadavg format")
	}
	var loads [3]float64
	for i := range loads {
		v, err := strconv.ParseFloat(fields[i], 64)
		if err != nil {
			return Reading{}, err
		}
		loads[i] = v
	}
	return Reading{
		Value:  loads[1] / float64(cpus) * 100,
		Active: true,
		Status: fmt.Sprintf("load %.2f %.2f %.2f", loads[0], loads[1], loads[2]),
		Detail: map[string]any{"load1": loads[0], "load5": loads[1], "load15": loads[2], "cpus": cpus},
	}, nil
}

func Memory() (Reading, error) {
	m := hw.ReadMeminfo()
	total, available := m["MemTotal"], m["MemAvailable"]
	if total == 0 {
		return Reading{}, fmt.Errorf("MemTotal missing from /proc/meminfo")
	}
	used := total - available
	return Reading{
		Value:  float64(used) / float64(total) * 100,
		Active: true,
		Status: fmt.Sprintf("%s / %s", Bytes(used), Bytes(total)),
		Detail: map[string]any{"used": used, "total": total},
	}, nil
}

func Swap() (Reading, error) {
	m := hw.ReadMeminfo()
	total, free := m["SwapTotal"], m["SwapFree"]
	if total == 0 {
		return Reading{Active: true, Status: "no swap"}, nil
	}
	used := total - free
	return Reading{
		Value:  float64(used) / float64(total) * 100,
		Active: true,
		Status: fmt.Sprintf("%s / %s", Bytes(used), Bytes(total)),
		Detail: map[string]any{"used": used, "total": total},
	}, nil
}

// DiskUsage reports used space the way df does: used / (used + available to
// unprivileged users), so the reserved blocks count as unavailable.
func DiskUsage(path string) (Reading, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return Reading{}, err
	}
	bsize := uint64(st.Bsize)
	used := (st.Blocks - st.Bfree) * bsize
	avail := st.Bavail * bsize
	if used+avail == 0 {
		return Reading{}, fmt.Errorf("%s reports zero size", path)
	}
	return Reading{
		Value:  float64(used) / float64(used+avail) * 100,
		Active: true,
		Status: fmt.Sprintf("%s / %s", Bytes(used), Bytes(used+avail)),
		Detail: map[string]any{"mount": path, "used": used, "free": avail, "total": used + avail},
	}, nil
}

// Battery combines all system batteries into one reading. The alert is only
// active while discharging.
func Battery(batteries []hw.Battery) (Reading, error) {
	var now, full, rate float64
	var capacitySum float64
	var statuses []string
	cycles := -1
	for _, b := range batteries {
		read := func(name string) float64 {
			v, _ := strconv.ParseFloat(hw.ReadString(filepath.Join(b.Dir, name)), 64)
			return v
		}
		statuses = append(statuses, strings.ToLower(hw.ReadString(filepath.Join(b.Dir, "status"))))
		capacitySum += read("capacity")
		if c := hw.ReadString(filepath.Join(b.Dir, "cycle_count")); c != "" && cycles < 0 {
			cycles, _ = strconv.Atoi(c)
		}
		// energy_* is in µWh with power_now in µW; charge_* is in µAh with
		// current_now in µA. Either pair gives hours = now / rate.
		if e := read("energy_full"); e > 0 {
			now += read("energy_now")
			full += e
			rate += math.Abs(read("power_now"))
		} else if c := read("charge_full"); c > 0 {
			now += read("charge_now")
			full += c
			rate += math.Abs(read("current_now"))
		}
	}
	if len(batteries) == 0 {
		return Reading{}, fmt.Errorf("no battery")
	}

	level := capacitySum / float64(len(batteries))
	if full > 0 {
		level = math.Min(now/full*100, 100)
	}

	status := combinedStatus(statuses)
	detail := map[string]any{"status": status}
	if cycles >= 0 {
		detail["cycle_count"] = cycles
	}
	note := status
	if rate > 0 {
		var hours float64
		switch status {
		case "discharging":
			hours = now / rate
		case "charging":
			hours = (full - now) / rate
		}
		if hours > 0 && hours < 48 {
			left := time.Duration(hours * float64(time.Hour)).Round(time.Minute)
			detail["time_left_seconds"] = int(left.Seconds())
			if status == "discharging" {
				note += ", " + Duration(left) + " left"
			} else {
				note += ", full in " + Duration(left)
			}
		}
	}
	return Reading{Value: level, Active: status == "discharging", Status: note, Detail: detail}, nil
}

func combinedStatus(statuses []string) string {
	has := func(s string) bool {
		for _, v := range statuses {
			if v == s {
				return true
			}
		}
		return false
	}
	switch {
	case has("discharging"):
		return "discharging"
	case has("charging"):
		return "charging"
	case has("not charging"):
		return "not charging"
	case has("full"):
		return "full"
	}
	return "unknown"
}

// Bytes formats a byte count with binary units.
func Bytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

// Duration formats a duration as "2h 14m" or "7m".
func Duration(d time.Duration) string {
	d = d.Round(time.Minute)
	h, m := int(d.Hours()), int(d.Minutes())%60
	switch {
	case h > 0 && m > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	case h > 0:
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dm", m)
}
