// health is the client for healthd: an interactive dashboard, plus
// one-shot commands for status, alert history and snoozes.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/joaovictornsv/healthd/internal/api"
	"github.com/joaovictornsv/healthd/internal/paths"
	"github.com/joaovictornsv/healthd/internal/tui"
)

const usage = `Usage: health [flags] [command]

Commands:
  (none)                         open the dashboard (status table when not a terminal)
  status                         print current status
  events [id]                    print recent alert events
  snooze <id> <30m|1h|until-resolved>
  unsnooze <id>
  reload                         reload healthd config

Flags:
`

func main() {
	socket := flag.String("socket", paths.Socket(), "healthd API socket")
	asJSON := flag.Bool("json", false, "print raw JSON")
	flag.Usage = func() {
		fmt.Fprint(os.Stderr, usage)
		flag.PrintDefaults()
	}
	flag.Parse()
	c := api.NewClient(*socket)
	args := flag.Args()

	var err error
	switch cmd := first(args); cmd {
	case "":
		if interactive && !*asJSON {
			err = tui.Run(c, *socket)
		} else {
			err = status(c, *asJSON)
		}
	case "status":
		err = status(c, *asJSON)
	case "events":
		err = events(c, first(args[1:]), *asJSON)
	case "snooze":
		if len(args) != 3 {
			flag.Usage()
			os.Exit(2)
		}
		duration := args[2]
		if duration == "until-resolved" {
			duration = api.UntilResolved
		}
		err = c.Snooze(args[1], duration)
	case "unsnooze":
		if len(args) != 2 {
			flag.Usage()
			os.Exit(2)
		}
		err = c.Unsnooze(args[1])
	case "reload":
		err = c.Reload()
	default:
		flag.Usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "health:", err)
		os.Exit(1)
	}
}

func first(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

var (
	interactive = isTerminal()
	color       = interactive && os.Getenv("NO_COLOR") == ""
)

func isTerminal() bool {
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func paint(code, s string) string {
	if !color {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func status(c *api.Client, asJSON bool) error {
	s, err := c.Status()
	if err != nil {
		return err
	}
	if asJSON {
		return printJSON(s)
	}

	fmt.Printf("%s · up %s · sampled %s ago\n\n", paint("1", s.Host),
		human(time.Duration(s.UptimeSeconds)*time.Second), human(time.Since(s.SampledAt)))
	fmt.Printf("%-20s %-12s %-10s %-28s %s\n", "ID", "VALUE", "STATE", "THRESHOLDS", "DETAILS")
	for _, ind := range s.Indicators {
		value := "—"
		if ind.Value != nil {
			value = api.FormatValue(ind.Unit, *ind.Value)
		}
		details := ind.Status
		if ind.Error != "" {
			details = paint("31", "error: "+ind.Error)
		}
		if note := tui.BootFindingLine(ind); note != "" {
			details += " · " + paint("31", note)
		}
		if ind.SnoozedUntil != nil {
			details += fmt.Sprintf(" · zz until %s", ind.SnoozedUntil.Format("15:04"))
		} else if ind.SnoozeUntilResolved {
			details += " · zz until resolved"
		}
		fmt.Printf("%-20s %-12s %s %-28s %s\n", ind.ID, value, stateLabel(ind), thresholds(ind), details)
	}
	for _, w := range s.Warnings {
		fmt.Println(paint("33", "warning: "+w))
	}
	return nil
}

func stateLabel(ind api.Indicator) string {
	label, code := ind.State, "32"
	switch {
	case ind.State == "firing" && ind.Critical:
		label, code = "CRITICAL", "1;31"
	case ind.State == "firing":
		label, code = "FIRING", "31"
	case ind.State == "pending":
		label, code = "PENDING", "33"
	}
	// Pad before colouring so escape codes don't break the column width.
	return paint(code, fmt.Sprintf("%-10s", label))
}

func thresholds(ind api.Indicator) string {
	t := ind.Thresholds
	if ind.Kind == "kernel" {
		return "any match"
	}
	op := "≥"
	if t.Direction == "below" {
		op = "≤"
	}
	s := fmt.Sprintf("%s%s", op, api.FormatValue(ind.Unit, t.FireAt))
	if t.CriticalAt != nil {
		s += fmt.Sprintf(" crit %s", api.FormatValue(ind.Unit, *t.CriticalAt))
	}
	return s + " (" + t.Source + ")"
}

func events(c *api.Client, id string, asJSON bool) error {
	evs, err := c.Events(id, 30)
	if err != nil {
		return err
	}
	if asJSON {
		return printJSON(evs)
	}
	if len(evs) == 0 {
		fmt.Println("no alert events yet")
		return nil
	}
	for _, e := range evs {
		kind := strings.ToUpper(e.Kind)
		if e.Critical && e.Kind != "resolved" {
			kind += " (critical)"
		}
		fmt.Printf("%s  %-20s %-20s %s\n", e.Time.Format("2006-01-02 15:04"), e.Indicator, kind, e.Message)
	}
	return nil
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func human(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
}
