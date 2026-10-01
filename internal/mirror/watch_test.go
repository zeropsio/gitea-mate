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

// registryWatch is what a loop's Watch reads: the registry as the Gitea
// project carries it now, settable by the test.
type registryWatch struct {
	mu    sync.Mutex
	now   string
	err   error
	reads int
}

func (w *registryWatch) read(context.Context) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.reads++
	return w.now, w.err
}

func (w *registryWatch) set(registry string, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.now, w.err = registry, err
}

func (w *registryWatch) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.reads
}

// setResult changes what the counter's next passes answer.
func (c *counter) setResult(r mirror.Result, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.result, c.err = r, err
}

// watchedLoop is a loop whose interval never ticks, so every pass after the
// first is the watch's.
func watchedLoop(t *testing.T, p *counter, w *registryWatch) *mirror.Loop {
	t.Helper()
	loop := mirror.NewLoop(p, slog.New(slog.DiscardHandler), time.Hour, time.Hour)
	loop.Watch = w.read
	loop.WatchInterval = 5 * time.Millisecond
	loop.ChaseInterval = 20 * time.Millisecond
	loop.ChaseFor = 150 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { loop.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return loop
}

// The press writes a Mate's registry tag and nothing after it. No Gitea hook
// fires on a Zerops tag, so without a watch the next tick — up to an interval
// later — was the first pass to see it. The watch reads the registry on a
// short cadence and runs a pass as soon as it differs from the one the last
// pass acted on, and runs none while it does not.
func TestARegistryChangeRunsAPassAtTheNextWatch(t *testing.T) {
	p := &counter{result: mirror.Result{Registry: "r1"}}
	w := &registryWatch{now: "r1"}
	watchedLoop(t, p, w)

	waitUntil(t, func() bool { runs, _ := p.counts(); return runs == 1 })
	waitUntil(t, func() bool { return w.count() >= 10 })
	if runs, _ := p.counts(); runs != 1 {
		t.Fatalf("an unchanged registry ran %d passes, want the first alone", runs)
	}

	p.setResult(mirror.Result{Registry: "r2"}, nil)
	w.set("r2", nil)
	waitUntil(t, func() bool { runs, _ := p.counts(); return runs == 2 })
	reads := w.count()
	waitUntil(t, func() bool { return w.count() >= reads+10 })
	if runs, _ := p.counts(); runs != 2 {
		t.Errorf("the change ran %d passes after the first, want one", runs-1)
	}
}

// A Mate the pass could not serve yet — its zcp not listed, a write refused —
// is passed for again on a shorter cadence for a while after the registry
// changed, and no longer once it is served or the while is over.
func TestAMateStillWaitingIsChasedForAWhileAfterTheChange(t *testing.T) {
	cases := []struct {
		name string
		// served is whether the Mate is served on the second chase pass.
		served bool
	}{
		{name: "until it is served", served: true},
		{name: "until the while is over", served: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &counter{result: mirror.Result{Registry: "r1"}}
			w := &registryWatch{now: "r1"}
			watchedLoop(t, p, w)
			waitUntil(t, func() bool { runs, _ := p.counts(); return runs == 1 })

			p.setResult(mirror.Result{Registry: "r2", MatesWaiting: 1}, nil)
			w.set("r2", nil)
			waitUntil(t, func() bool { runs, _ := p.counts(); return runs == 3 })
			if tc.served {
				p.setResult(mirror.Result{Registry: "r2"}, nil)
				waitUntil(t, func() bool { runs, _ := p.counts(); return runs == 4 })
			}
			time.Sleep(250 * time.Millisecond)
			runs, _ := p.counts()
			if tc.served && runs != 4 {
				t.Errorf("%d passes, want none after the Mate was served", runs)
			}
			// 150 ms of chase at 20 ms: about seven passes, never one a watch.
			if !tc.served && (runs < 4 || runs > 10) {
				t.Errorf("%d passes, want the chase's few and then none", runs)
			}
			before := runs
			time.Sleep(100 * time.Millisecond)
			if runs, _ := p.counts(); runs != before {
				t.Errorf("the chase went on: %d passes, then %d", before, runs)
			}
		})
	}
}

// A watch that cannot read, or a pass that fails on the change it was run
// for, leaves the loop on its interval: neither runs a pass every watch.
func TestAFailingWatchOrPassFallsBackToTheInterval(t *testing.T) {
	t.Run("the watch cannot read", func(t *testing.T) {
		p := &counter{result: mirror.Result{Registry: "r1"}}
		w := &registryWatch{now: "", err: errors.New("503")}
		watchedLoop(t, p, w)
		waitUntil(t, func() bool { return w.count() >= 10 })
		if runs, _ := p.counts(); runs != 1 {
			t.Errorf("%d passes, want the first alone", runs)
		}
	})
	t.Run("the pass fails", func(t *testing.T) {
		p := &counter{result: mirror.Result{Registry: "r1"}}
		w := &registryWatch{now: "r1"}
		watchedLoop(t, p, w)
		waitUntil(t, func() bool { runs, _ := p.counts(); return runs == 1 })

		p.setResult(mirror.Result{}, wrap(mirror.ErrUnreadable))
		w.set("r2", nil)
		waitUntil(t, func() bool { runs, _ := p.counts(); return runs == 2 })
		reads := w.count()
		waitUntil(t, func() bool { return w.count() >= reads+10 })
		if runs, _ := p.counts(); runs != 2 {
			t.Errorf("%d passes, want one for the change", runs)
		}
	})
}

// The watch reads what a pass reads: the registry on the Gitea project's
// tags, one call, so a change is a pass the moment it is seen. A tag outside
// the registry is no change.
func TestTheWatchReadsTheRegistryAPassActsOn(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()

	result := r.passAt(t, now)
	seen, err := r.mirror.Registry(ctx)
	if err != nil {
		t.Fatalf("Registry: %v", err)
	}
	if seen == "" || seen != result.Registry {
		t.Fatalf("the watch reads %q, the pass acted on %q", seen, result.Registry)
	}

	r.registry(append(rigTags(), "mate:standup:u-jan", "mate:tool:other")...)
	if again, _ := r.mirror.Registry(ctx); again != seen {
		t.Errorf("a tag outside the registry changed it: %q", again)
	}
	r.registry(append(rigTags(), "mate:gm:g-acme:p-new:mate")...)
	if again, _ := r.mirror.Registry(ctx); again == seen {
		t.Error("a Mate registered at a press is no change")
	}
}

// MatesWaiting is how many registered Mates a pass left without their Git
// access: one it could not read, or whose delivery failed.
func TestMatesWaitingCountsTheMatesAPassLeftUnserved(t *testing.T) {
	cases := []struct {
		name string
		bend func(r *rig)
		want int
	}{
		{name: "served", bend: func(*rig) {}, want: 0},
		{name: "no zcp service listed yet", bend: func(r *rig) { r.zerops.SetServices("p-fen") }, want: 1},
		{name: "its delivery refused", bend: func(r *rig) {
			r.zerops.Fail["POST /service-stack/"+zcpService+"/user-data"] = 403
		}, want: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			tc.bend(r)
			if got := r.passAt(t, now).MatesWaiting; got != tc.want {
				t.Errorf("MatesWaiting = %d, want %d", got, tc.want)
			}
		})
	}
}

// The search the org read rests on lags the platform's own reads by seconds
// (0.5–2.6 s, measured). The pass takes its registry from the read the watch
// makes, so a pass never acts on — and reports — a registry older than the
// one the watch saw, which would read as a change at every watch.
func TestThePassActsOnTheRegistryTheWatchSawWhileTheSearchLags(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.passAt(t, now)

	r.zerops.FreezeSearch()
	r.registry(append(rigTags(), "mate:release:g-acme:mates")...)
	seen, err := r.mirror.Registry(ctx)
	if err != nil {
		t.Fatalf("Registry: %v", err)
	}
	if got := r.passAt(t, now.Add(time.Second)).Registry; got != seen {
		t.Errorf("the pass acted on %q, the watch saw %q", got, seen)
	}
}

// A change any pass sees first — a tick's, a hook's nudge, a sign-in's — starts
// the chase as the watch's would: the watch then sees nothing new, and the
// Mate the pass left waiting must not wait out the interval.
func TestAChangeAnyPassSeesFirstStartsTheChase(t *testing.T) {
	triggers := []struct {
		name string
		run  func(loop *mirror.Loop)
	}{
		{name: "a nudge", run: func(loop *mirror.Loop) { loop.Nudge() }},
		{name: "RunNow", run: func(loop *mirror.Loop) { _, _ = loop.RunNow(context.Background()) }},
	}
	for _, tc := range triggers {
		t.Run(tc.name, func(t *testing.T) {
			p := &counter{result: mirror.Result{Registry: "r1"}}
			w := &registryWatch{now: "r1"}
			loop := mirror.NewLoop(p, slog.New(slog.DiscardHandler), time.Hour, time.Millisecond)
			loop.Watch = w.read
			loop.WatchInterval = 5 * time.Millisecond
			loop.ChaseInterval = 20 * time.Millisecond
			loop.ChaseFor = time.Hour
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { loop.Run(ctx); close(done) }()
			t.Cleanup(func() { cancel(); <-done })
			waitUntil(t, func() bool { runs, _ := p.counts(); return runs == 1 })

			// The watch is blind while the other pass sees the change.
			w.set("", errors.New("503"))
			p.setResult(mirror.Result{Registry: "r2", MatesWaiting: 1}, nil)
			tc.run(loop)
			waitUntil(t, func() bool { runs, _ := p.counts(); return runs == 2 })
			w.set("r2", nil)
			waitUntil(t, func() bool { runs, _ := p.counts(); return runs >= 4 })
		})
	}
}

// A watch that keeps failing is said at Info, so a broken one is seen; one
// failure alone is not.
func TestAWatchFailingTimeAfterTimeIsLogged(t *testing.T) {
	var buf safeBuffer
	p := &counter{result: mirror.Result{Registry: "r1"}}
	w := &registryWatch{err: errors.New("503")}
	loop := mirror.NewLoop(p, slog.New(slog.NewJSONHandler(&buf, nil)), time.Hour, time.Hour)
	loop.Watch = w.read
	loop.WatchInterval = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { loop.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	waitUntil(t, func() bool { return w.count() >= mirror.WatchFailuresBeforeLog })
	waitUntil(t, func() bool { return strings.Contains(buf.String(), `"level":"INFO","msg":"the rights loop's watch`) })
}

// safeBuffer is a log sink the loop's goroutine and the test share.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
