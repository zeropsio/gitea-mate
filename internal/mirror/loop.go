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

// DefaultWatchInterval is how often the loop's watch reads the registry. The
// press that adds a Mate writes its registry tag and nothing after it, and a
// Zerops tag fires no Gitea hook, so without the watch the next tick — up to
// an interval later — was the first pass to see a new Mate. One read of one
// project, on the token the broker already holds.
const DefaultWatchInterval = 15 * time.Second

// DefaultChaseInterval and DefaultChaseFor are how often, and for how long
// after the registry changed, the loop passes again while a registered Mate
// still waits for its Git access — its zcp not listed yet, a write refused.
const (
	DefaultChaseInterval = 30 * time.Second
	DefaultChaseFor      = 5 * time.Minute
)

// WatchFailuresBeforeLog is how many failed watch reads in a row are said at
// Info, and said again at every multiple: one failure is noise, a run of them
// a watch that is broken.
const WatchFailuresBeforeLog = 4

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

	// Watch reads the registry as it stands now ([Mirror.Registry]); nil
	// leaves the loop on its interval and its nudges. A registry that differs
	// from the one the last pass acted on is a pass at once, and a chase:
	// while that pass leaves a Mate waiting, another every ChaseInterval, for
	// ChaseFor. Every zero duration takes its default.
	Watch         func(ctx context.Context) (string, error)
	WatchInterval time.Duration
	ChaseInterval time.Duration
	ChaseFor      time.Duration

	nudges chan struct{}
	// passMu serialises passes: the ticker's, a nudge's and RunNow's never
	// overlap, whichever goroutine asks.
	passMu sync.Mutex

	// What the watch compares with, under lastMu: the registry last seen, by
	// the watch or by a pass; the registry the last pass acted on, how many
	// Mates it left waiting and when it ended; and until when a chase runs.
	lastMu      sync.Mutex
	lastSeen    string
	lastActed   string
	lastWaiting int
	lastAt      time.Time
	chaseUntil  time.Time

	// watchFailures is how many watch reads in a row failed; the Run
	// goroutine alone touches it.
	watchFailures int
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

	var watch <-chan time.Time
	if l.Watch != nil {
		watchTicker := time.NewTicker(orDefault(l.WatchInterval, DefaultWatchInterval))
		defer watchTicker.Stop()
		watch = watchTicker.C
	}
	l.pass(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-watch:
			if l.watch(ctx) {
				l.pass(ctx)
			}
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

// watch reads the registry and says whether a pass is due: the registry
// differs from the one last seen — which starts a chase — or a chase is on, a
// Mate still waits and the last pass is a chase interval old. A registry the
// watch cannot read is no reason for a pass. A changed one is taken as seen
// before its pass runs, so a pass that fails on it is retried by the
// interval, not by every watch.
func (l *Loop) watch(ctx context.Context) bool {
	seen, err := l.Watch(ctx)
	if err != nil {
		l.watchFailures++
		if l.watchFailures%WatchFailuresBeforeLog == 0 {
			l.Log.Info("the rights loop's watch cannot read the registry; a new registration waits for the interval",
				"failures_in_a_row", l.watchFailures, "err", err.Error())
		}
		return false
	}
	l.watchFailures = 0
	now := time.Now()
	l.lastMu.Lock()
	defer l.lastMu.Unlock()
	if seen != l.lastSeen {
		l.lastSeen = seen
		l.chaseUntil = now.Add(orDefault(l.ChaseFor, DefaultChaseFor))
		return true
	}
	return l.lastWaiting > 0 && now.Before(l.chaseUntil) &&
		now.Sub(l.lastAt) >= orDefault(l.ChaseInterval, DefaultChaseInterval)
}

func orDefault(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// settle records what a pass left, whichever trigger ran it: a registry other
// than the last pass's, or more Mates waiting than it left, starts a chase —
// the watch then sees no change, and the Mate must not wait out the interval.
// The pass reads its registry where the watch does, so what it saw is never
// older than what the watch saw before it.
func (l *Loop) settle(result Result, err error) {
	now := time.Now()
	l.lastMu.Lock()
	defer l.lastMu.Unlock()
	l.lastAt = now
	if err != nil {
		return
	}
	if result.Registry != l.lastActed || result.MatesWaiting > l.lastWaiting {
		l.chaseUntil = now.Add(orDefault(l.ChaseFor, DefaultChaseFor))
	}
	l.lastSeen, l.lastActed, l.lastWaiting = result.Registry, result.Registry, result.MatesWaiting
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
	result, err := l.Passer.Pass(ctx)
	l.settle(result, err)
	return result, err
}

func (l *Loop) pass(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	l.passMu.Lock()
	result, err := l.Passer.Pass(ctx)
	l.passMu.Unlock()
	l.settle(result, err)
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
