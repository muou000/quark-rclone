package quarkapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/muou000/quark-rclone/internal/quarkerr"
)

// validPreJSON is a PRE payload with every field the upload flow needs.
const validPreJSON = `{"status":200,"code":0,"data":{"task_id":"task-1","upload_id":"up-1","obj_key":"obj-1","upload_url":"https://oss.example.com","fid":"fid-1","bucket":"bucket-1","auth_info":"auth-1","callback":{"callbackUrl":"https://cb.example/c","callbackBody":"{}"}},"metadata":{"part_size":4}}`

func writeRaw(t *testing.T, w http.ResponseWriter, body string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

// uploadAPI is a fake Quark control plane plus a fake OSS endpoint. It counts
// every call by stage so a test can assert exactly how many times each ran,
// which is what distinguishes "retried the auth" from "replayed the body".
type uploadAPI struct {
	mu sync.Mutex

	// preResponse is returned for PRE. Set to a redirect or an error body to
	// exercise those paths.
	preResponse string
	preStatus   int
	// preCalls counts PRE requests.
	preCalls int

	// hashFinish makes /file/update/hash report the upload is already done.
	hashFinish bool
	hashCalls  int
	// hashErrors are returned in order; the call succeeds once exhausted.
	hashErrors []string

	// stageErrors injects auth failures per stage, keyed by "part" or
	// "commit". A single shared list would apply a commit failure to whichever
	// stage ran first, which is the part upload.
	stageErrors map[string][]string
	authCalls   map[string]int

	// partStatus is the OSS status for a part PUT. partBody is what it
	// returns, so a test can make the store emit wording a naive classifier
	// would retry.
	partStatus int
	partBody   string
	// partCalls counts OSS part PUTs; commitCalls counts the commit POST.
	partCalls   int
	commitCalls int

	// finishErrors are returned in order. finishStatus is the HTTP status
	// they are served with, because a permission denial and a provider outage
	// arrive with different statuses and the classifier keys on that.
	finishErrors []string
	finishStatus int
	finishCalls  int
	// visibility makes the directory listing contain the allocated FID.
	visibility bool
	sortCalls  int

	// authCanonical records the canonical strings that were signed.
	authCanonical []string

	// oss is the fake object store.
	oss *httptest.Server
}

func newUploadAPI() *uploadAPI {
	return &uploadAPI{
		preResponse: validPreJSON,
		preStatus:   http.StatusOK,
		partStatus:  http.StatusOK,
		authCalls:   map[string]int{},
		stageErrors: map[string][]string{},
	}
}

// serve builds a Quark control plane and an OSS endpoint around the fake.
func (a *uploadAPI) serve(t *testing.T) *Client {
	t.Helper()
	oss := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		status := a.partStatus
		// A part upload is a PUT; the commit that assembles them is a POST.
		// Counting them together would make the part count wrong for any file
		// that also commits.
		if r.Method == http.MethodPut {
			a.partCalls++
		} else {
			a.commitCalls++
		}
		a.mu.Unlock()
		// An empty body is what OSS returns for a successful part PUT; the
		// ETag is the only thing the client needs from it.
		w.Header().Set("Etag", `"etag-part"`)
		w.WriteHeader(status)
		if status >= 400 {
			body := a.partBody
			if body == "" {
				body = `<Error><Code>InternalError</Code></Error>`
			}
			_, _ = w.Write([]byte(body))
		}
	}))
	t.Cleanup(oss.Close)
	a.oss = oss

	quark := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/file/upload/pre":
			a.mu.Lock()
			a.preCalls++
			a.mu.Unlock()
			if a.preStatus >= 300 && a.preStatus < 400 {
				w.Header().Set("Location", "/redirected")
				w.WriteHeader(a.preStatus)
				return
			}
			w.WriteHeader(a.preStatus)
			writeRaw(t, w, a.preResponse)
		case "/file/update/hash":
			a.mu.Lock()
			a.hashCalls++
			var msg string
			if len(a.hashErrors) > 0 {
				msg = a.hashErrors[0]
				a.hashErrors = a.hashErrors[1:]
			}
			a.mu.Unlock()
			if msg != "" {
				w.WriteHeader(http.StatusInternalServerError)
				writeRaw(t, w, fmt.Sprintf(`{"status":500,"code":500,"message":%q}`, msg))
				return
			}
			writeRaw(t, w, fmt.Sprintf(`{"status":200,"code":0,"data":{"finish":%t}}`, a.hashFinish))
		case "/file/upload/auth":
			body, _ := io.ReadAll(r.Body)
			var req struct {
				AuthInfo string `json:"auth_info"`
				AuthMeta string `json:"auth_meta"`
				TaskID   string `json:"task_id"`
			}
			_ = json.Unmarshal(body, &req)
			// The stage is decided from the canonical string, which names the
			// OSS operation being signed: a part PUT carries partNumber, the
			// commit does not. Detecting the stage by HTTP method alone would
			// confuse the two, because both are signed through the same
			// /file/upload/auth endpoint.
			stage := "part"
			if !strings.Contains(req.AuthMeta, "partNumber=") {
				stage = "commit"
			}
			a.mu.Lock()
			a.authCalls[stage]++
			a.authCanonical = append(a.authCanonical, req.AuthMeta)
			// Errors are injected per stage so a test can target the commit
			// without also failing the part upload that runs first.
			var msg string
			if len(a.stageErrors[stage]) > 0 {
				msg = a.stageErrors[stage][0]
				a.stageErrors[stage] = a.stageErrors[stage][1:]
			}
			a.mu.Unlock()
			if msg != "" {
				w.WriteHeader(http.StatusInternalServerError)
				writeRaw(t, w, fmt.Sprintf(`{"status":500,"code":500,"message":%q}`, msg))
				return
			}
			writeRaw(t, w, `{"status":200,"code":0,"data":{"auth_key":"signed-key"}}`)
		case "/file/upload/finish":
			a.mu.Lock()
			a.finishCalls++
			var msg string
			if len(a.finishErrors) > 0 {
				msg = a.finishErrors[0]
				a.finishErrors = a.finishErrors[1:]
			}
			a.mu.Unlock()
			if msg != "" {
				status := a.finishStatus
				if status == 0 {
					status = http.StatusInternalServerError
				}
				w.WriteHeader(status)
				writeRaw(t, w, fmt.Sprintf(`{"status":%d,"code":%d,"message":%q}`, status, status, msg))
				return
			}
			writeRaw(t, w, `{"status":200,"code":0,"data":{}}`)
		case "/file/sort":
			a.mu.Lock()
			a.sortCalls++
			visible := a.visibility
			a.mu.Unlock()
			list := []any{}
			if visible {
				list = append(list, map[string]any{
					"fid": "fid-1", "file_name": "up.bin", "file": true, "size": 8,
				})
			}
			writeJSON(t, w, map[string]any{
				"status":   200,
				"code":     0,
				"data":     map[string]any{"list": list},
				"metadata": map[string]any{"_total": len(list)},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(quark.Close)

	return New(&fakeSource{cookie: "__puus=abc"}, Config{
		BaseURL:       quark.URL,
		UploadURLBase: oss.URL,
	}, &http.Client{})
}

func TestUploadSinglePart(t *testing.T) {
	shrinkBackoffs(t)
	api := newUploadAPI()
	c := api.serve(t)
	// PRE declares part_size 4, so 4 bytes is exactly one part.
	body := bytes.Repeat([]byte("x"), 4)

	fid, err := c.Upload(t.Context(), UploadRequest{
		ParentFID: "p1", Name: "up.bin", Size: int64(len(body)),
		ContentType: "text/plain", MD5: "d41d8cd98f00b204e9800998ecf8427e",
		SHA1: "da39a3ee5e6b4b0d3255bfef95601890afd80709",
	}, bytes.NewReader(body), nil)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if fid != "fid-1" {
		t.Fatalf("fid=%q, want fid-1", fid)
	}
	if api.preCalls != 1 {
		t.Errorf("preCalls=%d, want 1", api.preCalls)
	}
	if api.partCalls != 1 {
		t.Errorf("partCalls=%d, want 1", api.partCalls)
	}
	if api.commitCalls != 1 {
		t.Errorf("commitCalls=%d, want 1", api.commitCalls)
	}
	if api.finishCalls != 1 {
		t.Errorf("finishCalls=%d, want 1", api.finishCalls)
	}
}

// TestUploadDeduplicatesOnHash is the cheap success: the hash matched, so no
// bytes were sent. This is what makes a repeat upload of an unchanged file two
// requests instead of a full transfer.
func TestUploadDeduplicatesOnHash(t *testing.T) {
	shrinkBackoffs(t)
	api := newUploadAPI()
	api.hashFinish = true
	c := api.serve(t)

	fid, err := c.Upload(t.Context(), UploadRequest{
		ParentFID: "p1", Name: "up.bin", Size: 8,
		ContentType: "text/plain",
		MD5:         "d41d8cd98f00b204e9800998ecf8427e",
		SHA1:        "da39a3ee5e6b4b0d3255bfef95601890afd80709",
	}, bytes.NewReader(bytes.Repeat([]byte("x"), 8)), nil)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if fid != "fid-1" {
		t.Fatalf("fid=%q, want fid-1", fid)
	}
	if api.hashCalls != 1 {
		t.Errorf("hashCalls=%d, want 1", api.hashCalls)
	}
	if api.partCalls != 0 {
		t.Errorf("partCalls=%d, want 0: a deduplicated upload must not transfer bytes", api.partCalls)
	}
}

// TestUploadSplitsIntoProviderParts pins the part count to the size the
// provider asked for. Sending a different part size produces an OSS commit
// that rejects the assembled object.
func TestUploadSplitsIntoProviderParts(t *testing.T) {
	shrinkBackoffs(t)
	api := newUploadAPI()
	c := api.serve(t)
	// PRE declares part_size 4, so 10 bytes is three parts: 4, 4, 2.
	body := bytes.Repeat([]byte("y"), 10)

	if _, err := c.Upload(t.Context(), UploadRequest{
		ParentFID: "p1", Name: "up.bin", Size: int64(len(body)),
		ContentType: "text/plain",
	}, bytes.NewReader(body), nil); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if api.partCalls != 3 {
		t.Fatalf("partCalls=%d, want 3", api.partCalls)
	}
	if api.authCalls["part"] != 3 {
		t.Errorf("part authCalls=%d, want 3", api.authCalls["part"])
	}
	if api.authCalls["commit"] != 1 {
		t.Errorf("commit authCalls=%d, want 1", api.authCalls["commit"])
	}
}

// TestUploadReportsProgress checks the progress callback sees every byte,
// which is what a transfer UI needs to show a moving bar.
func TestUploadReportsProgress(t *testing.T) {
	shrinkBackoffs(t)
	api := newUploadAPI()
	c := api.serve(t)
	body := bytes.Repeat([]byte("z"), 10)

	var last int64
	var updates int
	if _, err := c.Upload(t.Context(), UploadRequest{
		ParentFID: "p1", Name: "up.bin", Size: int64(len(body)),
		ContentType: "text/plain",
	}, bytes.NewReader(body), func(sent int64) {
		last = sent
		updates++
	}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if last != 10 {
		t.Errorf("last progress=%d, want 10", last)
	}
	if updates != 3 {
		t.Errorf("updates=%d, want 3", updates)
	}
}

// TestUploadPreIsNotRetried is the central safety property of the flow. A PRE
// response may be lost after the provider already allocated a task, so a
// retry would create a second task while the caller believes it made one call.
func TestUploadPreIsNotRetried(t *testing.T) {
	shrinkBackoffs(t)
	api := newUploadAPI()
	api.preStatus = http.StatusInternalServerError
	api.preResponse = `{"status":500,"code":500,"message":"inner error, requestId e1-pre"}`
	c := api.serve(t)

	_, err := c.Upload(t.Context(), UploadRequest{
		ParentFID: "p1", Name: "up.bin", Size: 8, ContentType: "text/plain",
	}, bytes.NewReader(make([]byte, 8)), nil)
	if err == nil {
		t.Fatal("want error")
	}
	if api.preCalls != 1 {
		t.Fatalf("preCalls=%d, want 1: PRE must never be replayed", api.preCalls)
	}
	// Nothing downstream may run without an allocation.
	if api.partCalls != 0 || api.finishCalls != 0 {
		t.Errorf("partCalls=%d finishCalls=%d, want 0/0", api.partCalls, api.finishCalls)
	}
}

// TestUploadPreDoesNotFollowRedirect covers the same hazard from the redirect
// side: a followed redirect issues a second allocation.
func TestUploadPreDoesNotFollowRedirect(t *testing.T) {
	for _, status := range []int{http.StatusFound, http.StatusTemporaryRedirect} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			shrinkBackoffs(t)
			api := newUploadAPI()
			api.preStatus = status
			c := api.serve(t)
			// The default client follows redirects; PRE must not.
			c.http = &http.Client{}

			_, err := c.Upload(t.Context(), UploadRequest{
				ParentFID: "p1", Name: "up.bin", Size: 8, ContentType: "text/plain",
			}, bytes.NewReader(make([]byte, 8)), nil)
			if err == nil || !strings.Contains(err.Error(), "redirect") {
				t.Fatalf("err=%v, want a redirect refusal", err)
			}
			if api.preCalls != 1 {
				t.Fatalf("preCalls=%d, want 1", api.preCalls)
			}
		})
	}
}

func TestUploadPreCanceledMakesNoRequest(t *testing.T) {
	shrinkBackoffs(t)
	api := newUploadAPI()
	c := api.serve(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := c.Upload(ctx, UploadRequest{
		ParentFID: "p1", Name: "up.bin", Size: 8, ContentType: "text/plain",
	}, bytes.NewReader(make([]byte, 8)), nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", err)
	}
	if api.preCalls != 0 {
		t.Fatalf("preCalls=%d, want 0", api.preCalls)
	}
}

// TestUploadPreRejectsMissingFields names the missing field instead of
// letting a half-populated task fail later with an opaque OSS error.
func TestUploadPreRejectsMissingFields(t *testing.T) {
	for _, field := range []string{"task_id", "upload_id", "obj_key", "bucket", "auth_info"} {
		t.Run(field, func(t *testing.T) {
			shrinkBackoffs(t)
			api := newUploadAPI()
			var full map[string]any
			if err := json.Unmarshal([]byte(validPreJSON), &full); err != nil {
				t.Fatal(err)
			}
			data := full["data"].(map[string]any)
			delete(data, field)
			out, _ := json.Marshal(full)
			api.preResponse = string(out)
			c := api.serve(t)

			_, err := c.Upload(t.Context(), UploadRequest{
				ParentFID: "p1", Name: "up.bin", Size: 8, ContentType: "text/plain",
			}, bytes.NewReader(make([]byte, 8)), nil)
			if err == nil || !strings.Contains(err.Error(), field) {
				t.Fatalf("err=%v, want it to name %q", err, field)
			}
			if api.partCalls != 0 {
				t.Errorf("partCalls=%d, want 0", api.partCalls)
			}
		})
	}
}

// TestUploadPreRejectsHostWithPath guards the object URL construction. A path
// in the host suffix would send the object somewhere unintended, so it is
// refused rather than silently used.
//
// The base override is cleared first: with one set, the advertised host is
// replaced entirely and the validation is deliberately skipped, so the check
// would never run.
func TestUploadPreRejectsHostWithPath(t *testing.T) {
	for _, host := range []string{"oss.example.com/path", "oss.example.com?q=1", "oss.example.com#frag"} {
		t.Run(host, func(t *testing.T) {
			shrinkBackoffs(t)
			api := newUploadAPI()
			var full map[string]any
			if err := json.Unmarshal([]byte(validPreJSON), &full); err != nil {
				t.Fatal(err)
			}
			full["data"].(map[string]any)["upload_url"] = "https://" + host
			out, _ := json.Marshal(full)
			api.preResponse = string(out)
			c := api.serve(t)
			c.cfg.UploadURLBase = ""

			_, err := c.Upload(t.Context(), UploadRequest{
				ParentFID: "p1", Name: "up.bin", Size: 8, ContentType: "text/plain",
			}, bytes.NewReader(make([]byte, 8)), nil)
			if err == nil {
				t.Fatalf("want error for host %q", host)
			}
			if !strings.Contains(err.Error(), "host suffix") {
				t.Errorf("err = %v, want it to name the host-suffix problem", err)
			}
		})
	}
}

// TestUploadPartAuthIsRetried checks the control-plane transient before an OSS
// PUT is replayed, and that the body is sent again rather than skipped.
func TestUploadPartAuthIsRetried(t *testing.T) {
	shrinkBackoffs(t)
	api := newUploadAPI()
	api.stageErrors["part"] = []string{"inner error, requestId auth-1"}
	c := api.serve(t)

	if _, err := c.Upload(t.Context(), UploadRequest{
		ParentFID: "p1", Name: "up.bin", Size: 4, ContentType: "text/plain",
	}, bytes.NewReader(make([]byte, 4)), nil); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	// One auth retry, one OSS PUT: the body was replayed with a fresh
	// signature.
	if api.authCalls["part"] != 2 {
		t.Errorf("part authCalls=%d, want 2", api.authCalls["part"])
	}
	if api.partCalls != 1 {
		t.Errorf("partCalls=%d, want 1: the OSS PUT itself is not retried", api.partCalls)
	}
}

// TestUploadOSSErrorIsNotRetried is the dangerous case. OSS error bodies also
// contain "inner error, requestId ...", so a classifier that matched on the
// text alone would happily replay a request whose body already reached the
// server.
//
// The fake returns exactly that wording, which is what makes this a real test
// rather than a tautology: the protection has to come from the error's
// category, not from its message.
func TestUploadOSSErrorIsNotRetried(t *testing.T) {
	shrinkBackoffs(t)
	api := newUploadAPI()
	api.partStatus = http.StatusInternalServerError
	api.partBody = `<?xml version="1.0"?><Error><Code>InternalError</Code><Message>inner error, requestId oss-abc</Message></Error>`
	c := api.serve(t)

	_, err := c.Upload(t.Context(), UploadRequest{
		ParentFID: "p1", Name: "up.bin", Size: 4, ContentType: "text/plain",
	}, bytes.NewReader(make([]byte, 4)), nil)
	if err == nil {
		t.Fatal("want error")
	}
	// The body may have reached the store, so replaying it is unsafe. Exactly
	// one PUT is the assertion that would catch a text-matching classifier.
	if api.partCalls != 1 {
		t.Fatalf("partCalls=%d, want 1: an OSS-level error must not be replayed", api.partCalls)
	}
	// And the error must carry a non-transient class, not be re-decided from
	// its message.
	if quarkerr.CategoryOf(err) == quarkerr.CatUpstreamTransient {
		t.Errorf("category=%s, want a non-transient class for an OSS failure", quarkerr.CategoryOf(err))
	}
}

func TestUploadCommitIsRetried(t *testing.T) {
	shrinkBackoffs(t)
	api := newUploadAPI()
	api.stageErrors["commit"] = []string{"complete_upload_lock_timeout, requestId c1"}
	c := api.serve(t)

	if _, err := c.Upload(t.Context(), UploadRequest{
		ParentFID: "p1", Name: "up.bin", Size: 4, ContentType: "text/plain",
	}, bytes.NewReader(make([]byte, 4)), nil); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if api.authCalls["commit"] != 2 {
		t.Errorf("commit authCalls=%d, want 2", api.authCalls["commit"])
	}
	if api.finishCalls != 1 {
		t.Errorf("finishCalls=%d, want 1", api.finishCalls)
	}
}

// TestUploadFinishVisibilityTiebreaker covers the case where finish keeps
// failing but the object is already in the drive. Quark allocates the FID at
// PRE time and the OSS callback can fire before finish returns, so a listing
// showing the FID is real evidence the upload landed.
func TestUploadFinishVisibilityTiebreaker(t *testing.T) {
	shrinkBackoffs(t)
	api := newUploadAPI()
	// Exhaust the retry budget, then make the FID visible.
	api.finishErrors = []string{
		"inner error, requestId f1",
		"inner error, requestId f2",
		"inner error, requestId f3",
	}
	api.visibility = true
	c := api.serve(t)

	fid, err := c.Upload(t.Context(), UploadRequest{
		ParentFID: "p1", Name: "up.bin", Size: 4, ContentType: "text/plain",
	}, bytes.NewReader(make([]byte, 4)), nil)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if fid != "fid-1" {
		t.Errorf("fid=%q, want fid-1", fid)
	}
	if api.finishCalls != 3 {
		t.Errorf("finishCalls=%d, want 3", api.finishCalls)
	}
	if api.sortCalls == 0 {
		t.Error("sortCalls=0, want the visibility probe to consult the listing")
	}
}

// TestUploadFinishFailsClosedWhenNotVisible is the other half: an uncertain
// finish whose FID never appears must fail, not report success.
func TestUploadFinishFailsClosedWhenNotVisible(t *testing.T) {
	shrinkBackoffs(t)
	api := newUploadAPI()
	api.finishErrors = []string{
		"inner error, requestId f1",
		"inner error, requestId f2",
		"inner error, requestId f3",
	}
	api.visibility = false
	c := api.serve(t)

	if _, err := c.Upload(t.Context(), UploadRequest{
		ParentFID: "p1", Name: "up.bin", Size: 4, ContentType: "text/plain",
	}, bytes.NewReader(make([]byte, 4)), nil); err == nil {
		t.Fatal("want error: an upload that is not visible must not report success")
	}
}

func TestUploadFinishPermanentErrorIsNotRetried(t *testing.T) {
	shrinkBackoffs(t)
	api := newUploadAPI()
	// A permission refusal is a 403, not a 500: the classifier must see the
	// difference or it would back off and retry something that can never work.
	api.finishStatus = http.StatusForbidden
	api.finishErrors = []string{"permission denied"}
	c := api.serve(t)

	if _, err := c.Upload(t.Context(), UploadRequest{
		ParentFID: "p1", Name: "up.bin", Size: 4, ContentType: "text/plain",
	}, bytes.NewReader(make([]byte, 4)), nil); err == nil {
		t.Fatal("want error")
	}
	if api.finishCalls != 1 {
		t.Fatalf("finishCalls=%d, want 1: a permanent refusal must not be retried", api.finishCalls)
	}
}

// TestIsNameCollision pins the message match. Quark signals a same-name
// rejection with a bracketed Chinese tag rather than a distinct status code,
// so the match is on the text and must be exact about what it accepts.
func TestIsNameCollision(t *testing.T) {
	tag := "[" + "\u540c\u540d\u51b2\u7a81" + "]"
	cases := []struct {
		msg  string
		want bool
	}{
		{"file is doloading" + tag, true},
		{"same name file exists", true},
		{"same name", true},
		{"", false},
		{"permission denied", false},
		{"quota exceeded", false},
		{"inner error, requestId abc", false},
	}
	for _, tc := range cases {
		if got := isNameCollision(tc.msg); got != tc.want {
			t.Errorf("isNameCollision(%q) = %v, want %v", tc.msg, got, tc.want)
		}
	}
}

// TestUploadNameCollisionMapsToConflict is the overwrite contract: Quark
// refuses a same-name upload rather than replacing the file, and the caller
// must answer 409 so the client reports a conflict instead of silently losing
// the original.
func TestUploadNameCollisionMapsToConflict(t *testing.T) {
	tag := "[" + "\u540c\u540d\u51b2\u7a81" + "]"
	for _, msg := range []string{"file is doloading" + tag, "same name file exists"} {
		t.Run(msg, func(t *testing.T) {
			shrinkBackoffs(t)
			api := newUploadAPI()
			api.preStatus = http.StatusOK
			payload, err := json.Marshal(map[string]any{
				"status": 400, "code": 40001, "message": msg,
			})
			if err != nil {
				t.Fatal(err)
			}
			api.preResponse = string(payload)
			c := api.serve(t)

			_, err = c.Upload(t.Context(), UploadRequest{
				ParentFID: "p1", Name: "up.bin", Size: 4, ContentType: "text/plain",
			}, bytes.NewReader(make([]byte, 4)), nil)
			if err == nil {
				t.Fatal("want error")
			}
			if !quarkerr.IsCategory(err, quarkerr.CatTargetConflict) {
				t.Fatalf("category=%s, want target_conflict", quarkerr.CategoryOf(err))
			}
			if api.partCalls != 0 {
				t.Errorf("partCalls=%d, want 0: a refused upload must not transfer bytes", api.partCalls)
			}
		})
	}
}

func TestUploadAuthCanonicalCoversSignedFields(t *testing.T) {
	shrinkBackoffs(t)
	api := newUploadAPI()
	c := api.serve(t)

	if _, err := c.Upload(t.Context(), UploadRequest{
		ParentFID: "p1", Name: "up.bin", Size: 4, ContentType: "text/plain",
	}, bytes.NewReader(make([]byte, 4)), nil); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.authCanonical) == 0 {
		t.Fatal("no canonical strings were signed")
	}
	part := api.authCanonical[0]
	// The canonical string is what OSS checks the request against. Every field
	// it names must match what is actually sent, so a missing one here is a 403
	// whose OSS message says nothing about the real cause.
	for _, want := range []string{"PUT", "text/plain", "x-oss-date:", "x-oss-user-agent:", "bucket-1/obj-1", "partNumber=1", "uploadId=up-1"} {
		if !strings.Contains(part, want) {
			t.Errorf("canonical %q missing %q", part, want)
		}
	}
}

func TestUploadCanceledMidPartStops(t *testing.T) {
	shrinkBackoffs(t)
	api := newUploadAPI()
	c := api.serve(t)
	ctx, cancel := context.WithCancel(context.Background())

	var calls int64
	body := &countingReader{n: 64, cancel: cancel, counter: &calls}
	_, err := c.Upload(ctx, UploadRequest{
		ParentFID: "p1", Name: "up.bin", Size: 64, ContentType: "text/plain",
	}, body, nil)
	if err == nil {
		t.Fatal("want error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", err)
	}
}

// TestUploadRejectsNegativeSize catches a caller that computed the size wrong,
// which would otherwise be reported by Quark as a confusing size mismatch.
func TestUploadRejectsNegativeSize(t *testing.T) {
	shrinkBackoffs(t)
	api := newUploadAPI()
	c := api.serve(t)
	if _, err := c.Upload(t.Context(), UploadRequest{
		ParentFID: "p1", Name: "up.bin", Size: -1, ContentType: "text/plain",
	}, bytes.NewReader(nil), nil); err == nil {
		t.Fatal("want error for a negative size")
	}
	if api.preCalls != 0 {
		t.Errorf("preCalls=%d, want 0", api.preCalls)
	}
}

// TestUploadEmptyFile checks the zero-byte path: PRE allocates, hash offers
// nothing to send, commit is skipped because there are no parts, and finish
// still runs so the file exists.
func TestUploadEmptyFile(t *testing.T) {
	shrinkBackoffs(t)
	api := newUploadAPI()
	c := api.serve(t)

	fid, err := c.Upload(t.Context(), UploadRequest{
		ParentFID: "p1", Name: "empty.bin", Size: 0, ContentType: "text/plain",
	}, bytes.NewReader(nil), nil)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if fid != "fid-1" {
		t.Errorf("fid=%q, want fid-1", fid)
	}
	if api.partCalls != 0 {
		t.Errorf("partCalls=%d, want 0: an empty file has no parts", api.partCalls)
	}
	if api.finishCalls != 1 {
		t.Errorf("finishCalls=%d, want 1", api.finishCalls)
	}
}

// TestUploadConcurrentIsSafe runs several uploads at once under the race
// detector: the credential source is shared mutable state, and an upload
// takes several control-plane calls that each read it.
func TestUploadConcurrentIsSafe(t *testing.T) {
	shrinkBackoffs(t)
	api := newUploadAPI()
	c := api.serve(t)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = c.Upload(t.Context(), UploadRequest{
				ParentFID: "p1", Name: "up.bin", Size: 4, ContentType: "text/plain",
			}, bytes.NewReader(make([]byte, 4)), nil)
		}()
	}
	wg.Wait()

	if api.preCalls != 8 {
		t.Errorf("preCalls=%d, want 8", api.preCalls)
	}
}

// countingReader cancels the context partway through a read.
type countingReader struct {
	n       int
	pos     int
	cancel  context.CancelFunc
	counter *int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	if r.pos >= r.n {
		return 0, io.EOF
	}
	atomic.AddInt64(r.counter, 1)
	if atomic.LoadInt64(r.counter) > 1 {
		r.cancel()
		return 0, context.Canceled
	}
	n := copy(p, make([]byte, r.n-r.pos))
	r.pos += n
	return n, nil
}

// TestUploadPreCarriesRefreshIntoSource checks a PRE response that reissues
// the session still reaches the credential source, even though PRE reads its
// body outside the ordinary decode path.
func TestUploadPreCarriesRefreshIntoSource(t *testing.T) {
	shrinkBackoffs(t)
	oss := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Etag", `"etag-part"`)
	}))
	t.Cleanup(oss.Close)

	quark := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/file/upload/pre":
			http.SetCookie(w, &http.Cookie{Name: "__puus", Value: "pre-rotated"})
			writeRaw(t, w, validPreJSON)
		case "/file/update/hash":
			writeRaw(t, w, `{"status":200,"code":0,"data":{"finish":false}}`)
		case "/file/upload/auth":
			writeRaw(t, w, `{"status":200,"code":0,"data":{"auth_key":"signed-key"}}`)
		case "/file/upload/finish":
			writeRaw(t, w, `{"status":200,"code":0,"data":{}}`)
		case "/file/sort":
			writeJSON(t, w, map[string]any{
				"status": 200, "code": 0,
				"data":     map[string]any{"list": []any{}},
				"metadata": map[string]any{"_total": 0},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(quark.Close)

	src := &fakeSource{cookie: "__puus=abc"}
	c := New(src, Config{BaseURL: quark.URL, UploadURLBase: oss.URL}, &http.Client{})
	if _, err := c.Upload(t.Context(), UploadRequest{
		ParentFID: "p1", Name: "up.bin", Size: 4, ContentType: "text/plain",
	}, bytes.NewReader(make([]byte, 4)), nil); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	cookie, merges := src.snapshot()
	if merges == 0 || !strings.Contains(cookie, "__puus=pre-rotated") {
		t.Fatalf("PRE refresh was not merged: cookie=%q merges=%d", cookie, merges)
	}
}

// TestUploadRequestCarriesSnapshotCookie checks the PRE request is signed with
// the cookie snapshot taken before the call, so a rotation landing mid-flight
// cannot split the upload across two sessions.
func TestUploadRequestCarriesSnapshotCookie(t *testing.T) {
	shrinkBackoffs(t)
	var preCookie string
	quark := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/file/upload/pre":
			preCookie = r.Header.Get("Cookie")
			writeRaw(t, w, validPreJSON)
		case "/file/update/hash":
			writeRaw(t, w, `{"status":200,"code":0,"data":{"finish":false}}`)
		case "/file/upload/auth":
			writeRaw(t, w, `{"status":200,"code":0,"data":{"auth_key":"signed-key"}}`)
		case "/file/upload/finish":
			writeRaw(t, w, `{"status":200,"code":0,"data":{}}`)
		case "/file/sort":
			writeJSON(t, w, map[string]any{
				"status": 200, "code": 0,
				"data":     map[string]any{"list": []any{}},
				"metadata": map[string]any{"_total": 0},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(quark.Close)
	oss := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Etag", `"etag-part"`)
	}))
	t.Cleanup(oss.Close)

	c := New(&fakeSource{cookie: "__puus=snap"}, Config{
		BaseURL:       quark.URL,
		UploadURLBase: oss.URL,
	}, &http.Client{})
	if _, err := c.Upload(t.Context(), UploadRequest{
		ParentFID: "p1", Name: "up.bin", Size: 4, ContentType: "text/plain",
	}, bytes.NewReader(make([]byte, 4)), nil); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if preCookie != "__puus=snap" {
		t.Fatalf("PRE cookie = %q, want the snapshot", preCookie)
	}
}
