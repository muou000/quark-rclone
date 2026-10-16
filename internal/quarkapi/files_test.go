package quarkapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/muou000/quark-rclone/internal/quarkerr"
)

// newMutatingFake builds a client whose fake records each request it receives,
// so a test can assert the exact endpoint, method and body a mutation produced.
func newMutatingFake(t *testing.T, handler http.HandlerFunc) (*Client, *recorder) {
	t.Helper()
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r.Method, r.URL.Path)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	c := New(&fakeSource{cookie: "__puus=x"}, Config{BaseURL: srv.URL}, srv.Client())
	return c, rec
}

// recorder is a thread-safe request log.
type recorder struct {
	mu    sync.Mutex
	calls []recordedCall
}

// recordedCall is one request the fake observed.
type recordedCall struct {
	method string
	path   string
}

func (r *recorder) add(method, path string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, recordedCall{method: method, path: path})
}

// recorded returns a copy of the call log safe to read after requests finish.
func (r *recorder) recorded() []recordedCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedCall(nil), r.calls...)
}

// okBody writes a success envelope with an optional data member.
func okBody(t *testing.T, w http.ResponseWriter, data any) {
	t.Helper()
	payload := map[string]any{"status": 200, "code": 0, "message": "ok"}
	if data != nil {
		payload["data"] = data
	}
	writeJSON(t, w, payload)
}

// TestMkdirUsesCreateEndpointAndReturnsFID checks Mkdir posts to /file and
// adopts the FID from the response when the provider supplies one, rather than
// paying for an extra listing to rediscover it.
func TestMkdirUsesCreateEndpointAndReturnsFID(t *testing.T) {
	c, calls := newMutatingFake(t, func(w http.ResponseWriter, r *http.Request) {
		okBody(t, w, map[string]any{"fid": "new-dir"})
	})

	fid, err := c.Mkdir(t.Context(), "parent-1", "photos")
	if err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if fid != "new-dir" {
		t.Errorf("fid = %q, want new-dir", fid)
	}
	if got := calls.recorded(); len(got) != 1 || got[0].method != http.MethodPost || got[0].path != "/file" {
		t.Fatalf("calls = %+v, want a single POST /file", got)
	}
}

// TestMkdirFallsBackToListingWhenNoFID checks the listing fallback still
// resolves the directory when the create response omits its FID.
func TestMkdirFallsBackToListingWhenNoFID(t *testing.T) {
	c, _ := newMutatingFake(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/file":
			okBody(t, w, map[string]any{})
		case "/file/sort":
			okBody(t, w, map[string]any{
				"list":     []map[string]any{{"fid": "listed-dir", "file_name": "photos", "file": false}},
				"metadata": map[string]any{"_total": 1},
			})
		default:
			http.NotFound(w, r)
		}
	})

	fid, err := c.Mkdir(t.Context(), "parent-1", "photos")
	if err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if fid != "listed-dir" {
		t.Errorf("fid = %q, want listed-dir", fid)
	}
}

// TestMkdirMissingDirectoryIsNotFound checks a create that succeeds but leaves
// no trace is reported as absent, not as an unclassified error, so a caller can
// tell it apart from a provider failure.
func TestMkdirMissingDirectoryIsNotFound(t *testing.T) {
	c, _ := newMutatingFake(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/file":
			okBody(t, w, map[string]any{})
		case "/file/sort":
			okBody(t, w, map[string]any{
				"list":     []map[string]any{},
				"metadata": map[string]any{"_total": 0},
			})
		default:
			http.NotFound(w, r)
		}
	})

	_, err := c.Mkdir(t.Context(), "parent-1", "ghost")
	if err == nil {
		t.Fatal("Mkdir should fail when the directory never appears")
	}
	if !quarkerr.IsCategory(err, quarkerr.CatObjectNotFound) {
		t.Errorf("category = %v, want object_not_found", quarkerr.CategoryOf(err))
	}
}

// TestRenamePostsToRenameEndpoint checks the rename body carries the FID and
// the new name, since the FID is the only stable identity.
func TestRenamePostsToRenameEndpoint(t *testing.T) {
	c, calls := newMutatingFake(t, func(w http.ResponseWriter, r *http.Request) {
		okBody(t, w, nil)
	})

	if err := c.Rename(t.Context(), "fid-1", "renamed.mkv"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if got := calls.recorded(); len(got) != 1 || got[0].path != "/file/rename" {
		t.Fatalf("calls = %+v, want a single /file/rename", got)
	}
}

// TestMovePostsToMoveEndpoint checks the move body carries the file list and
// the destination parent, which is what Quark's action_type contract expects.
func TestMovePostsToMoveEndpoint(t *testing.T) {
	c, calls := newMutatingFake(t, func(w http.ResponseWriter, r *http.Request) {
		okBody(t, w, nil)
	})

	err := c.Move(t.Context(), MoveRequest{FileIDs: []string{"fid-1"}, ToPdirID: "dest-1"})
	if err != nil {
		t.Fatalf("Move: %v", err)
	}
	if got := calls.recorded(); len(got) != 1 || got[0].path != "/file/move" {
		t.Fatalf("calls = %+v, want a single /file/move", got)
	}
}

// TestDeletePostsToDeleteEndpoint checks delete targets the recycle-bin
// endpoint with the file list, so a test failure here means the call would hit
// the wrong operation.
func TestDeletePostsToDeleteEndpoint(t *testing.T) {
	c, calls := newMutatingFake(t, func(w http.ResponseWriter, r *http.Request) {
		okBody(t, w, nil)
	})

	err := c.Delete(t.Context(), DeleteRequest{FileIDs: []string{"fid-1"}})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := calls.recorded(); len(got) != 1 || got[0].path != "/file/delete" {
		t.Fatalf("calls = %+v, want a single /file/delete", got)
	}
}

// TestMutationsAreCancellable checks every mutation honours a cancelled
// context. A mount that cannot be stopped cleanly leaves requests in flight
// against the provider, which is what turns a shutdown into a rate-limit
// event.
func TestMutationsAreCancellable(t *testing.T) {
	c, calls := newMutatingFake(t, func(w http.ResponseWriter, r *http.Request) {
		okBody(t, w, nil)
	})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	cases := map[string]func() error{
		"mkdir":  func() error { _, err := c.Mkdir(ctx, "p", "n"); return err },
		"rename": func() error { return c.Rename(ctx, "fid", "n") },
		"move":   func() error { return c.Move(ctx, MoveRequest{FileIDs: []string{"f"}, ToPdirID: "d"}) },
		"delete": func() error { return c.Delete(ctx, DeleteRequest{FileIDs: []string{"f"}}) },
		"list":   func() error { _, err := c.List(ctx, "p"); return err },
	}
	for name, run := range cases {
		if err := run(); err == nil {
			t.Errorf("%s accepted a cancelled context; it must fail", name)
		}
	}
	if got := calls.recorded(); len(got) != 0 {
		t.Errorf("calls = %+v, want none for a cancelled context", got)
	}
}

// TestProviderRejectionIsCategorised checks a rejected mutation surfaces a
// stable category rather than the raw provider message, which is what lets a
// supervisor decide between retrying and prompting.
func TestProviderRejectionIsCategorised(t *testing.T) {
	c, _ := newMutatingFake(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{"status": 403, "code": 40001, "message": "无权限"})
	})

	err := c.Delete(t.Context(), DeleteRequest{FileIDs: []string{"fid"}})
	if err == nil {
		t.Fatal("Delete should fail on a rejection")
	}
	if quarkerr.CategoryOf(err) != quarkerr.CatPermissionDenied {
		t.Errorf("category = %v, want permission_denied", quarkerr.CategoryOf(err))
	}
	if !quarkerr.Permanent(err) {
		t.Error("a permission rejection is permanent; retrying cannot help")
	}
}

// TestFIDExistsTrueOnMatch checks presence requires the provider to echo the
// requested FID exactly.
func TestFIDExistsTrueOnMatch(t *testing.T) {
	c, _ := newMutatingFake(t, func(w http.ResponseWriter, r *http.Request) {
		okBody(t, w, map[string]any{"fid": "fid-1"})
	})
	exists, err := c.FIDExists(t.Context(), "fid-1")
	if err != nil {
		t.Fatalf("FIDExists: %v", err)
	}
	if !exists {
		t.Fatal("FIDExists = false, want true")
	}
}

// TestFIDExistsFalseOnlyOnExactSignature checks absence requires the exact
// triple — HTTP 404, provider status 404, code 21001 — and nothing weaker.
func TestFIDExistsFalseOnlyOnExactSignature(t *testing.T) {
	cases := []struct {
		name       string
		httpStatus int
		status     int
		code       int
		wantExists bool
		wantErr    bool
	}{
		{"exact signature", 404, 404, 21001, false, false},
		{"http 404 without provider code", 404, 404, 0, false, true},
		{"provider 404 on http 200", 200, 404, 21001, false, true},
		{"wrong code", 404, 404, 31001, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newMutatingFake(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.httpStatus)
				writeJSON(t, w, map[string]any{
					"status": tc.status, "code": tc.code, "message": "not found",
				})
			})
			exists, err := c.FIDExists(t.Context(), "fid-1")
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if exists != tc.wantExists {
				t.Fatalf("exists = %v, want %v", exists, tc.wantExists)
			}
		})
	}
}

// TestFIDExistsRejectsMismatchedEcho checks a provider answering with a
// different FID is an error, not absence: a destructive caller must not act on
// a guess.
func TestFIDExistsRejectsMismatchedEcho(t *testing.T) {
	c, _ := newMutatingFake(t, func(w http.ResponseWriter, r *http.Request) {
		okBody(t, w, map[string]any{"fid": "different-fid"})
	})
	if _, err := c.FIDExists(t.Context(), "fid-1"); err == nil {
		t.Fatal("a mismatched fid echo must be an error")
	}
}

func TestFIDExistsRejectsEmptyFID(t *testing.T) {
	c, _ := newMutatingFake(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("no request may be made for an empty fid")
	})
	if _, err := c.FIDExists(t.Context(), ""); err == nil {
		t.Fatal("FIDExists should reject an empty fid")
	}
}

// flakyTransport fails the first N requests to given paths with a transport
// error, then delegates to the real transport. It is how a test simulates "the
// request may have reached the provider".
type flakyTransport struct {
	mu       sync.Mutex
	failNext map[string]int
	attempts map[string]int
}

func (f *flakyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	if f.attempts == nil {
		f.attempts = map[string]int{}
	}
	f.attempts[req.URL.Path]++
	remaining, failing := f.failNext[req.URL.Path]
	if failing && remaining > 0 {
		f.failNext[req.URL.Path] = remaining - 1
		f.mu.Unlock()
		return nil, errors.New("connection reset by peer")
	}
	f.mu.Unlock()
	return http.DefaultTransport.RoundTrip(req)
}

// attemptsOf returns how many requests were issued to a path, including the
// ones that failed at transport level.
func (f *flakyTransport) attemptsOf(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts[path]
}

// newDeleteFake builds a client against a fake that serves /file/delete,
// /file/info and /file/sort with per-call behaviour, plus an optional
// transport-level failure injection for delete.
func newDeleteFake(t *testing.T, serve func(w http.ResponseWriter, r *http.Request), failDeleteTransport int) (*Client, *flakyTransport) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(serve))
	t.Cleanup(srv.Close)
	client := srv.Client()
	var flaky *flakyTransport
	if failDeleteTransport > 0 {
		flaky = &flakyTransport{failNext: map[string]int{"/file/delete": failDeleteTransport}}
		client = &http.Client{Transport: flaky}
	}
	return New(&fakeSource{cookie: "__puus=x"}, Config{BaseURL: srv.URL}, client), flaky
}

// deleteProbe counts calls per path so a test can assert exactly how many
// deletes and verifications ran.
type deleteProbe struct {
	mu    sync.Mutex
	calls map[string]int
}

func (p *deleteProbe) count(path string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.calls == nil {
		p.calls = map[string]int{}
	}
	p.calls[path]++
}

func (p *deleteProbe) countOf(path string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[path]
}

// TestDeleteReliableSucceedsFirstTry checks the happy path costs exactly one
// delete and zero verifications.
func TestDeleteReliableSucceedsFirstTry(t *testing.T) {
	probe := &deleteProbe{}
	c, _ := newDeleteFake(t, func(w http.ResponseWriter, r *http.Request) {
		probe.count(r.URL.Path)
		okBody(t, w, nil)
	}, 0)

	if err := c.DeleteReliable(t.Context(), "fid-1"); err != nil {
		t.Fatalf("DeleteReliable: %v", err)
	}
	if got := probe.countOf("/file/delete"); got != 1 {
		t.Errorf("delete calls = %d, want 1", got)
	}
	if got := probe.countOf("/file/info"); got != 0 {
		t.Errorf("info calls = %d, want 0", got)
	}
}

// TestDeleteReliableTreatsVerifiedAbsenceAsSuccess is the core property: a
// delete whose response says "inner error" but whose FID is verified gone is a
// success, not a failure to report.
func TestDeleteReliableTreatsVerifiedAbsenceAsSuccess(t *testing.T) {
	shrinkBackoffs(t)
	probe := &deleteProbe{}
	c, _ := newDeleteFake(t, func(w http.ResponseWriter, r *http.Request) {
		probe.count(r.URL.Path)
		switch r.URL.Path {
		case "/file/delete":
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(t, w, map[string]any{
				"status": 500, "code": 500, "message": "inner error, requestId abc",
			})
		case "/file/info":
			w.WriteHeader(http.StatusNotFound)
			writeJSON(t, w, map[string]any{
				"status": 404, "code": 21001, "message": "not exists",
			})
		default:
			http.NotFound(w, r)
		}
	}, 0)

	if err := c.DeleteReliable(t.Context(), "fid-1"); err != nil {
		t.Fatalf("DeleteReliable: %v (a verified-absent delete must succeed)", err)
	}
	if got := probe.countOf("/file/delete"); got != 1 {
		t.Errorf("delete calls = %d, want 1: absence was verified, no replay is needed", got)
	}
	if got := probe.countOf("/file/info"); got != 1 {
		t.Errorf("info calls = %d, want 1", got)
	}
}

// TestDeleteReliableRetriesWhenStillExists checks a transient failure whose
// FID is verified present is retried, and that the retry is another single
// delete rather than a blind loop.
func TestDeleteReliableRetriesWhenStillExists(t *testing.T) {
	shrinkBackoffs(t)
	probe := &deleteProbe{}
	transientsLeft := 2
	c, _ := newDeleteFake(t, func(w http.ResponseWriter, r *http.Request) {
		probe.count(r.URL.Path)
		switch r.URL.Path {
		case "/file/delete":
			if transientsLeft > 0 {
				transientsLeft--
				w.WriteHeader(http.StatusInternalServerError)
				writeJSON(t, w, map[string]any{
					"status": 500, "code": 500, "message": "inner error, requestId abc",
				})
				return
			}
			okBody(t, w, nil)
		case "/file/info":
			okBody(t, w, map[string]any{"fid": "fid-1"})
		default:
			http.NotFound(w, r)
		}
	}, 0)

	if err := c.DeleteReliable(t.Context(), "fid-1"); err != nil {
		t.Fatalf("DeleteReliable: %v", err)
	}
	if got := probe.countOf("/file/delete"); got != 3 {
		t.Errorf("delete calls = %d, want 3", got)
	}
	if got := probe.countOf("/file/info"); got != 2 {
		t.Errorf("info calls = %d, want 2 (one per transient failure)", got)
	}
}

// TestDeleteReliableReturnsPermanentRejection checks a permission refusal is
// returned unchanged with no verification and no retry.
func TestDeleteReliableReturnsPermanentRejection(t *testing.T) {
	probe := &deleteProbe{}
	c, _ := newDeleteFake(t, func(w http.ResponseWriter, r *http.Request) {
		probe.count(r.URL.Path)
		writeJSON(t, w, map[string]any{"status": 403, "code": 40001, "message": "无权限"})
	}, 0)

	err := c.DeleteReliable(t.Context(), "fid-1")
	if !quarkerr.IsCategory(err, quarkerr.CatPermissionDenied) {
		t.Fatalf("category = %s, want permission_denied", quarkerr.CategoryOf(err))
	}
	if got := probe.countOf("/file/delete"); got != 1 {
		t.Errorf("delete calls = %d, want 1", got)
	}
	if got := probe.countOf("/file/info"); got != 0 {
		t.Errorf("info calls = %d, want 0: a permanent rejection needs no verification", got)
	}
}

// TestDeleteReliableFailsClosedWhenVerifyUnavailable checks the ambiguous
// transport case: the delete's outcome is unknown and the verifier itself
// fails, so the operation reports ambiguity instead of replaying.
func TestDeleteReliableFailsClosedWhenVerifyUnavailable(t *testing.T) {
	probe := &deleteProbe{}
	c, flaky := newDeleteFake(t, func(w http.ResponseWriter, r *http.Request) {
		probe.count(r.URL.Path)
		switch r.URL.Path {
		case "/file/info":
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(t, w, map[string]any{
				"status": 500, "code": 500, "message": "inner error, requestId v1",
			})
		default:
			http.NotFound(w, r)
		}
	}, 1) // the delete itself fails at transport level

	err := c.DeleteReliable(t.Context(), "fid-1")
	if err == nil {
		t.Fatal("DeleteReliable must fail when the outcome is ambiguous and unverifiable")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("err = %v, want it to name the ambiguity", err)
	}
	if got := flaky.attemptsOf("/file/delete"); got != 1 {
		t.Errorf("delete attempts = %d, want 1: an unverifiable outcome must not be replayed", got)
	}
}

// TestDeleteReliableTransportErrorVerifiedAbsentSucceeds checks a dropped
// connection whose FID is then verified gone still reports success.
func TestDeleteReliableTransportErrorVerifiedAbsentSucceeds(t *testing.T) {
	probe := &deleteProbe{}
	c, _ := newDeleteFake(t, func(w http.ResponseWriter, r *http.Request) {
		probe.count(r.URL.Path)
		if r.URL.Path == "/file/info" {
			w.WriteHeader(http.StatusNotFound)
			writeJSON(t, w, map[string]any{
				"status": 404, "code": 21001, "message": "not exists",
			})
			return
		}
		http.NotFound(w, r)
	}, 1)

	if err := c.DeleteReliable(t.Context(), "fid-1"); err != nil {
		t.Fatalf("DeleteReliable: %v (the delete landed despite the dropped response)", err)
	}
}

// TestDeleteReliableHonorsCancelledContext checks cancellation between
// attempts surfaces as context.Canceled rather than a provider error.
func TestDeleteReliableHonorsCancelledContext(t *testing.T) {
	shrinkBackoffs(t)
	c, _ := newDeleteFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(t, w, map[string]any{
			"status": 500, "code": 500, "message": "inner error, requestId abc",
		})
	}, 0)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := c.DeleteReliable(ctx, "fid-1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
