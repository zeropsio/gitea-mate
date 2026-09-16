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
	run func(context.Context, Job)
	log *slog.Logger

	mu      sync.Mutex
	pending map[string]*Job
	running map[string]bool
	live    sync.WaitGroup
}

// NewQueue builds a queue that performs jobs with run.
func NewQueue(run func(context.Context, Job), log *slog.Logger) *Queue {
	if log == nil {
		log = slog.Default()
	}
	return &Queue{run: run, log: log, pending: map[string]*Job{}, running: map[string]bool{}}
}

// Submit queues a job. It never blocks.
func (q *Queue) Submit(ctx context.Context, job Job) {
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
	go q.loop(ctx, key, job)
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
