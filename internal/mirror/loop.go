package mirror

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// DefaultInterval is how often the rights loop runs a pass.
const DefaultInterval = 3 * time.Minute

// DefaultNudgeDelay is how long after a token issuance the loop runs one. The
// teams are written by the loop, so a person is in the right ones by the time
// they have looked at the page.
const DefaultNudgeDelay = 5 * time.Second

// Passer is what a [Loop] drives. [*Mirror] implements it.
type Passer interface {
	Pass(ctx context.Context) (Result, error)
}

// Loop runs passes on a timer and shortly after a nudge. One goroutine owns
// every pass, so two never overlap; a nudge that arrives while a pass is
// running simply queues the next one.
//
// There is no poke endpoint: the timer and the signed webhooks are the only
// triggers, and nothing a runner job can reach starts a pass.
type Loop struct {
	Passer Passer
	Log    *slog.Logger
	// Interval defaults to DefaultInterval.
	Interval time.Duration
	// NudgeDelay defaults to DefaultNudgeDelay.
	NudgeDelay time.Duration

	nudges chan struct{}
	// passMu serialises passes: the ticker's, a nudge's and RunNow's never
	// overlap, whichever goroutine asks.
	passMu sync.Mutex
}

// NewLoop builds a loop.
func NewLoop(p Passer, log *slog.Logger, interval, nudgeDelay time.Duration) *Loop {
	if interval <= 0 {
		interval = DefaultInterval
	}
	if nudgeDelay <= 0 {
		nudgeDelay = DefaultNudgeDelay
	}
	if log == nil {
		log = slog.Default()
	}
	return &Loop{
		Passer: p, Log: log, Interval: interval, NudgeDelay: nudgeDelay,
		nudges: make(chan struct{}, 1),
	}
}

// Nudge asks for a pass a few seconds from now. It never blocks: several
// issuances in a row coalesce into one pass.
func (l *Loop) Nudge() {
	select {
	case l.nudges <- struct{}{}:
	default:
	}
}

// Run makes a pass immediately — a fresh container converges without waiting
// out an interval — and then on every tick and every nudge, until ctx is done.
func (l *Loop) Run(ctx context.Context) {
	ticker := time.NewTicker(l.Interval)
	defer ticker.Stop()

	nudge := time.NewTimer(l.NudgeDelay)
	if !nudge.Stop() {
		<-nudge.C
	}
	defer nudge.Stop()

	l.pass(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.pass(ctx)
		case <-l.nudges:
			// Re-arm: a burst of issuances is one pass, a few seconds after the
			// last of them.
			if !nudge.Stop() {
				select {
				case <-nudge.C:
				default:
				}
			}
			nudge.Reset(l.NudgeDelay)
		case <-nudge.C:
			l.pass(ctx)
		}
	}
}

// pass runs one pass and logs its outcome as counts. Never a name of a token.
// RunNow runs one pass on the caller's goroutine and returns its outcome. It
// waits for a pass already under way rather than overlapping it. A route that
// just made an account true asks for this, so the person is in their teams by
// the time the route answers, instead of one nudge delay later.
func (l *Loop) RunNow(ctx context.Context) (Result, error) {
	l.passMu.Lock()
	defer l.passMu.Unlock()
	if ctx.Err() != nil {
		return Result{}, ctx.Err()
	}
	return l.Passer.Pass(ctx)
}

func (l *Loop) pass(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	l.passMu.Lock()
	result, err := l.Passer.Pass(ctx)
	l.passMu.Unlock()
	switch {
	case errors.Is(err, ErrCapped):
		// The plan is in the result for a person to read; the log says how many,
		// not who.
		l.Log.Warn("the rights loop stopped at the cap", "result", result, "err", err.Error())
	case errors.Is(err, ErrUnreadable):
		l.Log.Warn("the rights loop could not read the org, so it wrote nothing", "err", err.Error())
	case err != nil:
		l.Log.Error("the rights loop failed", "err", err.Error())
	default:
		l.Log.Info("rights loop pass", "result", result)
	}
}
