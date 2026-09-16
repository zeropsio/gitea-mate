package deploy

import (
	"context"
	"log/slog"
	"sync"
)

// Queue is one queue per environment, newest wins.
//
// Two deploys of one environment never run at once — the platform would race
// two app versions onto the same service — and an older request still waiting
// when a newer one arrives is dropped, because deploying the older commit
// after the newer one is exactly the wrong outcome. The dropped request's
// deploy records are carried over to the newer job, so a workflow polling one
// of them learns what the environment actually settled on rather than being
// told its request vanished.
type Queue struct {
	// base is the context every job runs on. It is the broker's own — never
	// the caller's: Submit does not block, so a webhook dispatch or an HTTP
	// handler has returned (and cancelled its context) long before the deploy
	// has read anything. Measured live on 2026-09-16, where every
	// webhook-driven deploy died on "context canceled" and only the catch-up
	// pass, which runs on the process's context, ever deployed anything.
	base context.Context
	run  func(context.Context, Job)
	log  *slog.Logger

	mu      sync.Mutex
	pending map[string]*Job
	running map[string]bool
	live    sync.WaitGroup
}

// NewQueue builds a queue that performs jobs with run. base is the context the
// jobs run on — the broker's, for as long as the broker lives.
func NewQueue(base context.Context, run func(context.Context, Job), log *slog.Logger) *Queue {
	if log == nil {
		log = slog.Default()
	}
	if base == nil {
		base = context.Background()
	}
	return &Queue{base: base, run: run, log: log, pending: map[string]*Job{}, running: map[string]bool{}}
}

// Submit queues a job. It never blocks, and the job it queues does not belong
// to whoever asked for it: it runs on the queue's own context.
func (q *Queue) Submit(job Job) {
	key := job.Key()

	q.mu.Lock()
	if waiting, ok := q.pending[key]; ok {
		// Newest wins: the waiting request is dropped, and whoever was asking
		// about it now follows this one.
		job.Records = append(append([]string(nil), waiting.Records...), job.Records...)
		q.log.Info("a queued deploy was superseded",
			"environment", key, "dropped", waiting.Sha(), "kept", job.Sha())
	}
	if q.running[key] {
		q.pending[key] = &job
		q.mu.Unlock()
		return
	}
	q.running[key] = true
	q.mu.Unlock()

	q.live.Add(1)
	go q.loop(q.base, key, job)
}

// loop performs one job and then whatever queued behind it, until nothing is
// waiting for this environment.
func (q *Queue) loop(ctx context.Context, key string, job Job) {
	defer q.live.Done()
	for {
		q.run(ctx, job)

		q.mu.Lock()
		next, waiting := q.pending[key]
		if !waiting {
			delete(q.running, key)
			q.mu.Unlock()
			return
		}
		delete(q.pending, key)
		q.mu.Unlock()
		job = *next
	}
}

// Wait blocks until every queued job has been performed. It exists for tests
// and for a clean shutdown; nothing in a request path calls it.
func (q *Queue) Wait() { q.live.Wait() }
