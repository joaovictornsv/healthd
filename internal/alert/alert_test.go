package alert

import (
	"testing"
	"time"
)

func ptr(v float64) *float64 { return &v }

type step struct {
	at     time.Duration
	value  float64
	active bool
	event  Event
	state  State
	crit   bool
}

func run(t *testing.T, r Rule, steps []step) {
	t.Helper()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var a Alert
	for i, s := range steps {
		got := a.Update(r, s.value, s.active, start.Add(s.at))
		if got != s.event || a.State != s.state || a.Critical != s.crit {
			t.Fatalf("step %d (t=%s v=%v): got event=%q state=%s crit=%v, want event=%q state=%s crit=%v",
				i, s.at, s.value, got, a.State, a.Critical, s.event, s.state, s.crit)
		}
	}
}

var tempRule = Rule{FireAt: 80, For: 5 * time.Minute, ResolveAt: 70, CriticalAt: ptr(92), Repeat: 10 * time.Minute}

func TestPendingThenFiringThenResolved(t *testing.T) {
	run(t, tempRule, []step{
		{0, 50, true, None, OK, false},
		{1 * time.Minute, 82, true, None, Pending, false},
		{3 * time.Minute, 85, true, None, Pending, false},
		{6 * time.Minute, 83, true, Fired, Firing, false},
		{8 * time.Minute, 75, true, None, Firing, false}, // between resolve and fire: stays firing
		{9 * time.Minute, 69, true, Resolved, OK, false},
	})
}

func TestPendingResetsWhenValueDrops(t *testing.T) {
	run(t, tempRule, []step{
		{0, 82, true, None, Pending, false},
		{2 * time.Minute, 60, true, None, OK, false},
		{3 * time.Minute, 82, true, None, Pending, false},
		{7 * time.Minute, 82, true, None, Pending, false}, // only 4m since pending restarted
		{8 * time.Minute, 82, true, Fired, Firing, false},
	})
}

func TestCriticalSkipsPendingAndEscalates(t *testing.T) {
	run(t, tempRule, []step{
		{0, 95, true, Escalated, Firing, true},
		{1 * time.Minute, 96, true, None, Firing, true},
		{11 * time.Minute, 95, true, Repeated, Firing, true},
		{12 * time.Minute, 85, true, None, Firing, false}, // de-escalates quietly
		{13 * time.Minute, 93, true, Escalated, Firing, true},
		{14 * time.Minute, 60, true, Resolved, OK, false},
	})
}

func TestRepeatWhileFiring(t *testing.T) {
	run(t, Rule{FireAt: 80, ResolveAt: 70, Repeat: 10 * time.Minute}, []step{
		{0, 85, true, Fired, Firing, false},
		{5 * time.Minute, 85, true, None, Firing, false},
		{10 * time.Minute, 85, true, Repeated, Firing, false},
		{15 * time.Minute, 85, true, None, Firing, false},
		{20 * time.Minute, 85, true, Repeated, Firing, false},
	})
}

func TestBelowDirectionAndInactiveClears(t *testing.T) {
	battery := Rule{Below: true, FireAt: 20, ResolveAt: 25, CriticalAt: ptr(10), Repeat: 10 * time.Minute}
	run(t, battery, []step{
		{0, 50, true, None, OK, false},
		{1 * time.Minute, 19, true, Fired, Firing, false},
		{2 * time.Minute, 9, true, Escalated, Firing, true},
		{3 * time.Minute, 9, false, Cleared, OK, false}, // plugged in
		{4 * time.Minute, 9, false, None, OK, false},
	})
}
