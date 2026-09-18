package deploy

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
)

// A record's states: a grant was handed over, and what the job then reported.
const (
	StatusGranted   = "granted"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
)

// Record is one grant: what a job was allowed to deploy, kept so that its
// report (POST /deploy/{id}/result) lands on the right commit without the job
// naming one.
type Record struct {
	ID          string
	Environment string
	Service     string
	Sha         string
	Status      string
	Message     string
	// Repository is the service repository the commit status is written on,
	// and the only repository whose jobs may report on this record.
	Repository string
	// ProjectID and ServiceID are where the deploy went, for what the broker
	// does once it landed (public access).
	ProjectID string
	ServiceID string
}

// DefaultRecordLimit is how many grants the broker remembers. Beyond it the
// oldest is forgotten, which is the same thing a restart does — a report on a
// forgotten grant is a 404 the action shrugs at, and the next pass writes the
// commit's status from what the service runs (docs/broker-api.md).
const DefaultRecordLimit = 2000

// Records is the broker's memory of the grants it handed out. There is no
// database: a restart forgets, an unknown id is 404, and the commit status
// says the same thing once a pass has looked.
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

// New records a grant and returns it with its id.
func (r *Records) New(rec Record) Record {
	rec.ID = newID()
	rec.Status = StatusGranted
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
