package api

import (
	"fmt"
	"math"
	"strconv"
)

// FormatValue renders a value with its unit: "84°C", "62%", "3 errors".
func FormatValue(unit string, v float64) string {
	switch unit {
	case "°C":
		return fmt.Sprintf("%.0f°C", v)
	case "%":
		return fmt.Sprintf("%.0f%%", v)
	case "errors":
		n := int(math.Round(v))
		if n == 1 {
			return "1 error"
		}
		return Thousands(n) + " errors"
	}
	return fmt.Sprintf("%.1f %s", v, unit)
}

// Thousands formats 1536 as "1,536".
func Thousands(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0 && s[i-1] != '-'; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// KernelErrors counts matches of a kernel pattern group: "1,536 PCIe link
// errors", "1 kernel error".
func KernelErrors(group string, n int) string {
	noun := "kernel error"
	if group == "pcie" {
		noun = "PCIe link error"
	}
	if n != 1 {
		noun += "s"
	}
	return Thousands(n) + " " + noun
}

// DisplayName is the indicator name plus its device, e.g. "Disk temp nvme0".
func (i Indicator) DisplayName() string {
	if i.Device == "" {
		return i.Name
	}
	return i.Name + " " + i.Device
}
