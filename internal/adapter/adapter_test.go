package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/muou000/quark-rclone/internal/quarkapi"
	"github.com/muou000/quark-rclone/internal/quarkerr"
	"github.com/muou000/quark-rclone/internal/session"
)

// fakeQuark is a scriptable Quark control plane covering every endpoint the
// service exposes, so the service-level tests exercise the real wire client
// and the real session store rather than mocks of them.
type fakeQuark struct {
	t *testing.T

	// files maps parent FID to named children, so listing and name lookups
	// behave like a drive.
	files map[string][]*quarkapi.File
	// deleted records FIDs removed by /file/delete.
	deleted map[string]bool

	// uploadPreScript is returned in order by /file/upload/pre.
	uploadPreScript []string
	uploadPreCalls  int
}

func newFakeQuark(t *testing.T) *fakeQuark {
	return &fakeQuark{
		t:       t,
		files:   map[string][]*quarkapi.File{},
		deleted: map[string]bool{},
	}
}

func (f *fakeQuark) child(parent, name string) *quarkapi.File {
	for _, c := range f.files[parent] {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// serve runs the fake and wires the service against it.
func (f *fakeQuark) serve(t *testing.T) *Service {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(srv.Close)
	store := session.NewStore("__puus=seed", nil)
	api := quarkapi.New(store, quarkapi.Config{BaseURL: srv.URL}, srv.Client())
	return New(api, store)
}

func (f *fakeQuark) handle(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/config":
		writeJSONStr(w, `{"status":200,"code":0,"data":{}}`)
	case "/file/sort":
		parent := r.URL.Query().Get("pdir_fid")
		list := []map[string]any{}
		for _, c := range f.files[parent] {
			if f.deleted[c.FID] {
				continue
			}
			list = append(list, map[string]any{
				"fid": c.FID, "file_name": c.Name, "file": bool(c.Kind),
				"size": c.Size,
			})
		}
		writeJSONStr(w, `{"status":200,"code":0,"data":{"list":`+mustJSON(list)+`},"metadata":{"_total":`+mustJSON(len(list))+`}}`)
	case "/file":
		var body struct {
			PdirFID  string `json:"pdir_fid"`
			FileName string `json:"file_name"`
		}
		decodeBody(r, &body)
		if existing := f.child(body.PdirFID, body.FileName); existing != nil {
			// Quark tolerates a same-name directory create and returns its
			// FID; for the service test this branch is enough.
			writeJSONStr(w, `{"status":200,"code":0,"data":{"fid":"`+existing.FID+`"}}`)
			return
		}
		fid := "dir-" + body.FileName
		f.files[body.PdirFID] = append(f.files[body.PdirFID], &quarkapi.File{
			FID: fid, Name: body.FileName, Kind: quarkapi.KindDir,
		})
		writeJSONStr(w, `{"status":200,"code":0,"data":{"fid":"`+fid+`"}}`)
	case "/file/info":
		fid := r.URL.Query().Get("fid")
		if f.deleted[fid] {
			w.WriteHeader(http.StatusNotFound)
			writeJSONStr(w, `{"status":404,"code":21001,"message":"not exists"}`)
			return
		}
		writeJSONStr(w, `{"status":200,"code":0,"data":{"fid":"`+fid+`"}}`)
	case "/file/rename", "/file/move":
		writeJSONStr(w, `{"status":200,"code":0,"data":{}}`)
	case "/file/delete":
		var body struct {
			Filelist []string `json:"filelist"`
		}
		decodeBody(r, &body)
		for _, fid := range body.Filelist {
			f.deleted[fid] = true
		}
		writeJSONStr(w, `{"status":200,"code":0,"data":{}}`)
	case "/file/download":
		writeJSONStr(w, `{"status":200,"code":0,"data":[{"download_url":"https://cdn.example.com/signed?sign=abc"}]}`)
	case "/file/upload/pre":
		f.uploadPreCalls++
		if len(f.uploadPreScript) > 0 {
			next := f.uploadPreScript[0]
			f.uploadPreScript = f.uploadPreScript[1:]
			w.WriteHeader(http.StatusBadRequest)
			writeJSONStr(w, next)
			return
		}
		writeJSONStr(w, `{"status":200,"code":0,"data":{"task_id":"t","upload_id":"u","obj_key":"k","upload_url":"https://oss.example.com","fid":"uploaded-fid","bucket":"b","auth_info":"a","callback":{}},"metadata":{"part_size":4}}`)
	default:
		http.NotFound(w, r)
	}
}

func decodeBody(r *http.Request, v any) {
	_ = json.NewDecoder(r.Body).Decode(v)
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func writeJSONStr(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

func TestCapabilitiesStateTheContract(t *testing.T) {
	svc := newFakeQuark(t).serve(t)
	caps := svc.Capabilities()
	if caps.Copy {
		t.Error("Copy must be reported unsupported")
	}
	if caps.OverwriteUpload {
		t.Error("overwrite upload must be reported unsupported")
	}
}

// TestCopyIsExplicitlyRefused checks the unsupported operation reports its
// category rather than attempting an emulation that would mint a new FID.
func TestCopyIsExplicitlyRefused(t *testing.T) {
	svc := newFakeQuark(t).serve(t)
	err := svc.Copy(t.Context(), "fid-1", "dest", "name")
	if !quarkerr.IsNotSupported(err) {
		t.Fatalf("err = %v, want operation_not_supported", err)
	}
	if quarkerr.StatusCode(err) != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", quarkerr.StatusCode(err))
	}
}

// TestValidateAndActivateRoundTrip checks the service exposes the credential
// lifecycle and that the activated cookie is visible through the snapshot.
func TestValidateAndActivateRoundTrip(t *testing.T) {
	f := newFakeQuark(t)
	f.files[quarkapi.RootFID] = []*quarkapi.File{{FID: "c1", Name: "child", Kind: quarkapi.KindDir}}
	svc := f.serve(t)

	if err := svc.ValidateAndActivate(t.Context(), " __puus=fresh "); err != nil {
		t.Fatalf("ValidateAndActivate: %v", err)
	}
	if got := svc.CookieSnapshot(); got != "__puus=fresh" {
		t.Fatalf("snapshot = %q, want the activated cookie", got)
	}
}

// TestListDownloadMkdirRenameMoveDelete is the full-surface walkthrough: each
// service operation against the scripted fake, asserting the FID-based
// identity survives the whole chain.
func TestListDownloadMkdirRenameMoveDelete(t *testing.T) {
	f := newFakeQuark(t)
	f.files[quarkapi.RootFID] = []*quarkapi.File{
		{FID: "file-1", Name: "a.txt", Kind: quarkapi.KindFile, Size: 3},
		{FID: "dir-1", Name: "sub", Kind: quarkapi.KindDir},
	}
	svc := f.serve(t)
	ctx := t.Context()

	// List by FID.
	entries, err := svc.List(ctx, quarkapi.RootFID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}

	// Download pairs the URL with its signed headers.
	dl, err := svc.DownloadURL(ctx, "file-1")
	if err != nil {
		t.Fatalf("DownloadURL: %v", err)
	}
	if dl.URL == "" || dl.Header["Cookie"] == "" {
		t.Fatalf("download result incomplete: %+v", dl)
	}

	// Mkdir returns the FID of the created directory.
	fid, err := svc.Mkdir(ctx, quarkapi.RootFID, "newdir")
	if err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if fid != "dir-newdir" {
		t.Fatalf("fid = %q, want dir-newdir", fid)
	}

	// Rename keeps the FID.
	if err := svc.Rename(ctx, "file-1", "renamed.txt"); err != nil {
		t.Fatalf("Rename: %v", err)
	}

	// Move into the new directory.
	if err := svc.Move(ctx, []string{"file-1"}, fid); err != nil {
		t.Fatalf("Move: %v", err)
	}

	// Delete, then confirm the FID is gone through the store's own listing.
	if err := svc.Delete(ctx, "file-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	remaining, err := svc.List(ctx, quarkapi.RootFID)
	if err != nil {
		t.Fatalf("List after delete: %v", err)
	}
	for _, e := range remaining {
		if e.FID == "file-1" {
			t.Fatal("deleted FID is still listed")
		}
	}
}

// TestUploadNameCollisionSurfacesConflict checks the no-overwrite contract at
// the service boundary: a same-name rejection from PRE becomes
// CatTargetConflict, the answer a caller turns into 409.
func TestUploadNameCollisionSurfacesConflict(t *testing.T) {
	tag := "[" + "\u540c\u540d\u51b2\u7a81" + "]"
	f := newFakeQuark(t)
	f.uploadPreScript = []string{`{"status":400,"code":40001,"message":"file is doloading` + tag + `"}`}
	svc := f.serve(t)

	_, err := svc.Upload(t.Context(), UploadRequest{
		ParentFID: quarkapi.RootFID, Name: "up.bin", Size: 4, ContentType: "text/plain",
	}, bytes.NewReader(make([]byte, 4)), nil)
	if !quarkerr.IsCategory(err, quarkerr.CatTargetConflict) {
		t.Fatalf("category = %s, want target_conflict", quarkerr.CategoryOf(err))
	}
	if quarkerr.StatusCode(err) != http.StatusConflict {
		t.Fatalf("status = %d, want 409", quarkerr.StatusCode(err))
	}
}

// TestServiceOperationsAreCancellable checks every public operation honours a
// cancelled context, which is what lets a mount stop cleanly.
func TestServiceOperationsAreCancellable(t *testing.T) {
	svc := newFakeQuark(t).serve(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	cases := map[string]func() error{
		"list":   func() error { _, err := svc.List(ctx, "p"); return err },
		"mkdir":  func() error { _, err := svc.Mkdir(ctx, "p", "n"); return err },
		"rename": func() error { return svc.Rename(ctx, "f", "n") },
		"move":   func() error { return svc.Move(ctx, []string{"f"}, "d") },
		"delete": func() error { return svc.Delete(ctx, "f") },
		"upload": func() error {
			_, err := svc.Upload(ctx, UploadRequest{ParentFID: "p", Name: "n"}, bytes.NewReader(nil), nil)
			return err
		},
		"copy":    func() error { return svc.Copy(ctx, "f", "d", "n") },
		"refresh": func() error { return svc.RefreshSessionCookie(ctx) },
		"download": func() error {
			_, err := svc.DownloadURL(ctx, "f")
			return err
		},
		"validate": func() error { return svc.ValidateAndActivate(ctx, "__puus=x") },
	}
	for name, run := range cases {
		if err := run(); err == nil {
			t.Errorf("%s accepted a cancelled context; it must fail", name)
		}
	}
}

// TestReauthSurfacedAsCategory checks an authentication rejection crosses the
// service boundary as its stable category, which is the supervisor's trigger
// to prompt for a new cookie.
func TestReauthSurfacedAsCategory(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSONStr(w, `{"status":401,"code":31001,"message":"登录已过期"}`)
	}))
	t.Cleanup(srv.Close)
	store := session.NewStore("__puus=seed", nil)
	api := quarkapi.New(store, quarkapi.Config{BaseURL: srv.URL}, srv.Client())
	svc := New(api, store)

	err := svc.ValidateAndActivate(t.Context(), "__puus=stale")
	if !quarkerr.NeedsReauth(err) {
		t.Fatalf("err = %v, want NeedsReauth", err)
	}
	if !errors.Is(err, quarkapi.ErrNotAuthenticated) {
		t.Fatalf("err = %v, want ErrNotAuthenticated", err)
	}
	// The store kept the previous credential.
	if got := svc.CookieSnapshot(); got != "__puus=seed" {
		t.Fatalf("snapshot = %q, want the preserved credential", got)
	}
}

// TestCookieNeverLeaksIntoServiceErrors is a log-hygiene property: error
// messages crossing the service boundary must not carry the cookie value,
// because those messages are exactly what a caller logs verbatim.
func TestCookieNeverLeaksIntoServiceErrors(t *testing.T) {
	svc := newFakeQuark(t).serve(t)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := svc.List(ctx, "p")
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), "__puus=seed") {
		t.Fatalf("error leaks the cookie: %v", err)
	}
}
