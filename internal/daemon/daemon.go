// Package daemon runs the healthd service: it samples every indicator,
// drives the alert state machines, sends notifications, records history and
// serves the API.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/joaovictornsv/healthd/internal/alert"
	"github.com/joaovictornsv/healthd/internal/api"
	"github.com/joaovictornsv/healthd/internal/config"
	"github.com/joaovictornsv/healthd/internal/hw"
	"github.com/joaovictornsv/healthd/internal/metrics"
	"github.com/joaovictornsv/healthd/internal/notify"
	"github.com/joaovictornsv/healthd/internal/store"
)

const (
	ringWindow       = time.Hour
	sparkWindow      = 15 * time.Minute
	maintainInterval = 10 * time.Minute
	topProcesses     = 5
)

type Options struct {
	ConfigPath string
	SocketPath string
	DBPath     string
}

type indicator struct {
	spec
	alert   alert.Alert
	reading metrics.Reading
	read    bool // at least one successful reading
	err     error
	ring    []api.Point

	snoozedUntil        time.Time
	snoozeUntilResolved bool
}

func (ind *indicator) snoozed(now time.Time) bool {
	return ind.snoozeUntilResolved || now.Before(ind.snoozedUntil)
}

type Daemon struct {
	opts      Options
	startedAt time.Time
	store     *store.Store
	kernel    *metrics.KernelWatcher
	procs     *metrics.ProcSampler

	reloadMu sync.Mutex // serializes reloads

	mu        sync.Mutex // guards everything below
	cfg       *config.Config
	warnings  []string
	notifier  *notify.Notifier
	inds      []*indicator
	sampledAt time.Time
	topCPU    []metrics.Proc
	topMem    []metrics.Proc
}

func New(opts Options) *Daemon {
	return &Daemon{opts: opts, procs: metrics.NewProcSampler()}
}

func (d *Daemon) Run(ctx context.Context) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	d.startedAt = time.Now()
	ln, err := listen(d.opts.SocketPath)
	if err != nil {
		return err
	}
	st, err := store.Open(d.opts.DBPath)
	if err != nil {
		return fmt.Errorf("open history: %w", err)
	}
	defer st.Close()
	d.store = st

	if d.kernel, err = metrics.NewKernelWatcher(metrics.DefaultKernelPatterns); err != nil {
		return err
	}
	if err := d.Reload(); err != nil {
		return err
	}
	d.kernel.Start(ctx)
	if source, problem := d.kernel.Source(); problem != "" {
		slog.Warn("kernel errors indicator disabled", "reason", problem)
	} else {
		slog.Info("watching kernel messages", "source", source)
	}

	srv := d.serve(ln)
	defer srv.close()

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)

	if err := st.Maintain(time.Now()); err != nil {
		slog.Warn("history maintenance failed", "err", err)
	}
	maintain := time.NewTicker(maintainInterval)
	defer maintain.Stop()
	sample := time.NewTimer(0)
	defer sample.Stop()

	slog.Info("healthd started", "socket", d.opts.SocketPath, "config", d.opts.ConfigPath, "history", d.opts.DBPath)
	for {
		select {
		case <-ctx.Done():
			slog.Info("healthd stopping")
			return nil
		case <-sample.C:
			d.tick(time.Now())
			sample.Reset(d.interval())
		case <-maintain.C:
			if err := st.Maintain(time.Now()); err != nil {
				slog.Warn("history maintenance failed", "err", err)
			}
		case <-hup:
			if err := d.Reload(); err != nil {
				slog.Error("reload failed, keeping previous config", "err", err)
			}
		}
	}
}

func (d *Daemon) interval() time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cfg.SampleInterval.Duration
}

// Reload reads the config, re-detects hardware and rebuilds the indicator
// list. Alert state, history and snoozes carry over for indicators that still
// exist. On a config error the previous setup stays in place.
func (d *Daemon) Reload() error {
	d.reloadMu.Lock()
	defer d.reloadMu.Unlock()

	cfg, exists, warnings, err := config.Load(d.opts.ConfigPath)
	if err != nil {
		return err
	}
	for _, w := range warnings {
		slog.Warn(w)
	}
	if err := d.kernel.SetPatterns(append(append([]string{}, metrics.DefaultKernelPatterns...), cfg.Kernel.ExtraPatterns...)); err != nil {
		return fmt.Errorf("kernel extra_patterns: %w", err)
	}

	h := hw.Detect(cfg.CPUTemp.Sensor, cfg.CPUTemp.Label)
	if cfg.CPUTemp.Sensor != "" && h.CPUTemp == nil {
		warnings = append(warnings, fmt.Sprintf("cpu_temp.sensor %q not found", cfg.CPUTemp.Sensor))
	}
	specs := buildSpecs(cfg, h, d.kernel)

	if !exists {
		if err := writeStarterConfig(d.opts.ConfigPath, h, specs); err != nil {
			slog.Warn("could not write starter config", "err", err)
		} else {
			slog.Info("wrote starter config", "path", d.opts.ConfigPath)
		}
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	previous := map[string]*indicator{}
	for _, ind := range d.inds {
		previous[ind.id] = ind
	}
	inds := make([]*indicator, 0, len(specs))
	for _, s := range specs {
		ind := &indicator{spec: s}
		if old, ok := previous[s.id]; ok {
			ind.alert, ind.reading, ind.read, ind.err, ind.ring = old.alert, old.reading, old.read, old.err, old.ring
			ind.snoozedUntil, ind.snoozeUntilResolved = old.snoozedUntil, old.snoozeUntilResolved
		} else {
			ind.ring = d.loadRing(s.id)
		}
		inds = append(inds, ind)
	}
	d.inds = inds
	d.cfg = cfg
	d.warnings = warnings
	switch {
	case !cfg.NotifyEnabled():
		d.notifier = nil
	case d.notifier == nil:
		d.notifier = notify.New(sounds(cfg))
	default:
		d.notifier.SetSounds(sounds(cfg))
	}

	ids := make([]string, len(inds))
	for i, ind := range inds {
		ids[i] = ind.id
	}
	slog.Info("monitoring", "indicators", strings.Join(ids, ", "))
	return nil
}

// loadRing restores the last hour from SQLite so sparklines survive restarts.
func (d *Daemon) loadRing(id string) []api.Point {
	points, err := d.store.Raw(id, time.Now().Add(-ringWindow))
	if err != nil {
		slog.Warn("load history", "indicator", id, "err", err)
		return nil
	}
	return points
}

// outgoing is a notification and event produced by a tick, delivered after
// the lock is released.
type outgoing struct {
	event   api.Event
	message *notify.Message
}

func (d *Daemon) tick(now time.Time) {
	topCPU, topMem := d.procs.Sample(topProcesses)

	d.mu.Lock()
	d.sampledAt, d.topCPU, d.topMem = now, topCPU, topMem
	var samples []store.Sample
	var out []outgoing
	for _, ind := range d.inds {
		r, err := ind.spec.read()
		if err != nil {
			if ind.err == nil || ind.err.Error() != err.Error() {
				slog.Warn("read failed", "indicator", ind.id, "err", err)
			}
			ind.err = err
			continue
		}
		ind.err, ind.reading, ind.read = nil, r, true
		ind.push(now, r.Value)
		samples = append(samples, store.Sample{Indicator: ind.id, Value: r.Value})

		ev := ind.alert.Update(ind.rule, r.Value, r.Active, now)
		if ev == alert.None {
			continue
		}
		o := outgoing{}
		if ev != alert.Repeated {
			o.event = api.Event{
				Time: now, Indicator: ind.id, Kind: string(ev), Critical: ind.alert.Critical,
				Value: r.Value, Message: summary(ind, ev),
			}
		}
		if ev != alert.Cleared && !ind.snoozed(now) {
			msg := message(ind, ev, d.topFor(ind.kind))
			o.message = &msg
		}
		if ev == alert.Resolved || ev == alert.Cleared {
			ind.snoozeUntilResolved = false
		}
		out = append(out, o)
	}
	notifier := d.notifier
	d.mu.Unlock()

	if err := d.store.AddSamples(now, samples); err != nil {
		slog.Warn("save samples", "err", err)
	}
	for _, o := range out {
		if o.event.Kind != "" {
			slog.Info("alert", "indicator", o.event.Indicator, "event", o.event.Kind,
				"critical", o.event.Critical, "value", o.event.Value)
			if err := d.store.AddEvent(o.event); err != nil {
				slog.Warn("save event", "err", err)
			}
		}
		if o.message != nil && notifier != nil {
			if err := notifier.Send(*o.message); err != nil {
				slog.Warn("notification failed", "err", err)
			}
		}
	}
}

func (ind *indicator) push(now time.Time, v float64) {
	ind.ring = append(ind.ring, api.Point{T: now.Unix(), V: v})
	cutoff := now.Add(-ringWindow).Unix()
	i := 0
	for i < len(ind.ring) && ind.ring[i].T < cutoff {
		i++
	}
	if i > 0 {
		ind.ring = append([]api.Point(nil), ind.ring[i:]...)
	}
}

// topFor returns the process list relevant to an indicator kind.
func (d *Daemon) topFor(kind string) []metrics.Proc {
	switch kind {
	case "cpu_load", "cpu_temp":
		return d.topCPU
	case "memory", "swap":
		return d.topMem
	}
	return nil
}

func (d *Daemon) status() api.Status {
	d.mu.Lock()
	defer d.mu.Unlock()
	host, _ := os.Hostname()
	s := api.Status{
		Host:           host,
		UptimeSeconds:  machineUptime(),
		StartedAt:      d.startedAt,
		SampledAt:      d.sampledAt,
		SampleInterval: d.cfg.SampleInterval.String(),
		ConfigPath:     d.opts.ConfigPath,
		Warnings:       d.warnings,
		Indicators:     make([]api.Indicator, 0, len(d.inds)),
	}
	now := time.Now()
	for _, ind := range d.inds {
		s.Indicators = append(s.Indicators, ind.toAPI(now, d.topFor(ind.kind)))
	}
	return s
}

func (ind *indicator) toAPI(now time.Time, top []metrics.Proc) api.Indicator {
	direction := "above"
	if ind.rule.Below {
		direction = "below"
	}
	out := api.Indicator{
		ID: ind.id, Kind: ind.kind, Name: ind.name, Device: ind.device, Unit: ind.unit,
		Status:   ind.reading.Status,
		State:    string(ind.alert.State),
		Critical: ind.alert.Critical,
		Since:    ind.alert.Since,
		Active:   ind.reading.Active,
		Thresholds: api.Thresholds{
			Direction: direction, FireAt: ind.rule.FireAt, For: ind.rule.For.String(),
			ResolveAt: ind.rule.ResolveAt, CriticalAt: ind.rule.CriticalAt, Source: ind.source,
		},
		SnoozeUntilResolved: ind.snoozeUntilResolved,
		Detail:              ind.reading.Detail,
	}
	if out.State == "" {
		out.State = string(alert.OK)
	}
	if ind.read {
		v := ind.reading.Value
		out.Value = &v
	}
	if ind.err != nil {
		out.Error = ind.err.Error()
	}
	if now.Before(ind.snoozedUntil) {
		t := ind.snoozedUntil
		out.SnoozedUntil = &t
	}
	cutoff := now.Add(-sparkWindow).Unix()
	for _, p := range ind.ring {
		if p.T >= cutoff {
			out.Spark = append(out.Spark, p.V)
		}
	}
	if len(top) > 0 {
		detail := map[string]any{}
		for k, v := range out.Detail {
			detail[k] = v
		}
		detail["top_processes"] = top
		out.Detail = detail
	}
	return out
}

func (d *Daemon) snooze(id, duration string) error {
	var until time.Time
	untilResolved := duration == api.UntilResolved
	if !untilResolved {
		dur, err := time.ParseDuration(duration)
		if err != nil || dur <= 0 {
			return fmt.Errorf("invalid duration %q: use e.g. 30m, 1h or %s", duration, api.UntilResolved)
		}
		until = time.Now().Add(dur)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, ind := range d.inds {
		if ind.id == id {
			ind.snoozedUntil, ind.snoozeUntilResolved = until, untilResolved
			slog.Info("snoozed", "indicator", id, "duration", duration)
			return nil
		}
	}
	return errUnknownIndicator(id)
}

func (d *Daemon) has(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, ind := range d.inds {
		if ind.id == id {
			return true
		}
	}
	return false
}

func (d *Daemon) ring(id string, since time.Time) []api.Point {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, ind := range d.inds {
		if ind.id != id {
			continue
		}
		var out []api.Point
		for _, p := range ind.ring {
			if p.T >= since.Unix() {
				out = append(out, p)
			}
		}
		return out
	}
	return nil
}

type errUnknownIndicator string

func (e errUnknownIndicator) Error() string { return fmt.Sprintf("unknown indicator %q", string(e)) }

func isUnknown(err error) bool {
	var e errUnknownIndicator
	return errors.As(err, &e)
}

func machineUptime() int64 {
	fields := strings.Fields(hw.ReadString("/proc/uptime"))
	if len(fields) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(fields[0], 64)
	return int64(v)
}
