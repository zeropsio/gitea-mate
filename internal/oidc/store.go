package oidc

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"
)

// How long each transient thing lives (docs/broker-api.md).
const (
	RequestTTL     = 10 * time.Minute
	CodeTTL        = 10 * time.Minute
	AccessTokenTTL = 5 * time.Minute
)

// request is one sign-in in flight: what Gitea asked for, kept under a random
// rid while the person consents in the Mate app.
type request struct {
	RedirectURI string
	State       string
	Nonce       string
	Scope       string
	Expires     time.Time
}

// code is a one-use authorization code bound to its request.
type code struct {
	Claims  Claims
	Nonce   string
	Expires time.Time
}

// access is an opaque access token.
type access struct {
	Claims  Claims
	Expires time.Time
}

// store is the broker's whole memory. A restart forgets requests, codes and
// access tokens; the person signs in again.
type store struct {
	mu       sync.Mutex
	now      func() time.Time
	requests map[string]request
	codes    map[string]code
	accesses map[string]access
}

func newStore(now func() time.Time) *store {
	return &store{
		now:      now,
		requests: map[string]request{},
		codes:    map[string]code{},
		accesses: map[string]access{},
	}
}

func (s *store) putRequest(rid string, r request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweep()
	s.requests[rid] = r
}

func (s *store) takeRequest(rid string) (request, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweep()
	r, ok := s.requests[rid]
	if !ok {
		return request{}, false
	}
	// One consent per request: a replayed rid finds nothing.
	delete(s.requests, rid)
	return r, true
}

func (s *store) putCode(value string, c code) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweep()
	s.codes[value] = c
}

// takeCode consumes a code. A second exchange of the same code finds nothing,
// which is what makes it one-use.
func (s *store) takeCode(value string) (code, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweep()
	c, ok := s.codes[value]
	if !ok {
		return code{}, false
	}
	delete(s.codes, value)
	return c, true
}

func (s *store) putAccess(value string, a access) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweep()
	s.accesses[value] = a
}

func (s *store) readAccess(value string) (access, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweep()
	a, ok := s.accesses[value]
	return a, ok
}

// sweep drops everything expired. Called under the lock on every access, so
// nothing needs a goroutine of its own.
func (s *store) sweep() {
	now := s.now()
	for k, v := range s.requests {
		if now.After(v.Expires) {
			delete(s.requests, k)
		}
	}
	for k, v := range s.codes {
		if now.After(v.Expires) {
			delete(s.codes, k)
		}
	}
	for k, v := range s.accesses {
		if now.After(v.Expires) {
			delete(s.accesses, k)
		}
	}
}

// counts is what a test — and only a test — asks the store.
func (s *store) counts() (int, int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweep()
	return len(s.requests), len(s.codes), len(s.accesses)
}

// randomID makes an unguessable identifier: 256 bits, base64url.
func randomID() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
