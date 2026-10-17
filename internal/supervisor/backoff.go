package supervisor

import "time"

// DefaultBackoffSchedule is the network-retry cadence of ADR 0004: 5s, 15s,
// 30s, 60s, then 5 minutes repeated until the mount succeeds, a paused state
// needs a human, or the user disables auto recovery.
var DefaultBackoffSchedule = []time.Duration{
	5 * time.Second,
	15 * time.Second,
	30 * time.Second,
	60 * time.Second,
	5 * time.Minute,
}

// Backoff walks the retry schedule. It is not goroutine-safe: the lifecycle
// loop is the only caller.
type Backoff struct {
	schedule []time.Duration
	next     int
}

// NewBackoff builds a backoff over the schedule. An empty schedule falls back
// to the default cadence.
func NewBackoff(schedule []time.Duration) Backoff {
	if len(schedule) == 0 {
		schedule = DefaultBackoffSchedule
	}
	return Backoff{schedule: schedule}
}

// Next returns the wait before the following attempt. Once past the last
// step it stays at the final, longest interval forever — a network outage of
// hours must not hammer the provider, but must also never give up (ADR 0004:
// keep retrying until success, a paused state, or the user disables it).
func (b *Backoff) Next() time.Duration {
	if b.next >= len(b.schedule) {
		b.next = len(b.schedule) - 1
	}
	d := b.schedule[b.next]
	b.next++
	return d
}
