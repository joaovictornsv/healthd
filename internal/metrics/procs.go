package metrics

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/joaovictornsv/healthd/internal/hw"
)

// clockTicks is USER_HZ, which Linux fixes at 100 on all mainstream architectures.
const clockTicks = 100

type Proc struct {
	Name string  `json:"name"`
	CPU  float64 `json:"cpu"` // percent of one CPU, like top
	Mem  uint64  `json:"mem"` // resident bytes
}

// ProcSampler computes per-process CPU usage between consecutive samples.
// Processes are grouped by name so 30 browser processes show as one line.
type ProcSampler struct {
	prev   map[int]uint64
	prevAt time.Time
}

func NewProcSampler() *ProcSampler {
	return &ProcSampler{prev: map[int]uint64{}}
}

// Sample returns the top n process groups by CPU and by memory.
func (p *ProcSampler) Sample(n int) (byCPU, byMem []Proc) {
	now := time.Now()
	elapsed := now.Sub(p.prevAt).Seconds()
	pageSize := uint64(os.Getpagesize())

	groups := map[string]*Proc{}
	current := map[int]uint64{}
	entries, _ := os.ReadDir(hw.ProcRoot)
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		name, ticks, rssPages, ok := readStat(pid)
		if !ok {
			continue
		}
		current[pid] = ticks
		g := groups[name]
		if g == nil {
			g = &Proc{Name: name}
			groups[name] = g
		}
		g.Mem += rssPages * pageSize
		if prev, seen := p.prev[pid]; seen && elapsed > 0 && ticks >= prev {
			g.CPU += float64(ticks-prev) / clockTicks / elapsed * 100
		}
	}
	p.prev, p.prevAt = current, now

	all := make([]Proc, 0, len(groups))
	for _, g := range groups {
		all = append(all, *g)
	}
	byCPU = topBy(all, n, func(a, b Proc) bool { return a.CPU > b.CPU })
	byMem = topBy(all, n, func(a, b Proc) bool { return a.Mem > b.Mem })
	return byCPU, byMem
}

func topBy(all []Proc, n int, less func(a, b Proc) bool) []Proc {
	sorted := append([]Proc(nil), all...)
	sort.Slice(sorted, func(i, j int) bool { return less(sorted[i], sorted[j]) })
	if len(sorted) > n {
		sorted = sorted[:n]
	}
	return sorted
}

// readStat parses /proc/<pid>/stat. The command name is in parentheses and
// may itself contain spaces or parentheses, so fields are counted from the
// last ')'.
func readStat(pid int) (name string, ticks, rssPages uint64, ok bool) {
	b, err := os.ReadFile(filepath.Join(hw.ProcRoot, strconv.Itoa(pid), "stat"))
	if err != nil {
		return "", 0, 0, false
	}
	s := string(b)
	open, close := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
	if open < 0 || close < open {
		return "", 0, 0, false
	}
	fields := strings.Fields(s[close+1:])
	// fields[0] is field 3 (state); utime=14, stime=15, rss=24.
	if len(fields) < 22 {
		return "", 0, 0, false
	}
	utime, _ := strconv.ParseUint(fields[11], 10, 64)
	stime, _ := strconv.ParseUint(fields[12], 10, 64)
	rss, _ := strconv.ParseUint(fields[21], 10, 64)
	return s[open+1 : close], utime + stime, rss, true
}
