package session

import (
	"context"
	"math/rand/v2"
	"sync"
	"time"
)

// Refresher runs the periodic keep-alive that keeps the Quark session from
// expiring while the mount is up.
//
// The loop must be stoppable and must actually stop: a leaked refresher keeps
// sending requests after the mount is gone, which is both a privacy surprise
// and a rate-limit risk (issue #2's "停止时必须取消刷新循环").
type Refresher struct {
	store *Store
	api   refresherAPI

	interval time.Duration
	jitter   time.Duration

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

// refresherAPI is the slice of the wire client the refresher needs. It exists
// as an interface so tests can drive the loop without a provider.
type refresherAPI interface {
	RefreshSessionCookie(ctx context.Context) error
}

// NewRefresher builds a keep-alive loop. A non-positive interval selects the
// production cadence. A negative jitter selects the production spread; zero
// disables jitter, which tests use to make the tick deterministic.
func NewRefresher(store *Store, api refresherAPI, interval, jitter time.Duration) *Refresher {
	if interval <= 0 {
		interval = RefreshInterval
	}
	if jitter < 0 {
		jitter = RefreshJitter
	}
	return &Refresher{
		store:    store,
		api:      api,
		interval: interval,
		jitter:   jitter,
	}
}

// Start launches the loop. It returns immediately; the first refresh fires
// after one jittered interval. Starting an already-running refresher is a
// no-op, because two loops would double the request rate against the provider.
func (r *Refresher) Start(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancel != nil {
		return
	}
	loopCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	r.cancel = cancel
	r.done = done
	go r.loop(loopCtx, done)
}

// Stop cancels the loop and waits for it to finish. A stopped refresher can be
// started again. Stop on a never-started refresher is a no-op.
func (r *Refresher) Stop() {
	r.mu.Lock()
	cancel, done := r.cancel, r.done
	r.cancel, r.done = nil, nil
	r.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
}

// loop is the body of the keep-alive goroutine.
func (r *Refresher) loop(ctx context.Context, done chan struct{}) {
	defer close(done)
	for {
		wait := r.interval + r.jitterOffset()
		// A jitter larger than the interval would produce a negative or zero
		// wait, which a timer turns into a busy loop. The interval is the
		// floor.
		if wait <= 0 {
			wait = r.interval
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		// A cancelled loop does not refresh: the mount is going away and a
		// final request would outlive it.
		if ctx.Err() != nil {
			return
		}
		// A refresh failure needs no handling here: a transient outage is
		// retried on the next tick, and an authentication failure is the
		// supervisor's state to act on, not this loop's. The refresh call
		// itself already leaves the store untouched on failure.
		_ = r.api.RefreshSessionCookie(ctx)
	}
}

// jitterOffset returns a duration in [-jitter, +jitter] so a fleet of clients
// renews at slightly different times. A zero jitter returns zero, which is how
// a test asks for a deterministic tick.
func (r *Refresher) jitterOffset() time.Duration {
	if r.jitter == 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(2*r.jitter)+1) - int64(r.jitter))
}
