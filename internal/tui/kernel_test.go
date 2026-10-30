package tui

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/joaovictornsv/healthd/internal/api"
	"github.com/joaovictornsv/healthd/internal/metrics"
)

// kernelFixture is the kernel indicator as the API sends it after the boot
// check found the PCIe burst in the previous boot.
func kernelFixture(t *testing.T) *api.Status {
	t.Helper()
	burst := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	detail := map[string]any{
		"source": "journal",
		"recent": []metrics.KernelEvent{},
		"backlog": metrics.Backlog{
			Done:    true,
			Current: metrics.BootFindings{BootID: "current", Scanned: true},
			Previous: &metrics.BootFindings{
				BootID: "crashed", Offset: -1, Scanned: true, Count: 1537,
				Groups: map[string]metrics.GroupFindings{
					metrics.GroupPCIe: {Count: 1536, First: burst, Last: burst.Add(time.Second),
						Sample: "pcieport 0000:00:06.2: AER: Correctable error message received from 0000:03:00.0"},
					metrics.GroupKernel: {Count: 1, First: burst, Last: burst, Sample: "EXT4-fs error (device nvme0n1p2)"},
				},
			},
		},
		"boot_summary": map[string]any{"time": time.Now(), "sent": true, "found": true},
	}
	// Round-trip through JSON, as the client sees it.
	data, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	s := fixture()
	for i := range s.Indicators {
		if s.Indicators[i].Kind == "kernel" {
			s.Indicators[i].Detail = decoded
		}
	}
	return s
}

func TestKernelBootCheck(t *testing.T) {
	s := kernelFixture(t)
	burst := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	span := burst.Format("Mon 15:04:05") + "–" + burst.Add(time.Second).Format("15:04:05")
	var m tea.Model = New(nil, "")
	m, _ = m.Update(statusMsg{status: s})
	_, out := render(t, m, 120, 30)
	if !strings.Contains(out, "prev boot: 1,536 PCIe link errors") {
		t.Errorf("dashboard card missing the previous-boot finding\n%s", out)
	}

	m, _ = m.(Model).openDetail("kernel")
	_, out = render(t, m, 120, 30)
	for _, want := range []string{
		"Boot check", "summary sent today",
		"previous boot  1,536 PCIe link errors",
		span + "  pcieport 0000:00:06.2",
		"1 kernel error", "EXT4-fs error",
		"this boot", "no errors",
		"Recent kernel errors", "none since healthd started",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("kernel detail missing %q\n%s", want, out)
		}
	}
}
