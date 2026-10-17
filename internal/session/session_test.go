package session

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/muou000/quark-rclone/internal/quarkapi"
	"github.com/muou000/quark-rclone/internal/quarkerr"
)

// recordingPersist captures every persisted cookie in order.
type recordingPersist struct {
	mu    sync.Mutex
	saved []string
}

func (p *recordingPersist) persist(cookie string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.saved = append(p.saved, cookie)
}

func (p *recordingPersist) all() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.saved...)
}

// newAPI builds a wire client against a fake control plane serving /config
// and the root listing, which is everything validation probes.
func newAPI(t *testing.T, serve func(w http.ResponseWriter, r *http.Request)) *quarkapi.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(serve))
	t.Cleanup(srv.Close)
	return quarkapi.New(&fakeNullSource{}, quarkapi.Config{BaseURL: srv.URL}, srv.Client())
}

// fakeNullSource is a CredentialSource the validation probes never touch.
type fakeNullSource struct{}

func (f *fakeNullSource) CookieSnapshot() string         { return "__puus=active" }
func (f *fakeNullSource) MergeRefresh(map[string]string) {}

// serveOK answers both validation probes successfully.
func serveOK(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/config":
		writeJSONStr(w, `{"status":200,"code":0,"data":{}}`)
	case "/file/sort":
		writeJSONStr(w, `{"status":200,"code":0,"data":{"list":[]},"metadata":{"_total":0}}`)
	default:
		http.NotFound(w, r)
	}
}

func writeJSONStr(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

func TestValidateAndActivateSwapsValidatedCookie(t *testing.T) {
	var persisted recordingPersist
	store := NewStore("__puus=old", persisted.persist)
	api := newAPI(t, serveOK)

	if err := store.ValidateAndActivate(t.Context(), api, "  __puus=new; extra=1  "); err != nil {
		t.Fatalf("ValidateAndActivate: %v", err)
	}
	if got := store.CookieSnapshot(); got != "__puus=new; extra=1" {
		t.Fatalf("active cookie = %q, want the trimmed candidate", got)
	}
	if saved := persisted.all(); len(saved) != 1 || saved[0] != "__puus=new; extra=1" {
		t.Fatalf("persisted = %v, want exactly the activated cookie", saved)
	}
}

// TestValidateAndActivateAppliesProbeRefresh checks refresh fields observed
// during validation land in the activated cookie, so the credential starts
// from the freshest session the provider issued.
func TestValidateAndActivateAppliesProbeRefresh(t *testing.T) {
	store := NewStore("__puus=old", nil)
	api := newAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/config" {
			http.SetCookie(w, &http.Cookie{Name: "__puus", Value: "during-validation"})
		}
		serveOK(w, r)
	})

	if err := store.ValidateAndActivate(t.Context(), api, "__puus=candidate; keep=1"); err != nil {
		t.Fatalf("ValidateAndActivate: %v", err)
	}
	got := store.CookieSnapshot()
	if got != "__puus=during-validation; keep=1" {
		t.Fatalf("active cookie = %q, want the probe-rotated value", got)
	}
}

// TestValidateAndActivateKeepsOldOnFailure is the ADR 0003 contract: a
// rejected candidate leaves the current credential and the persist history
// untouched.
func TestValidateAndActivateKeepsOldOnFailure(t *testing.T) {
	var persisted recordingPersist
	store := NewStore("__puus=old", persisted.persist)
	api := newAPI(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSONStr(w, `{"status":401,"code":31001,"message":"登录已过期"}`)
	})

	err := store.ValidateAndActivate(t.Context(), api, "__puus=bad")
	if !errors.Is(err, quarkapi.ErrNotAuthenticated) {
		t.Fatalf("err = %v, want ErrNotAuthenticated", err)
	}
	if got := store.CookieSnapshot(); got != "__puus=old" {
		t.Fatalf("active cookie = %q, want the old one preserved", got)
	}
	if saved := persisted.all(); len(saved) != 0 {
		t.Fatalf("persisted = %v, want nothing", saved)
	}
}

// TestValidateAndActivateRejectsMalformedCandidate checks the shape check runs
// before any request is made.
func TestValidateAndActivateRejectsMalformedCandidate(t *testing.T) {
	store := NewStore("__puus=old", nil)
	api := newAPI(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("no request may be made for a malformed candidate")
	})
	if err := store.ValidateAndActivate(t.Context(), api, "not a cookie"); err == nil {
		t.Fatal("want an error for a malformed candidate")
	} else if !quarkerr.IsCategory(err, quarkerr.CatInvalidRequest) {
		t.Fatalf("category = %s, want invalid_request", quarkerr.CategoryOf(err))
	}
}

// TestMergeRefreshLayersOntoCurrentValue is the concurrency contract: a late
// response must not resurrect the value its request was sent with.
func TestMergeRefreshLayersOntoCurrentValue(t *testing.T) {
	var persisted recordingPersist
	store := NewStore("__puus=installed-by-other; __pus=keepme", persisted.persist)

	store.MergeRefresh(map[string]string{"__puus": "newest"})

	got := store.CookieSnapshot()
	if got != "__puus=newest; __pus=keepme" {
		t.Fatalf("merged cookie = %q, want __puus=newest with __pus preserved", got)
	}
	if saved := persisted.all(); len(saved) != 1 || saved[0] != got {
		t.Fatalf("persisted = %v, want the merged value", saved)
	}
}

func TestMergeRefreshEmptyIsNoop(t *testing.T) {
	var persisted recordingPersist
	store := NewStore("__puus=x", persisted.persist)
	store.MergeRefresh(nil)
	if got := store.CookieSnapshot(); got != "__puus=x" {
		t.Fatalf("cookie = %q", got)
	}
	if saved := persisted.all(); len(saved) != 0 {
		t.Fatalf("persisted = %v, want nothing", saved)
	}
}

// TestConcurrentRotationLosesNoUpdate runs simultaneous merges with values
// derived from each "request", so an interleaved write leaves a malformed
// cookie that the assertion can detect. Run with -race to catch the data race
// itself.
func TestConcurrentRotationLosesNoUpdate(t *testing.T) {
	store := NewStore("__puus=start", nil)

	const workers = 16
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			store.MergeRefresh(map[string]string{"__puus": "v", "__pus": "p" + itoa(i)})
		}(i)
	}
	wg.Wait()

	got := store.CookieSnapshot()
	// The cookie must be a single well-formed entry, not a concatenation
	// produced by two writes that interleaved.
	if n := count(got, "__puus="); n != 1 {
		t.Fatalf("cookie has %d __puus entries; concurrent writes interleaved: %s", n, got)
	}
	if n := count(got, "__pus="); n != 1 {
		t.Fatalf("cookie has %d __pus entries; concurrent writes interleaved: %s", n, got)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

func count(s, sub string) int {
	n := 0
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			n++
			i += len(sub) - 1
		}
	}
	return n
}

// fakeRefreshAPI counts refresh calls and can fail them.
type fakeRefreshAPI struct {
	calls atomic.Int64
	fail  atomic.Bool
}

func (f *fakeRefreshAPI) RefreshSessionCookie(ctx context.Context) error {
	f.calls.Add(1)
	if f.fail.Load() {
		return quarkerr.New(quarkerr.CatUpstreamTransient, "refresh", "injected failure")
	}
	return nil
}

// TestRefresherRefreshesPeriodically checks the loop fires on the cadence and
// that Stop actually stops: no further refresh may happen afterwards.
func TestRefresherRefreshesPeriodically(t *testing.T) {
	api := &fakeRefreshAPI{}
	r := NewRefresher(NewStore("__puus=x", nil), api, 10*time.Millisecond, 0)

	r.Start(t.Context())
	// Two intervals is comfortably enough for one refresh without flaking on
	// a slow runner.
	time.Sleep(25 * time.Millisecond)
	r.Stop()

	if got := api.calls.Load(); got < 1 {
		t.Fatalf("refresh calls = %d, want at least 1", got)
	}
	// Stop must be effective: record the count, wait an interval, and require
	// it unchanged.
	after := api.calls.Load()
	time.Sleep(30 * time.Millisecond)
	if got := api.calls.Load(); got != after {
		t.Fatalf("refresh calls grew from %d to %d after Stop", after, got)
	}
}

// TestRefresherSurvivesTransientFailures checks a failing refresh does not
// kill the loop; the next tick tries again.
func TestRefresherSurvivesTransientFailures(t *testing.T) {
	api := &fakeRefreshAPI{}
	api.fail.Store(true)
	r := NewRefresher(NewStore("__puus=x", nil), api, 5*time.Millisecond, 0)

	r.Start(t.Context())
	defer r.Stop()
	time.Sleep(20 * time.Millisecond)
	if got := api.calls.Load(); got < 2 {
		t.Fatalf("refresh calls = %d, want the loop to keep trying", got)
	}
}

// TestRefresherStartTwiceRunsOneLoop checks double Start does not double the
// request rate against the provider.
//
// The probe is concurrency, not call counting: the first refresh is parked on
// a gate so it cannot return, and the peak number of simultaneously parked
// calls is observed. A single loop parks exactly one call — its next tick
// cannot begin while the previous call has not returned — whereas a second
// loop would park a second one alongside it.
func TestRefresherStartTwiceRunsOneLoop(t *testing.T) {
	api := &gatingRefreshAPI{gate: make(chan struct{})}
	r := NewRefresher(NewStore("__puus=x", nil), api, 5*time.Millisecond, 0)

	r.Start(t.Context())
	r.Start(t.Context())
	defer r.Stop()

	deadline := time.Now().Add(2 * time.Second)
	for api.calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if api.calls.Load() == 0 {
		t.Fatal("refresher never made its first call")
	}
	// A stray second loop would carry the same 5ms timer armed at the same
	// moment, so two more intervals are ample for it to pile onto the parked
	// call.
	time.Sleep(10 * time.Millisecond)
	if got := api.maxInFlight.Load(); got != 1 {
		t.Fatalf("peak concurrent refresh calls = %d, want 1 (a single loop)", got)
	}
}

// gatingRefreshAPI parks every refresh call on a gate until the test releases
// it or the context ends, so a test can observe how many calls are running at
// once.
type gatingRefreshAPI struct {
	gate        chan struct{}
	calls       atomic.Int64
	inFlight    atomic.Int64
	maxInFlight atomic.Int64
}

// RefreshSessionCookie implements refresherAPI.
func (g *gatingRefreshAPI) RefreshSessionCookie(ctx context.Context) error {
	g.calls.Add(1)
	n := g.inFlight.Add(1)
	for {
		old := g.maxInFlight.Load()
		if n <= old || g.maxInFlight.CompareAndSwap(old, n) {
			break
		}
	}
	defer g.inFlight.Add(-1)
	select {
	case <-g.gate:
	case <-ctx.Done():
	}
	return nil
}

// TestRefresherStopBeforeStart checks Stop on a fresh refresher is a no-op
// rather than a nil-dereference or a hang.
func TestRefresherStopBeforeStart(t *testing.T) {
	r := NewRefresher(NewStore("__puus=x", nil), &fakeRefreshAPI{}, time.Millisecond, 0)
	r.Stop() // must not panic or hang
}

// TestRefresherRestartAfterStop checks a stopped refresher can serve again.
func TestRefresherRestartAfterStop(t *testing.T) {
	api := &fakeRefreshAPI{}
	r := NewRefresher(NewStore("__puus=x", nil), api, 5*time.Millisecond, 0)

	r.Start(t.Context())
	time.Sleep(8 * time.Millisecond)
	r.Stop()
	callsAtStop := api.calls.Load()

	r.Start(t.Context())
	time.Sleep(12 * time.Millisecond)
	r.Stop()

	if got := api.calls.Load(); got <= callsAtStop {
		t.Fatalf("refresh calls = %d after restart, want growth from %d", got, callsAtStop)
	}
}

// TestRefresherJitterStaysInRange checks the jittered delay never leaves
// [interval-jitter, interval+jitter], so a fleet's renewals stay spread but
// never absurdly early or late.
func TestRefresherJitterStaysInRange(t *testing.T) {
	r := NewRefresher(NewStore("__puus=x", nil), &fakeRefreshAPI{}, time.Minute, 5*time.Minute)
	for i := 0; i < 200; i++ {
		j := r.jitterOffset()
		if j < -5*time.Minute || j > 5*time.Minute {
			t.Fatalf("jitter %v out of range", j)
		}
	}
}
