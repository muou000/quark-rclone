package dav

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/muou000/quark-rclone/internal/adapter"
	"github.com/muou000/quark-rclone/internal/quarkapi"
	"github.com/muou000/quark-rclone/internal/session"
)

// fakeDrive is a scriptable Quark control plane plus a fake CDN. It mirrors
// the fake used at the adapter layer so dav tests exercise the real wire
// client, session store and adapter service end to end.
type fakeDrive struct {
	t *testing.T

	mu      sync.Mutex
	files   map[string][]*quarkapi.File
	parent  map[string]string // fid → parent fid, so move/rename mutate listings
	deleted map[string]bool

	// download recording, served from the fake CDN.
	cdn           *httptest.Server
	downloadCalls int
	cdnRanges     []string
	cdnSawCtxDone chan struct{}
	cdnBlock      chan struct{} // when non-nil, the CDN blocks until closed
	cdnSetCookie  bool          // make the CDN plant a Set-Cookie header

	// upload recording.
	preCalls     int
	hashCalls    int
	authCalls    int
	partCalls    int
	commitCalls  int
	finishCalls  int
	uploadSizes  []int64
	ossBody      []byte
	preResponses []string // scripted /file/upload/pre answers, in order
	preStatuses  []int
	// pendingUpload remembers what PRE allocated so /file/upload/finish can
	// make the object visible in listings, the way the provider's callback
	// does.
	pendingUpload *quarkapi.File
	pendingDir    string
}

func newFakeDrive(t *testing.T) *fakeDrive {
	return &fakeDrive{
		t:             t,
		files:         map[string][]*quarkapi.File{},
		parent:        map[string]string{},
		deleted:       map[string]bool{},
		cdnSawCtxDone: make(chan struct{}, 1),
	}
}

// add seeds a file or directory under a parent FID.
func (f *fakeDrive) add(parent, fid, name string, kind quarkapi.FileKind, size int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[parent] = append(f.files[parent], &quarkapi.File{
		FID: fid, Name: name, Kind: kind, Size: size,
	})
	f.parent[fid] = parent
}

// serve starts the fake control plane, the fake CDN and the fake OSS, and
// wires the adapter service against them.
func (f *fakeDrive) serve(t *testing.T) *adapter.Service {
	t.Helper()
	f.cdn = httptest.NewServer(http.HandlerFunc(f.serveCDN))
	t.Cleanup(f.cdn.Close)
	oss := httptest.NewServer(http.HandlerFunc(f.serveOSS))
	t.Cleanup(oss.Close)

	srv := httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(srv.Close)
	store := session.NewStore("__puus=seed", nil)
	api := quarkapi.New(store, quarkapi.Config{
		BaseURL:       srv.URL,
		UploadURLBase: oss.URL,
	}, srv.Client())
	return adapter.New(api, store)
}

func (f *fakeDrive) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case "/config":
		writeStr(w, `{"status":200,"code":0,"data":{}}`)
	case "/file/sort":
		parent := r.URL.Query().Get("pdir_fid")
		list := []map[string]any{}
		for _, c := range f.files[parent] {
			if f.deleted[c.FID] {
				continue
			}
			list = append(list, map[string]any{
				"fid": c.FID, "file_name": c.Name, "file": bool(c.Kind), "size": c.Size,
			})
		}
		writeStr(w, `{"status":200,"code":0,"data":{"list":`+mustJSON(list)+`},"metadata":{"_total":`+mustJSON(len(list))+`}}`)
	case "/file/download":
		f.downloadCalls++
		writeStr(w, `{"status":200,"code":0,"data":[{"download_url":"`+f.cdn.URL+`/obj"}]}`)
	case "/file":
		var body struct {
			PdirFID  string `json:"pdir_fid"`
			FileName string `json:"file_name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		fid := "dir-" + body.FileName
		f.files[body.PdirFID] = append(f.files[body.PdirFID], &quarkapi.File{
			FID: fid, Name: body.FileName, Kind: quarkapi.KindDir,
		})
		f.parent[fid] = body.PdirFID
		writeStr(w, `{"status":200,"code":0,"data":{"fid":"`+fid+`"}}`)
	case "/file/rename":
		var body struct {
			FID      string `json:"fid"`
			FileName string `json:"file_name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		parent := f.parent[body.FID]
		for _, c := range f.files[parent] {
			if c.FID == body.FID {
				c.Name = body.FileName
			}
		}
		writeStr(w, `{"status":200,"code":0,"data":{}}`)
	case "/file/move":
		var body struct {
			Filelist  []string `json:"filelist"`
			ToPdirFID string   `json:"to_pdir_fid"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		for _, fid := range body.Filelist {
			old := f.parent[fid]
			for i, c := range f.files[old] {
				if c.FID == fid {
					f.files[old] = append(f.files[old][:i], f.files[old][i+1:]...)
					f.files[body.ToPdirFID] = append(f.files[body.ToPdirFID], c)
					f.parent[fid] = body.ToPdirFID
					break
				}
			}
		}
		writeStr(w, `{"status":200,"code":0,"data":{}}`)
	case "/file/delete":
		var body struct {
			Filelist []string `json:"filelist"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		for _, fid := range body.Filelist {
			f.deleted[fid] = true
		}
		writeStr(w, `{"status":200,"code":0,"data":{}}`)
	case "/file/upload/pre":
		f.preCalls++
		if len(f.preResponses) > 0 {
			resp := f.preResponses[0]
			f.preResponses = f.preResponses[1:]
			status := http.StatusOK
			if len(f.preStatuses) > 0 {
				status = f.preStatuses[0]
				f.preStatuses = f.preStatuses[1:]
			}
			w.WriteHeader(status)
			writeStr(w, resp)
			return
		}
		var body struct {
			PdirFID  string `json:"pdir_fid"`
			FileName string `json:"file_name"`
			Size     int64  `json:"size"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.pendingDir = body.PdirFID
		f.pendingUpload = &quarkapi.File{
			FID: "uploaded-fid", Name: body.FileName, Kind: quarkapi.KindFile, Size: body.Size,
		}
		writeStr(w, `{"status":200,"code":0,"data":{"task_id":"t","upload_id":"u","obj_key":"k","upload_url":"`+f.cdn.URL+`","fid":"uploaded-fid","bucket":"b","auth_info":"a","callback":{}},"metadata":{"part_size":1048576}}`)
	case "/file/update/hash":
		f.hashCalls++
		writeStr(w, `{"status":200,"code":0,"data":{"finish":false}}`)
	case "/file/upload/auth":
		f.authCalls++
		writeStr(w, `{"status":200,"code":0,"data":{"auth_key":"sig"}}`)
	case "/file/upload/finish":
		f.finishCalls++
		if f.pendingUpload != nil && !f.deleted[f.pendingUpload.FID] {
			already := false
			for _, c := range f.files[f.pendingDir] {
				if c.FID == f.pendingUpload.FID {
					already = true
				}
			}
			if !already {
				f.files[f.pendingDir] = append(f.files[f.pendingDir], f.pendingUpload)
				f.parent[f.pendingUpload.FID] = f.pendingDir
			}
			f.pendingUpload = nil
		}
		writeStr(w, `{"status":200,"code":0,"data":{}}`)
	default:
		http.NotFound(w, r)
	}
}

// serveCDN stands in for the signed CDN endpoint. It asserts nothing itself;
// tests read the recorded headers and ranges.
func (f *fakeDrive) serveCDN(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.downloadCalls++
	f.cdnRanges = append(f.cdnRanges, r.Header.Get("Range"))
	f.mu.Unlock()

	if f.cdnSetCookie {
		http.SetCookie(w, &http.Cookie{Name: "__puus", Value: "cdn-echo"})
	}
	if f.cdnBlock != nil {
		// A blocked CDN still honours upstream cancellation: that is exactly
		// the path the cancellation and shutdown tests observe.
		select {
		case <-f.cdnBlock:
		case <-r.Context().Done():
			select {
			case f.cdnSawCtxDone <- struct{}{}:
			default:
			}
			return
		}
	}
	w.Header().Set("Content-Length", "11")
	w.Header().Set("Accept-Ranges", "bytes")
	if rng := r.Header.Get("Range"); rng != "" {
		w.Header().Set("Content-Range", "bytes 2-10/11")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("llo world"))
		return
	}
	_, _ = w.Write([]byte("hello world"))
}

// serveOSS accepts the single part PUT and the commit POST of the staged
// upload flow.
func (f *fakeDrive) serveOSS(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method == http.MethodPut {
		f.partCalls++
		body, _ := io.ReadAll(r.Body)
		f.ossBody = append(f.ossBody, body...)
		w.Header().Set("Etag", `"etag-1"`)
		return
	}
	f.commitCalls++
}

// counts snapshots the upload-stage counters.
func (f *fakeDrive) counts() (pre, hash, auth, part, commit, finish int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.preCalls, f.hashCalls, f.authCalls, f.partCalls, f.commitCalls, f.finishCalls
}

// uploadedSize returns the total bytes that reached the fake OSS.
func (f *fakeDrive) uploadedSize() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return int64(len(f.ossBody))
}

// startServer builds the dav Server against the fake and runs it on an
// ephemeral loopback port.
func (f *fakeDrive) startServer(t *testing.T, mutate func(*Config)) *Server {
	t.Helper()
	svc := f.serve(t)
	cfg := Config{
		Addr:    "127.0.0.1:0",
		Cookie:  "session-cookie",
		Service: svc,
		// Deterministic listings: writes invalidate, nothing expires.
		DirTTL: -1,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	s := New(cfg)
	if err := s.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	go func() { _ = s.Serve() }()
	return s
}

// url builds a loopback URL for a DAV path.
func (s *Server) url(t *testing.T, p string) string {
	t.Helper()
	return "http://" + s.Addr() + "/dav" + p
}

func writeStr(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// davReq issues one request against the server with the session cookie
// attached.
func davReq(t *testing.T, method, url, body string, header map[string]string) (*http.Response, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Cookie", "session-cookie")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp, string(got)
}

func TestHealthzAndCapabilitiesCarryNoCredential(t *testing.T) {
	f := newFakeDrive(t)
	s := f.startServer(t, nil)

	resp, body := davReq(t, "GET", "http://"+s.Addr()+"/healthz", "", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"ok"`) {
		t.Fatalf("healthz = %d %q", resp.StatusCode, body)
	}
	resp, body = davReq(t, "GET", "http://"+s.Addr()+"/capabilities", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("capabilities = %d", resp.StatusCode)
	}
	for _, want := range []string{`"copy":false`, `"overwrite_upload":false`, `"PROPFIND"`, `"LOCK"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("capabilities missing %s: %s", want, body)
		}
	}
	// The health and capability surface must not leak the pinned credential.
	if strings.Contains(body, "session-cookie") {
		t.Fatal("capabilities leaked the cookie")
	}
}

// TestRefusedMethodsAnswerStableUnsupported pins the issue-18 contract for
// COPY, PROPPATCH and POST: one stable unsupported answer, with the Allow
// header advertising the real surface.
func TestRefusedMethodsAnswerStableUnsupported(t *testing.T) {
	f := newFakeDrive(t)
	s := f.startServer(t, nil)

	for _, method := range []string{"COPY", "PROPPATCH", "POST"} {
		t.Run(method, func(t *testing.T) {
			resp, body := davReq(t, method, s.url(t, "/a.txt"), "", nil)
			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Fatalf("status = %d, want 405", resp.StatusCode)
			}
			if !strings.Contains(body, "operation_not_supported") {
				t.Fatalf("body = %q, want a stable unsupported error", body)
			}
			if allow := resp.Header.Get("Allow"); !strings.Contains(allow, "PROPFIND") || !strings.Contains(allow, "UNLOCK") {
				t.Fatalf("Allow = %q, want the implemented surface", allow)
			}
		})
	}
}

// TestCookieGateRejectsMissingAndWrong checks the endpoint is not a
// user-facing service: the pinned Cookie header is the only way in, and
// rejections never echo it.
func TestCookieGateRejectsMissingAndWrong(t *testing.T) {
	f := newFakeDrive(t)
	s := f.startServer(t, nil)

	for _, tc := range []struct {
		name   string
		cookie string
	}{
		{"missing", ""},
		{"wrong", "session-cookie-x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest("PROPFIND", s.url(t, "/"), nil)
			if err != nil {
				t.Fatal(err)
			}
			if tc.cookie != "" {
				req.Header.Set("Cookie", tc.cookie)
			}
			req.Header.Set("Depth", "0")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			got, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", resp.StatusCode)
			}
			if !strings.Contains(string(got), "auth_required") {
				t.Fatalf("body = %q, want auth_required", got)
			}
			if strings.Contains(string(got), "session-cookie") {
				t.Fatal("rejection leaked the credential")
			}
		})
	}

	// The right cookie opens the surface.
	resp, body := davReq(t, "PROPFIND", s.url(t, "/"), "", map[string]string{"Depth": "0"})
	if resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("propfind with cookie = %d %q", resp.StatusCode, body)
	}
}

// TestPropfindListsRoot walks the fake drive through PROPFIND: the listing is
// FID-resolved, cached, and reported as WebDAV multistatus.
func TestPropfindListsRoot(t *testing.T) {
	f := newFakeDrive(t)
	f.add(quarkapi.RootFID, "f1", "a.txt", quarkapi.KindFile, 3)
	f.add(quarkapi.RootFID, "d1", "sub", quarkapi.KindDir, 0)
	s := f.startServer(t, nil)

	resp, body := davReq(t, "PROPFIND", s.url(t, "/"), "", map[string]string{"Depth": "1"})
	if resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("status = %d body = %q", resp.StatusCode, body)
	}
	for _, want := range []string{"a.txt", "sub", "<D:multistatus"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q: %s", want, body)
		}
	}
	// Depth 1 lists the collection itself plus its children.
	if got := strings.Count(body, "<D:response>"); got < 3 {
		t.Fatalf("responses = %d, want root + 2 children", got)
	}
}

// TestGetProxiesCDNWithSignedHeadersAndRange pins the download contract: the
// minted URL is fetched with the replayed Cookie/Referer/User-Agent, the
// client's Range passes through untouched, the response stays a clean 206,
// and the CDN's Set-Cookie never reaches the loopback client.
func TestGetProxiesCDNWithSignedHeadersAndRange(t *testing.T) {
	f := newFakeDrive(t)
	f.add(quarkapi.RootFID, "f1", "a.txt", quarkapi.KindFile, 11)
	f.cdnSetCookie = true
	s := f.startServer(t, nil)

	resp, body := davReq(t, "GET", s.url(t, "/a.txt"), "", map[string]string{"Range": "bytes=2-"})
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d body = %q", resp.StatusCode, body)
	}
	if body != "llo world" {
		t.Fatalf("body = %q", body)
	}
	if cr := resp.Header.Get("Content-Range"); cr != "bytes 2-10/11" {
		t.Fatalf("content-range = %q", cr)
	}
	if resp.Header.Get("Set-Cookie") != "" {
		t.Fatal("CDN Set-Cookie leaked to the client")
	}

	// The upstream request carried the mint snapshot and the Range.
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.cdnRanges) != 1 || f.cdnRanges[0] != "bytes=2-" {
		t.Fatalf("upstream ranges = %v", f.cdnRanges)
	}
}

// TestHeadFileSendsHeadersOnly checks HEAD mints and proxies without a body.
func TestHeadFileSendsHeadersOnly(t *testing.T) {
	f := newFakeDrive(t)
	f.add(quarkapi.RootFID, "f1", "a.txt", quarkapi.KindFile, 11)
	s := f.startServer(t, nil)

	req, err := http.NewRequest(http.MethodHead, s.url(t, "/a.txt"), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Cookie", "session-cookie")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(got) != 0 {
		t.Fatalf("head = %d body %q", resp.StatusCode, got)
	}
	if resp.Header.Get("Content-Length") != "11" {
		t.Fatalf("content-length = %q", resp.Header.Get("Content-Length"))
	}
}

// TestGetMissingMapsToNotFound checks resolution failures surface as 404
// with the stable error shape.
func TestGetMissingMapsToNotFound(t *testing.T) {
	f := newFakeDrive(t)
	s := f.startServer(t, nil)

	resp, body := davReq(t, "GET", s.url(t, "/nope.txt"), "", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d body = %q", resp.StatusCode, body)
	}
	if !strings.Contains(body, "object_not_found") {
		t.Fatalf("body = %q", body)
	}
}

// TestGetOnCollectionRefused checks GET/HEAD on a directory answers 405 with
// the method surface rather than attempting a listing body.
func TestGetOnCollectionRefused(t *testing.T) {
	f := newFakeDrive(t)
	s := f.startServer(t, nil)

	resp, body := davReq(t, "GET", s.url(t, "/"), "", nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d body = %q", resp.StatusCode, body)
	}
	if !strings.Contains(resp.Header.Get("Allow"), "PROPFIND") {
		t.Fatalf("Allow = %q", resp.Header.Get("Allow"))
	}
}

// TestPutUploadsThroughAndInvalidates is the write path end to end: bytes
// spool locally, the staged upload runs against the fake provider, the
// response is 201, and a following PROPFIND observes the new file because the
// parent listing cache was invalidated.
func TestPutUploadsThroughAndInvalidates(t *testing.T) {
	f := newFakeDrive(t)
	s := f.startServer(t, nil)

	resp, _ := davReq(t, "PUT", s.url(t, "/new.txt"), "hello", nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("put status = %d", resp.StatusCode)
	}
	if _, _, _, _, _, finish := f.counts(); finish != 1 {
		t.Fatal("upload flow did not reach finish")
	}
	if got := f.uploadedSize(); got != 5 {
		t.Fatalf("oss bytes = %d, want 5", got)
	}

	resp, body := davReq(t, "PROPFIND", s.url(t, "/"), "", map[string]string{"Depth": "1"})
	if resp.StatusCode != http.StatusMultiStatus || !strings.Contains(body, "new.txt") {
		t.Fatalf("propfind after put = %d, listing stale: %s", resp.StatusCode, body)
	}
}

// TestPutNameConflictSurfacesConflict pins the no-overwrite contract at the
// DAV boundary: the provider's same-name refusal becomes 409, not the webdav
// handler's generic 405.
func TestPutNameConflictSurfacesConflict(t *testing.T) {
	f := newFakeDrive(t)
	s := f.startServer(t, nil)
	f.mu.Lock()
	f.preResponses = []string{`{"status":400,"code":40001,"message":"file is doloading[` + "\u540c\u540d\u51b2\u7a81" + `]"}`}
	f.mu.Unlock()

	resp, body := davReq(t, "PUT", s.url(t, "/existing.txt"), "hello", nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d body = %q, want 409", resp.StatusCode, body)
	}
}

// TestPutTransientSurfacesRetryable checks a provider outage during upload
// becomes a 5xx the mount client retries, again overriding the handler's 405.
func TestPutTransientSurfacesRetryable(t *testing.T) {
	f := newFakeDrive(t)
	s := f.startServer(t, nil)
	f.mu.Lock()
	f.preResponses = []string{`{"status":500,"code":500,"message":"internal error"}`}
	f.preStatuses = []int{http.StatusInternalServerError}
	f.mu.Unlock()

	resp, body := davReq(t, "PUT", s.url(t, "/new.txt"), "hello", nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d body = %q, want 502", resp.StatusCode, body)
	}
}

// TestMkcolMoveDeleteFlow exercises the write surface through the webdav
// handler: each mutation lands on the provider and invalidates the cache so
// the next PROPFIND tells the truth.
func TestMkcolMoveDeleteFlow(t *testing.T) {
	f := newFakeDrive(t)
	f.add(quarkapi.RootFID, "f1", "a.txt", quarkapi.KindFile, 3)
	s := f.startServer(t, nil)

	resp, _ := davReq(t, "MKCOL", s.url(t, "/sub"), "", nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("mkcol = %d", resp.StatusCode)
	}

	header := map[string]string{"Destination": s.url(t, "/sub/b.txt")}
	resp, _ = davReq(t, "MOVE", s.url(t, "/a.txt"), "", header)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("move = %d", resp.StatusCode)
	}

	resp, body := davReq(t, "PROPFIND", s.url(t, "/"), "", map[string]string{"Depth": "1"})
	if !strings.Contains(body, "sub") || strings.Contains(body, "a.txt") {
		t.Fatalf("listing after move = %s", body)
	}
	resp, body = davReq(t, "PROPFIND", s.url(t, "/sub"), "", map[string]string{"Depth": "1"})
	if !strings.Contains(body, "b.txt") {
		t.Fatalf("listing of destination = %s", body)
	}

	resp, _ = davReq(t, "DELETE", s.url(t, "/sub/b.txt"), "", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d", resp.StatusCode)
	}
	resp, body = davReq(t, "PROPFIND", s.url(t, "/sub"), "", map[string]string{"Depth": "1"})
	if strings.Contains(body, "b.txt") {
		t.Fatalf("listing after delete = %s", body)
	}
}

// TestLockUnlockGuardsWrites checks the in-memory lock system: a LOCK mints a
// token, a second conflicting LOCK is 423, a locked name refuses writes
// without the token, and UNLOCK with the token releases.
func TestLockUnlockGuardsWrites(t *testing.T) {
	f := newFakeDrive(t)
	f.add(quarkapi.RootFID, "f1", "a.txt", quarkapi.KindFile, 3)
	s := f.startServer(t, nil)

	lockBody := `<?xml version="1.0" encoding="utf-8"?><d:lockinfo xmlns:d="DAV:"><d:lockscope><d:exclusive/></d:lockscope><d:locktype><d:write/></d:locktype><d:owner>mount</d:owner></d:lockinfo>`
	resp, body := davReq(t, "LOCK", s.url(t, "/a.txt"), lockBody, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("lock = %d body = %q", resp.StatusCode, body)
	}
	token := resp.Header.Get("Lock-Token")
	if token == "" {
		t.Fatalf("no lock token in %v", resp.Header)
	}

	// A conflicting LOCK without the token is refused as locked.
	resp, _ = davReq(t, "LOCK", s.url(t, "/a.txt"), lockBody, nil)
	if resp.StatusCode != http.StatusLocked {
		t.Fatalf("second lock = %d, want 423", resp.StatusCode)
	}

	// A write that presents the token via the If header succeeds.
	resp, _ = davReq(t, "PUT", s.url(t, "/a.txt"), "x", map[string]string{"If": "(<" + strings.Trim(token, "<>") + ">)"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("put under own lock = %d, want 201", resp.StatusCode)
	}

	// UNLOCK with the token releases; afterwards the surface is free.
	resp, _ = davReq(t, "UNLOCK", s.url(t, "/a.txt"), "", map[string]string{"Lock-Token": token})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("unlock = %d, want 204", resp.StatusCode)
	}
	resp, _ = davReq(t, "LOCK", s.url(t, "/a.txt"), lockBody, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("relock after unlock = %d", resp.StatusCode)
	}
}

// TestClientCancelReachesUpstream checks request cancellation propagates: a
// loopback client that walks away cancels the in-flight CDN transfer, which
// is what keeps the endpoint from burning provider bandwidth for a dead
// reader.
func TestClientCancelReachesUpstream(t *testing.T) {
	f := newFakeDrive(t)
	f.add(quarkapi.RootFID, "f1", "a.txt", quarkapi.KindFile, 11)
	// Park the transfer inside the CDN so the cancel lands mid-flight.
	block := make(chan struct{})
	f.cdnBlock = block
	defer close(block)
	s := f.startServer(t, nil)

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "GET", s.url(t, "/a.txt"), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Cookie", "session-cookie")

	done := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		done <- err
	}()

	// Mint and CDN connect happen fast; once the CDN sees the request, cancel
	// the client.
	deadline := time.After(2 * time.Second)
	for {
		f.mu.Lock()
		seen := f.downloadCalls >= 2 // one mint + one CDN hit
		f.mu.Unlock()
		if seen {
			break
		}
		select {
		case <-deadline:
			t.Fatal("CDN was never reached")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()

	select {
	case <-f.cdnSawCtxDone:
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not reach the upstream request")
	}
	<-done
}

// TestStreamTimeoutBoundsHangingCDN checks the stream phase timeout: an
// upstream that never answers is cut off and the client gets a 5xx instead of
// hanging forever.
func TestStreamTimeoutBoundsHangingCDN(t *testing.T) {
	f := newFakeDrive(t)
	f.add(quarkapi.RootFID, "f1", "a.txt", quarkapi.KindFile, 11)
	release := make(chan struct{})
	f.cdnBlock = release
	defer close(release)
	s := f.startServer(t, func(c *Config) {
		c.Timeouts.Mint = time.Second
		c.Timeouts.Stream = 100 * time.Millisecond
	})

	start := time.Now()
	resp, body := davReq(t, "GET", s.url(t, "/a.txt"), "", nil)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("stream timeout did not fire promptly (%v)", elapsed)
	}
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d body = %q, want 502", resp.StatusCode, body)
	}
}

// TestShutdownDrainsInFlight checks the shutdown contract: Shutdown stops
// accepting work but waits for the in-flight transfer instead of cutting it.
func TestShutdownDrainsInFlight(t *testing.T) {
	f := newFakeDrive(t)
	f.add(quarkapi.RootFID, "f1", "a.txt", quarkapi.KindFile, 11)
	release := make(chan struct{})
	f.cdnBlock = release
	s := f.startServer(t, func(c *Config) {
		c.Timeouts.Stream = 0 // disable the stream cap: the test controls the CDN
	})

	type result struct {
		body string
		err  error
	}
	clientDone := make(chan result, 1)
	go func() {
		req, _ := http.NewRequest("GET", "http://"+s.Addr()+"/dav/a.txt", nil)
		req.Header.Set("Cookie", "session-cookie")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			clientDone <- result{err: err}
			return
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		clientDone <- result{body: string(b)}
	}()

	// Wait until the transfer is parked inside the CDN.
	deadline := time.After(2 * time.Second)
	for {
		f.mu.Lock()
		seen := f.downloadCalls >= 2
		f.mu.Unlock()
		if seen {
			break
		}
		select {
		case <-deadline:
			t.Fatal("CDN was never reached")
		case <-time.After(time.Millisecond):
		}
	}

	shutdownDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		shutdownDone <- s.Shutdown(ctx)
	}()

	// Shutdown must still be waiting while the CDN holds the request.
	select {
	case err := <-shutdownDone:
		t.Fatalf("Shutdown returned before the in-flight request drained: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	if err := <-shutdownDone; err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	got := <-clientDone
	if got.err != nil {
		t.Fatalf("drained client failed: %v", got.err)
	}
	if got.body != "hello world" {
		t.Fatalf("drained body = %q", got.body)
	}
}

// TestListenRefusesNonLoopback pins the security boundary: the endpoint never
// binds a non-loopback interface, even by misconfiguration.
func TestListenRefusesNonLoopback(t *testing.T) {
	f := newFakeDrive(t)
	svc := f.serve(t)
	s := New(Config{Addr: "0.0.0.0:8080", Cookie: "c", Service: svc})
	if err := s.Listen(); err == nil {
		t.Fatal("Listen accepted a non-loopback bind")
	}
}

// TestNonLoopbackHostInConfigIsRejected covers the textual form too.
func TestNonLoopbackHostInConfigIsRejected(t *testing.T) {
	f := newFakeDrive(t)
	svc := f.serve(t)
	for _, addr := range []string{"192.168.1.5:8080", ":8080"} {
		s := New(Config{Addr: addr, Cookie: "c", Service: svc})
		if err := s.Listen(); err == nil {
			t.Fatalf("Listen accepted %q", addr)
		}
	}
}

// TestOpsTimeoutBoundsListing checks the control-plane path answers promptly
// rather than hanging on a missing tree.
func TestOpsTimeoutBoundsListing(t *testing.T) {
	f := newFakeDrive(t)
	s := f.startServer(t, nil)
	start := time.Now()
	resp, body := davReq(t, "PROPFIND", s.url(t, "/missing/deep/path"), "", map[string]string{"Depth": "0"})
	if time.Since(start) > 5*time.Second {
		t.Fatal("ops path hung")
	}
	if resp.StatusCode != http.StatusNotFound && !strings.Contains(body, "not_found") {
		t.Fatalf("status = %d body = %q", resp.StatusCode, body)
	}
}
