// Package session owns the Quark credential lifecycle: validation of a
// candidate cookie, activation, refresh-field merging and the periodic
// keep-alive refresh.
//
// The split with the wire layer (ADR 0008) is exact: quarkapi decides HOW a
// request is made and reports which refresh fields a response carried; this
// package decides WHAT the current credential is and WHEN it is replaced. The
// two meet at the CredentialSource interface, which this package implements on
// top of a mutex-protected store.
package session

import (
	"context"
	"sync"
	"time"

	"github.com/muou000/quark-rclone/internal/cookieutil"
	"github.com/muou000/quark-rclone/internal/quarkapi"
	"github.com/muou000/quark-rclone/internal/quarkerr"
)

// Refresh cadence (issue #2): Quark sessions expire after roughly two hours,
// and the web client renews around every 100 minutes. A ±5 minute jitter
// spreads renewals so a fleet of clients does not stampede the provider on the
// same tick.
const (
	RefreshInterval = 100 * time.Minute
	RefreshJitter   = 5 * time.Minute
)

// PersistFunc is called with the effective cookie whenever it changes.
//
// It runs while the store's lock is held, which is what keeps concurrent
// updates reaching the disk in the order they were applied — an out-of-lock
// call would let an older cookie overwrite a newer one. Two consequences for
// implementers: it must be fast (requests snapshot the cookie under the same
// lock), and it must not call back into the store, which would deadlock.
type PersistFunc func(cookie string)

// Store is the authoritative holder of the active Quark cookie.
//
// It implements quarkapi.CredentialSource, so the wire client reads a snapshot
// per request and hands observed refresh fields straight back here.
type Store struct {
	mu      sync.Mutex
	cookie  string
	persist PersistFunc
}

// NewStore builds a store around an initial cookie. The cookie is used as
// given: validation of a candidate is an explicit operation, because an
// unvalidated swap is how a dead credential takes over a working mount.
func NewStore(cookie string, persist PersistFunc) *Store {
	return &Store{cookie: cookie, persist: persist}
}

// CookieSnapshot returns the current cookie for one request. A snapshot is a
// plain copy: the string is immutable, so the caller cannot race a rotation.
func (s *Store) CookieSnapshot() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cookie
}

// MergeRefresh merges observed refresh fields into the CURRENT credential,
// not the one any particular request was sent with, so a late response cannot
// clobber a newer concurrent update (ADR 0008). Unknown cookie fields are
// preserved; only the named refresh fields are replaced.
//
// Persistence runs under the same lock, so two concurrent merges reach the
// disk in the order they were merged rather than whichever goroutine woke up
// first.
func (s *Store) MergeRefresh(fields map[string]string) {
	if len(fields) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	merged := s.cookie
	for name, value := range fields {
		merged = cookieutil.Set(merged, name, value)
	}
	s.cookie = merged
	if s.persist != nil {
		s.persist(merged)
	}
}

// Replace swaps in an already-validated cookie and notifies persistence.
func (s *Store) Replace(cookie string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cookie = cookie
	if s.persist != nil {
		s.persist(cookie)
	}
}

// ValidateAndActivate promotes a candidate cookie to the active credential.
//
// Validation is performed by the wire layer against the candidate alone; the
// active credential is neither read nor written during the probes. Only when
// both probes succeed is the candidate — with any refresh fields observed
// during validation applied — swapped in atomically. A failure returns the
// categorised error and leaves the current credential untouched (ADR 0003).
func (s *Store) ValidateAndActivate(ctx context.Context, api *quarkapi.Client, candidate string) error {
	clean, err := cookieutil.Validate(candidate)
	if err != nil {
		return quarkerr.Wrap(quarkerr.CatInvalidRequest, "import_cookie", err)
	}
	effective, err := api.ValidateCandidate(ctx, clean)
	if err != nil {
		return err
	}
	s.Replace(effective)
	return nil
}
