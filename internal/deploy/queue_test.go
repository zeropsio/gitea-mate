package deploy_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeropsio/gitea-mate/internal/deploy"
	"github.com/zeropsio/gitea-mate/internal/environments"
)

func job(slug, env, sha string) deploy.Job {
	return deploy.Job{
		Slug:        slug,
		Environment: environments.Environment{Name: env, Tier: environments.TierStage},
		Targets:     []deploy.Target{{Service: "api", Sha: sha}},
	}
}

func TestQueueNewestWins(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var ran []string
	release := make(chan struct{})
	started := make(chan struct{}, 1)

	queue := deploy.NewQueue(context.Background(), func(_ context.Context, j deploy.Job) {
		mu.Lock()
		ran = append(ran, j.Sha())
		first := len(ran) == 1
		mu.Unlock()
		if first {
			started <- struct{}{}
			<-release
		}
	}, nil)

	queue.Submit(job("acme", "stage", "aaa"))
	<-started // the first one is now running and will not finish until released

	queue.Submit(job("acme", "stage", "bbb"))
	queue.Submit(job("acme", "stage", "ccc"))
	close(release)
	queue.Wait()

	mu.Lock()
	defer mu.Unlock()
	if strings.Join(ran, ",") != "aaa,ccc" {
		t.Fatalf("the queue ran %v, want the first and then only the newest", ran)
	}
}

// TestQueueKeepsAPersonsAskThroughASupersedingJob — a pass's job that replaces
// a waiting release still carries the person's ask, or a release that queued
// behind another job would be dropped by the failure it was meant to retry.
func TestQueueKeepsAPersonsAskThroughASupersedingJob(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var ran []deploy.Job
	release := make(chan struct{})
	started := make(chan struct{}, 1)

	queue := deploy.NewQueue(context.Background(), func(_ context.Context, j deploy.Job) {
		mu.Lock()
		ran = append(ran, j)
		first := len(ran) == 1
		mu.Unlock()
		if first {
			started <- struct{}{}
			<-release
		}
	}, nil)

	queue.Submit(job("acme", "production", "aaa"))
	<-started

	requested := job("acme", "production", "bbb")
	requested.Requested = true
	queue.Submit(requested)
	queue.Submit(job("acme", "production", "bbb"))
	close(release)
	queue.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(ran) != 2 || !ran[1].Requested {
		t.Fatalf("the queue ran %+v, want the superseding job to carry the person's ask", ran)
	}
}

func TestQueueRunsTwoEnvironmentsAtOnce(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	inFlight, peak := 0, 0
	hold := make(chan struct{})

	queue := deploy.NewQueue(context.Background(), func(_ context.Context, _ deploy.Job) {
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mu.Unlock()
		<-hold
		mu.Lock()
		inFlight--
		mu.Unlock()
	}, nil)

	queue.Submit(job("acme", "stage", "aaa"))
	queue.Submit(job("acme", "production", "bbb"))
	queue.Submit(job("other", "stage", "ccc"))

	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		got := peak
		mu.Unlock()
		if got == 3 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	close(hold)
	queue.Wait()

	mu.Lock()
	defer mu.Unlock()
	if peak != 3 {
		t.Fatalf("at most %d deploys ran at once, want the three environments in parallel", peak)
	}
}

func TestRecordsForgetTheOldest(t *testing.T) {
	t.Parallel()
	records := deploy.NewRecords(2)
	grant := func(sha string) deploy.Record {
		return records.New(deploy.Record{Environment: "stage", Service: "api", Repository: "acme/api", Sha: sha})
	}
	first, second, third := grant("aaa"), grant("bbb"), grant("ccc")

	if _, ok := records.Get(first.ID); ok {
		t.Fatal("the oldest record survived the limit")
	}
	for _, rec := range []deploy.Record{second, third} {
		if _, ok := records.Get(rec.ID); !ok {
			t.Fatalf("%s was forgotten", rec.ID)
		}
	}
	if first.Status != deploy.StatusGranted {
		t.Fatalf("a new record is %q, want granted", first.Status)
	}

	records.Update(third.ID, func(r *deploy.Record) { r.Status = deploy.StatusSucceeded; r.Message = "live" })
	back, _ := records.Get(third.ID)
	if back.Status != deploy.StatusSucceeded || back.Message != "live" {
		t.Fatalf("Update left %+v", back)
	}
	// Updating one the store has forgotten is not an error: a deploy still
	// finishes when nobody is left to ask about it.
	records.Update(first.ID, func(r *deploy.Record) { r.Status = deploy.StatusFailed })
}

func TestRecordIDsAreDistinct(t *testing.T) {
	t.Parallel()
	records := deploy.NewRecords(0)
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		id := records.New(deploy.Record{Environment: "stage", Service: "api", Repository: "acme/api", Sha: "aaa"}).ID
		if !strings.HasPrefix(id, "d_") {
			t.Fatalf("id = %q, want a d_ prefix", id)
		}
		if seen[id] {
			t.Fatalf("id %q came back twice", id)
		}
		seen[id] = true
	}
}

// TestQueueOutlivesTheRequestThatAskedForIt pins what broke live on
// 2026-09-16: every webhook-driven deploy failed with "context canceled". The
// queue ran its jobs on the caller's context — a webhook dispatch's, or an
// HTTP request's — and Submit does not block, so the context was cancelled the
// instant the caller returned and the deploy died on its first API read. A
// deploy outlives the request that asked for it, or it does not happen.
func TestQueueOutlivesTheRequestThatAskedForIt(t *testing.T) {
	t.Parallel()

	done := make(chan error, 1)
	queue := deploy.NewQueue(context.Background(), func(ctx context.Context, _ deploy.Job) {
		// What a real job does first: a call that takes a context.
		time.Sleep(20 * time.Millisecond)
		done <- ctx.Err()
	}, nil)

	// The caller's context, cancelled as soon as it has submitted — exactly
	// what `defer cancel()` in a webhook dispatch does.
	caller, cancel := context.WithCancel(context.Background())
	queue.Submit(job("acme", "stage", "aaa"))
	cancel()
	_ = caller

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the job ran on a dead context: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the job never ran")
	}
	queue.Wait()
}
