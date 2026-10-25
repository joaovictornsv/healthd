package api

import (
	"fmt"
	"math"
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
		return fmt.Sprintf("%d errors", n)
	}
	return fmt.Sprintf("%.1f %s", v, unit)
}

// DisplayName is the indicator name plus its device, e.g. "Disk temp nvme0".
func (i Indicator) DisplayName() string {
	if i.Device == "" {
		return i.Name
	}
	return i.Name + " " + i.Device
}
