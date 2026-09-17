package mirror_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeropsio/gitea-mate/internal/mirror"
)

// counter is a Passer that records how often it ran, how long each run
// overlapped another, and can be made slow.
type counter struct {
	mu       sync.Mutex
	runs     int
	inFlight int
	overlaps int
	slow     time.Duration
	err      error
	result   mirror.Result
}

func (c *counter) Pass(ctx context.Context) (mirror.Result, error) {
	c.mu.Lock()
	c.runs++
	c.inFlight++
	if c.inFlight > 1 {
		c.overlaps++
	}
	slow, err, result := c.slow, c.err, c.result
	c.mu.Unlock()

	if slow > 0 {
		time.Sleep(slow)
	}

	c.mu.Lock()
	c.inFlight--
	c.mu.Unlock()
	return result, err
}

func (c *counter) counts() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.runs, c.overlaps
}

func TestLoopRunsOnStartAndOnEveryTick(t *testing.T) {
	p := &counter{}
	loop := mirror.NewLoop(p, slog.New(slog.DiscardHandler), 20*time.Millisecond, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { loop.Run(ctx); close(done) }()

	// The first pass is immediate, so a fresh container converges without
	// waiting out an interval.
	waitUntil(t, func() bool { runs, _ := p.counts(); return runs >= 1 })
	waitUntil(t, func() bool { runs, _ := p.counts(); return runs >= 3 })

	cancel()
	<-done
}

func TestANudgeRunsAPassShortlyAfter(t *testing.T) {
	p := &counter{}
	loop := mirror.NewLoop(p, slog.New(slog.DiscardHandler), time.Hour, 10*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { loop.Run(ctx); close(done) }()

	waitUntil(t, func() bool { runs, _ := p.counts(); return runs == 1 })
	loop.Nudge()
	waitUntil(t, func() bool { runs, _ := p.counts(); return runs == 2 })

	cancel()
	<-done
}

// Several issuances in a row are one pass, a few moments after the last of
// them — not one pass each.
func TestNudgesCoalesce(t *testing.T) {
	p := &counter{}
	loop := mirror.NewLoop(p, slog.New(slog.DiscardHandler), time.Hour, 60*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { loop.Run(ctx); close(done) }()
	waitUntil(t, func() bool { runs, _ := p.counts(); return runs == 1 })

	for range 10 {
		loop.Nudge()
		time.Sleep(2 * time.Millisecond)
	}
	waitUntil(t, func() bool { runs, _ := p.counts(); return runs == 2 })
	time.Sleep(100 * time.Millisecond)
	if runs, _ := p.counts(); runs != 2 {
		t.Errorf("ten nudges made %d passes, want 2 (the start and one more)", runs)
	}

	cancel()
	<-done
}

// Passes never overlap: one goroutine owns every pass, so a slow one simply
// delays the next.
func TestPassesNeverOverlap(t *testing.T) {
	p := &counter{slow: 15 * time.Millisecond}
	loop := mirror.NewLoop(p, slog.New(slog.DiscardHandler), 2*time.Millisecond, 2*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { loop.Run(ctx); close(done) }()

	for range 50 {
		loop.Nudge()
		time.Sleep(time.Millisecond)
	}
	waitUntil(t, func() bool { runs, _ := p.counts(); return runs >= 3 })
	cancel()
	<-done

	if _, overlaps := p.counts(); overlaps != 0 {
		t.Errorf("%d passes overlapped", overlaps)
	}
}

func TestRunStopsWithItsContext(t *testing.T) {
	p := &counter{}
	loop := mirror.NewLoop(p, slog.New(slog.DiscardHandler), time.Millisecond, time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { loop.Run(ctx); close(done) }()
	waitUntil(t, func() bool { runs, _ := p.counts(); return runs >= 2 })
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return when its context was cancelled")
	}
}

// A pass's outcome is logged as counts. Nothing that names a credential goes
// anywhere near it.
func TestOutcomeIsLoggedAsCounts(t *testing.T) {
	var buf bytes.Buffer
	p := &counter{result: mirror.Result{
		Planned: 4, Applied: 4, Destructive: 1,
		Plan: mirror.Plan{Actions: []mirror.Action{
			{Kind: mirror.DeleteBotToken, Login: "mate-p-fen", TokenName: "mate/mate-p-fen/1"},
		}},
	}}
	loop := mirror.NewLoop(p, slog.New(slog.NewJSONHandler(&buf, nil)), time.Hour, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { loop.Run(ctx); close(done) }()
	waitUntil(t, func() bool { runs, _ := p.counts(); return runs == 1 })
	cancel()
	<-done

	line := buf.String()
	for _, want := range []string{`"planned":4`, `"applied":4`, `"destructive":1`} {
		if !strings.Contains(line, want) {
			t.Errorf("the log lacks %s: %s", want, line)
		}
	}
	for _, unwanted := range []string{"mate/mate-p-fen/1", "mate-p-fen", "delete_bot_token"} {
		if strings.Contains(line, unwanted) {
			t.Errorf("the log names %q: %s", unwanted, line)
		}
	}
}

func TestACappedPassIsWarnedNotFailed(t *testing.T) {
	var buf bytes.Buffer
	p := &counter{err: errors.New("wrapped: " + mirror.ErrCapped.Error()), result: mirror.Result{Destructive: 99}}
	p.err = wrap(mirror.ErrCapped)
	loop := mirror.NewLoop(p, slog.New(slog.NewJSONHandler(&buf, nil)), time.Hour, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { loop.Run(ctx); close(done) }()
	waitUntil(t, func() bool { runs, _ := p.counts(); return runs == 1 })
	cancel()
	<-done

	if !strings.Contains(buf.String(), `"level":"WARN"`) {
		t.Errorf("a capped pass was not a warning: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "cap") {
		t.Errorf("the warning does not mention the cap: %s", buf.String())
	}
}

func TestAnUnreadableOrgIsWarnedNotFailed(t *testing.T) {
	var buf bytes.Buffer
	p := &counter{err: wrap(mirror.ErrUnreadable)}
	loop := mirror.NewLoop(p, slog.New(slog.NewJSONHandler(&buf, nil)), time.Hour, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { loop.Run(ctx); close(done) }()
	waitUntil(t, func() bool { runs, _ := p.counts(); return runs == 1 })
	cancel()
	<-done

	if !strings.Contains(buf.String(), "wrote nothing") {
		t.Errorf("the warning does not say nothing was written: %s", buf.String())
	}
}

func TestNewLoopDefaults(t *testing.T) {
	loop := mirror.NewLoop(&counter{}, nil, 0, 0)
	if loop.Interval != mirror.DefaultInterval {
		t.Errorf("Interval = %v, want %v", loop.Interval, mirror.DefaultInterval)
	}
	if loop.NudgeDelay != mirror.DefaultNudgeDelay {
		t.Errorf("NudgeDelay = %v, want %v", loop.NudgeDelay, mirror.DefaultNudgeDelay)
	}
	// A nudge before Run never blocks, and never panics.
	loop.Nudge()
	loop.Nudge()
}

func wrap(err error) error { return errors.Join(err, errors.New("the details")) }

func waitUntil(t *testing.T, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if done() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the condition never held")
}

func TestRunNowRunsAPassAndNeverOverlapsTheTicker(t *testing.T) {
	p := &counter{slow: 20 * time.Millisecond}
	loop := mirror.NewLoop(p, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), 15*time.Millisecond, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go loop.Run(ctx)

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := loop.RunNow(ctx); err != nil {
				t.Errorf("RunNow: %v", err)
			}
		}()
	}
	wg.Wait()
	cancel()

	runs, overlaps := p.counts()
	if runs < 4 {
		t.Errorf("runs = %d, want at least the four RunNow asked for", runs)
	}
	if overlaps != 0 {
		t.Errorf("%d passes overlapped", overlaps)
	}
}

func TestRunNowRefusesADeadContext(t *testing.T) {
	p := &counter{}
	loop := mirror.NewLoop(p, nil, time.Hour, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := loop.RunNow(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("RunNow on a dead context: %v, want context.Canceled", err)
	}
	if runs, _ := p.counts(); runs != 0 {
		t.Errorf("runs = %d, want 0", runs)
	}
}
