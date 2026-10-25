// Package alert implements the OK → PENDING → FIRING → RESOLVED state machine
// shared by every indicator.
package alert

import "time"

type State string

const (
	OK      State = "ok"
	Pending State = "pending"
	Firing  State = "firing"
)

type Rule struct {
	// Below means lower values are worse (battery). Otherwise higher is worse.
	Below     bool
	FireAt    float64
	For       time.Duration
	ResolveAt float64
	// CriticalAt is optional; nil means the indicator has no critical level.
	CriticalAt *float64
	Repeat     time.Duration
}

func (r Rule) worse(v, threshold float64) bool {
	if r.Below {
		return v <= threshold
	}
	return v >= threshold
}

// Event is what the caller should tell the user after an update.
type Event string

const (
	None      Event = ""
	Fired     Event = "fired"
	Escalated Event = "escalated" // became critical (from OK, PENDING or FIRING)
	Repeated  Event = "repeated"  // still firing, reminder due
	Resolved  Event = "resolved"
	Cleared   Event = "cleared" // stopped applying (e.g. charger plugged in); no notification
)

type Alert struct {
	State        State     `json:"state"`
	Critical     bool      `json:"critical"`
	Since        time.Time `json:"since"` // when the current state started
	LastNotified time.Time `json:"-"`
}

// Update feeds one sample into the state machine. active=false means the
// rule doesn't apply right now; any alert is cleared without a notification.
func (a *Alert) Update(r Rule, v float64, active bool, now time.Time) Event {
	if a.State == "" {
		a.State, a.Since = OK, now
	}

	if !active {
		if a.State == OK {
			return None
		}
		a.set(OK, now)
		return Cleared
	}

	if r.CriticalAt != nil && r.worse(v, *r.CriticalAt) {
		if a.State != Firing || !a.Critical {
			if a.State != Firing {
				a.set(Firing, now)
			}
			a.Critical = true
			a.LastNotified = now
			return Escalated
		}
		return a.repeat(r, now)
	}

	if r.worse(v, r.FireAt) {
		switch a.State {
		case Firing:
			// Back below critical but still above fire: stay firing, quietly.
			a.Critical = false
			return a.repeat(r, now)
		case OK:
			a.set(Pending, now)
		}
		if now.Sub(a.Since) >= r.For {
			a.set(Firing, now)
			a.LastNotified = now
			return Fired
		}
		return None
	}

	switch a.State {
	case Firing:
		// Hysteresis: stay firing until the value is clearly better.
		if r.worse(v, r.ResolveAt) {
			a.Critical = false
			return None
		}
		a.set(OK, now)
		a.LastNotified = now
		return Resolved
	case Pending:
		a.set(OK, now)
	}
	return None
}

func (a *Alert) repeat(r Rule, now time.Time) Event {
	if r.Repeat > 0 && now.Sub(a.LastNotified) >= r.Repeat {
		a.LastNotified = now
		return Repeated
	}
	return None
}

func (a *Alert) set(s State, now time.Time) {
	a.State, a.Since = s, now
	if s != Firing {
		a.Critical = false
	}
}
