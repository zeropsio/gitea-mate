package deploy

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
)

// The statuses of docs/broker-api.md § GET /deploy/{id}.
const (
	StatusQueued  = "queued"
	StatusRunning = "running"
	StatusActive  = "active"
	StatusFailed  = "failed"
)

// Record is one deploy as the endpoints answer it.
type Record struct {
	ID          string `json:"id"`
	Environment string `json:"environment"`
	Service     string `json:"service"`
	Sha         string `json:"sha"`
	Status      string `json:"status"`
	VersionID   string `json:"versionId,omitempty"`
	Message     string `json:"message"`
	// Repository is the service repository the commit status is written on. It
	// is not part of the answer; it is how a poll finds its own commit.
	Repository string `json:"-"`
}

// DefaultRecordLimit is how many deploys the broker remembers. Beyond it the
// oldest is forgotten, which is the same thing a restart does — and the action
// falls back to the commit status either way (docs/broker-api.md).
const DefaultRecordLimit = 2000

// Records is the broker's memory of its deploys. There is no database: a
// restart forgets, an unknown id is 404, and the commit status the broker
// wrote on every outcome says the same thing.
type Records struct {
	mu    sync.Mutex
	byID  map[string]*Record
	order []string
	limit int
}

// NewRecords builds a store. limit ≤ 0 means [DefaultRecordLimit].
func NewRecords(limit int) *Records {
	if limit <= 0 {
		limit = DefaultRecordLimit
	}
	return &Records{byID: map[string]*Record{}, limit: limit}
}

// New records a queued deploy and returns it.
func (r *Records) New(environment, service, repository, sha string) Record {
	rec := Record{
		ID: newID(), Environment: environment, Service: service,
		Repository: repository, Sha: sha, Status: StatusQueued,
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	stored := rec
	r.byID[rec.ID] = &stored
	r.order = append(r.order, rec.ID)
	for len(r.order) > r.limit {
		delete(r.byID, r.order[0])
		r.order = r.order[1:]
	}
	return rec
}

// Get reads one back. A deploy the broker has forgotten is not found, which
// the endpoint answers as 404.
func (r *Records) Get(id string) (Record, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.byID[id]
	if !ok {
		return Record{}, false
	}
	return *rec, true
}

// Update changes a record in place. An id the store has forgotten is ignored:
// a deploy still finishes when nobody is left to ask about it.
func (r *Records) Update(id string, change func(*Record)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rec, ok := r.byID[id]; ok {
		change(rec)
	}
}

// UpdateAll applies one change to several records.
func (r *Records) UpdateAll(ids []string, change func(*Record)) {
	for _, id := range ids {
		r.Update(id, change)
	}
}

// newID is the "d_…" of the contract.
func newID() string {
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand does not fail on any platform the broker runs on; if it
		// ever did, a predictable id is still only a lookup key for a deploy
		// whose real authority is the caller's job token.
		return "d_0000000000000000"
	}
	return "d_" + hex.EncodeToString(buf[:])
}
