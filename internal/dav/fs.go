// Package dav exposes the Quark adapter as a loopback-only WebDAV endpoint
// that rclone consumes for the mount (issue #18, ADR 0009).
//
// The split mirrors the adapter layering: quarkapi/session/adapter stay
// protocol- and credential-agnostic; this package is the only one that speaks
// WebDAV. It is deliberately NOT a user-facing WebDAV service — the endpoint
// binds 127.0.0.1 only and every request must carry the session's pinned
// Cookie header, which is the credential rclone replays from its temporary
// config. Upstream Quark requests always use the live session store, so
// `__puus` rotations keep flowing through the supervisor's persistence
// without restarting the mount.
package dav

import (
	"context"
	"errors"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/muou000/quark-rclone/internal/adapter"
	"golang.org/x/net/webdav"
)

// Timeouts bounds the phases a DAV request can spend in this package.
//
// There is deliberately no wall-clock cap on the whole request: a large
// transfer legitimately runs long. The caps target the phases that should
// never take long — control-plane calls, URL minting — plus generous overall
// caps on streaming and uploading that a wedged upstream hits while a healthy
// transfer never does.
type Timeouts struct {
	// Ops bounds each control-plane call the filesystem layer makes
	// (listing, mkdir, move, rename, delete).
	Ops time.Duration
	// Mint bounds one download-URL mint before the CDN transfer starts.
	Mint time.Duration
	// Stream bounds a CDN body transfer. Zero disables the cap.
	Stream time.Duration
	// Upload bounds the whole PUT close phase: spool flush plus the staged
	// Quark upload.
	Upload time.Duration
}

// withDefaults fills the production cadence for unset fields.
func (t Timeouts) withDefaults() Timeouts {
	if t.Ops <= 0 {
		t.Ops = 30 * time.Second
	}
	if t.Mint <= 0 {
		t.Mint = 30 * time.Second
	}
	if t.Stream <= 0 {
		t.Stream = 30 * time.Minute
	}
	if t.Upload <= 0 {
		t.Upload = 30 * time.Minute
	}
	return t
}

// errNotDir reports a path that exists but is not a directory, where the
// caller needed one.
var errNotDir = errors.New("not a directory")

// entry is one resolved drive object: a FID plus the display metadata the
// WebDAV layer reports.
type entry struct {
	fid   string
	isDir bool
	size  int64
	mod   time.Time
}

// dirNode is one cached directory listing keyed by DAV path.
type dirNode struct {
	loaded  time.Time
	entries map[string]entry
}

// FS adapts the FID-addressed adapter service to the path-addressed
// webdav.FileSystem interface.
//
// The path→FID resolution walks segment by segment through cached directory
// listings. The cache is the reason a PROPFIND of a deep tree costs one
// listing per uncached level instead of one per character; writes invalidate
// the directories they touch, and everything expires on a short TTL so an
// out-of-band provider change surfaces without a restart.
type FS struct {
	svc      *adapter.Service
	rootFID  string
	timeouts Timeouts
	ttl      time.Duration

	mu   sync.Mutex
	dirs map[string]*dirNode
}

// NewFS builds the filesystem adapter. A negative ttl keeps listings forever
// (tests use it for determinism); a zero ttl re-fetches on every walk.
func NewFS(svc *adapter.Service, rootFID string, timeouts Timeouts, ttl time.Duration) *FS {
	if rootFID == "" {
		rootFID = "0"
	}
	return &FS{
		svc:      svc,
		rootFID:  rootFID,
		timeouts: timeouts.withDefaults(),
		ttl:      ttl,
		dirs:     map[string]*dirNode{},
	}
}

// cleanPath normalises a DAV path: always rooted, never trailing, never
// escaping through dot segments.
func cleanPath(p string) string {
	if p == "" {
		return "/"
	}
	return path.Clean("/" + p)
}

// splitName splits a cleaned path into its parent directory and final
// segment. The root splits into itself and an empty base.
func splitName(p string) (dir, base string) {
	dir, base = path.Split(p)
	return cleanPath(dir), strings.TrimSuffix(base, "/")
}

// errNotExist builds the os.ErrNotExist error the webdav.Handler maps to the
// not-found statuses of each method.
func errNotExist(op, p string) error {
	return &os.PathError{Op: op, Path: p, Err: os.ErrNotExist}
}

// dirEntries returns the live entry map of one directory, resolving the
// directory's own FID by walking its ancestors first.
func (f *FS) dirEntries(ctx context.Context, dirPath string) (map[string]entry, error) {
	dirPath = cleanPath(dirPath)
	f.mu.Lock()
	node, ok := f.dirs[dirPath]
	if ok && (f.ttl < 0 || time.Since(node.loaded) < f.ttl) {
		f.mu.Unlock()
		return node.entries, nil
	}
	f.mu.Unlock()

	var dirEntry entry
	if dirPath == "/" {
		dirEntry = entry{fid: f.rootFID, isDir: true}
	} else {
		e, err := f.resolve(ctx, dirPath)
		if err != nil {
			return nil, err
		}
		if !e.isDir {
			return nil, &os.PathError{Op: "list", Path: dirPath, Err: errNotDir}
		}
		dirEntry = e
	}

	files, err := f.svc.List(ctx, dirEntry.fid)
	if err != nil {
		// A failed listing must not leave a stale cached copy in place: the
		// next caller would keep reading an expired truth.
		f.invalidateDir(dirPath)
		return nil, err
	}
	entries := make(map[string]entry, len(files))
	for _, file := range files {
		entries[file.Name] = entry{
			fid:   file.FID,
			isDir: file.IsDir(),
			size:  file.Size,
			mod:   time.UnixMilli(file.UpdatedAt),
		}
	}
	f.mu.Lock()
	f.dirs[dirPath] = &dirNode{loaded: time.Now(), entries: entries}
	f.mu.Unlock()
	return entries, nil
}

// resolve maps a DAV path to its drive entry. The FID is the only value the
// result is addressed by; the name is display text.
func (f *FS) resolve(ctx context.Context, p string) (entry, error) {
	p = cleanPath(p)
	if p == "/" {
		return entry{fid: f.rootFID, isDir: true}, nil
	}
	dir, base := splitName(p)
	parent, err := f.dirEntries(ctx, dir)
	if err != nil {
		return entry{}, err
	}
	e, ok := parent[base]
	if !ok {
		return entry{}, errNotExist("resolve", p)
	}
	return e, nil
}

// invalidateDir drops one directory's cached listing.
func (f *FS) invalidateDir(dirPath string) {
	f.mu.Lock()
	delete(f.dirs, cleanPath(dirPath))
	f.mu.Unlock()
}

// invalidateTree drops the cached listings of a directory and everything
// under it, which is what a recursive provider delete leaves stale.
func (f *FS) invalidateTree(dirPath string) {
	dirPath = cleanPath(dirPath)
	prefix := dirPath
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	f.mu.Lock()
	for cached := range f.dirs {
		if cached == dirPath || strings.HasPrefix(cached, prefix) {
			delete(f.dirs, cached)
		}
	}
	f.mu.Unlock()
}

// Mkdir implements webdav.FileSystem. The handler refuses MKCOL on an
// existing name before calling, so a successful call always creates.
func (f *FS) Mkdir(ctx context.Context, name string, perm os.FileMode) error {
	p := cleanPath(name)
	dir, base := splitName(p)
	parent, err := f.resolve(ctx, dir)
	if err != nil {
		return err
	}
	if !parent.isDir {
		return &os.PathError{Op: "mkdir", Path: dir, Err: errNotDir}
	}
	if _, err := f.svc.Mkdir(ctx, parent.fid, base); err != nil {
		return err
	}
	f.invalidateDir(dir)
	return nil
}

// OpenFile implements webdav.FileSystem.
//
// Write mode spools to a local temp file and uploads on Close: Quark's PRE
// needs the total size before any byte moves, and the WebDAV PUT body length
// is not known until the client has sent it all.
//
// Read mode on a directory returns a listing handle for PROPFIND. Read mode
// on a file returns a stub: GET and HEAD are served by the CDN proxy before
// the webdav handler ever sees them, so a read-open of a file has no caller
// in this package.
func (f *FS) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (webdav.File, error) {
	p := cleanPath(name)
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_APPEND) != 0 {
		dir, base := splitName(p)
		parent, err := f.resolve(ctx, dir)
		if err != nil {
			// A missing parent surfaces as 409 Conflict, which is exactly the
			// WebDAV contract for PUT into a nonexistent collection.
			return nil, err
		}
		if !parent.isDir {
			return nil, &os.PathError{Op: "open", Path: dir, Err: errNotDir}
		}
		return newWriteFile(ctx, f, p, parent.fid, base)
	}
	e, err := f.resolve(ctx, p)
	if err != nil {
		return nil, err
	}
	if e.isDir {
		kids, err := f.dirEntries(ctx, p)
		if err != nil {
			return nil, err
		}
		infos := make([]os.FileInfo, 0, len(kids))
		for name, kid := range kids {
			infos = append(infos, fileInfo{entry: kid, name: name})
		}
		sort.Slice(infos, func(i, j int) bool { return infos[i].Name() < infos[j].Name() })
		return &dirFile{fi: fileInfo{entry: e, name: path.Base(p)}, children: infos}, nil
	}
	return &stubFile{fi: fileInfo{entry: e, name: path.Base(p)}}, nil
}

// RemoveAll implements webdav.FileSystem. The provider delete removes a
// directory together with its contents, so the cache is invalidated for the
// whole subtree rather than just the parent.
func (f *FS) RemoveAll(ctx context.Context, name string) error {
	p := cleanPath(name)
	e, err := f.resolve(ctx, p)
	if err != nil {
		return err
	}
	if err := f.svc.Delete(ctx, e.fid); err != nil {
		return err
	}
	if p == "/" {
		f.mu.Lock()
		f.dirs = map[string]*dirNode{}
		f.mu.Unlock()
		return nil
	}
	dir, _ := splitName(p)
	f.invalidateTree(p)
	f.invalidateDir(dir)
	return nil
}

// Rename implements webdav.FileSystem for MOVE.
//
// A cross-directory move and a rename are two provider operations; the move
// runs first, so a rename failure afterwards leaves the object at the new
// location under the old name — visible, addressable by FID, and repairable
// by a retry that finds the source gone.
func (f *FS) Rename(ctx context.Context, oldName, newName string) error {
	op, np := cleanPath(oldName), cleanPath(newName)
	if op == np {
		return nil
	}
	if op == "/" {
		return &os.PathError{Op: "rename", Path: op, Err: errNotDir}
	}
	e, err := f.resolve(ctx, op)
	if err != nil {
		return err
	}
	newDir, newBase := splitName(np)
	newParent, err := f.resolve(ctx, newDir)
	if err != nil {
		// A missing destination parent is the caller's mistake and the
		// WebDAV contract answers it with 409 Conflict.
		return err
	}
	if !newParent.isDir {
		return &os.PathError{Op: "rename", Path: newDir, Err: errNotDir}
	}
	oldDir, oldBase := splitName(op)
	if oldDir != newDir {
		if err := f.svc.Move(ctx, []string{e.fid}, newParent.fid); err != nil {
			return err
		}
	}
	if oldBase != newBase {
		if err := f.svc.Rename(ctx, e.fid, newBase); err != nil {
			return err
		}
	}
	f.invalidateTree(op)
	f.invalidateDir(oldDir)
	f.invalidateDir(newDir)
	return nil
}

// Stat implements webdav.FileSystem.
func (f *FS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	e, err := f.resolve(ctx, name)
	if err != nil {
		return nil, err
	}
	return fileInfo{entry: e, name: path.Base(cleanPath(name))}, nil
}

// fileInfo implements os.FileInfo over a resolved entry.
type fileInfo struct {
	entry entry
	name  string
}

func (fi fileInfo) Name() string { return fi.name }
func (fi fileInfo) Size() int64  { return fi.entry.size }

func (fi fileInfo) Mode() os.FileMode {
	if fi.entry.isDir {
		return os.ModeDir | 0o755
	}
	return 0o644
}

func (fi fileInfo) ModTime() time.Time { return fi.entry.mod }
func (fi fileInfo) IsDir() bool        { return fi.entry.isDir }
func (fi fileInfo) Sys() any           { return nil }

// dirFile is the read handle of a directory: PROPFIND walks children through
// Readdir, and everything else is refused because collections carry no body.
type dirFile struct {
	fi       fileInfo
	children []os.FileInfo
	off      int
}

func (d *dirFile) Stat() (os.FileInfo, error) { return d.fi, nil }
func (d *dirFile) Close() error               { return nil }

func (d *dirFile) Read([]byte) (int, error) {
	return 0, &os.PathError{Op: "read", Path: d.fi.name, Err: errNotDir}
}

func (d *dirFile) Seek(int64, int) (int64, error) {
	return 0, &os.PathError{Op: "seek", Path: d.fi.name, Err: errNotDir}
}

func (d *dirFile) Write(p []byte) (int, error) {
	return 0, &os.PathError{Op: "write", Path: d.fi.name, Err: errNotDir}
}

// Readdir implements the os.File contract: n <= 0 drains the rest, n > 0
// returns at most n entries and eventually io.EOF.
func (d *dirFile) Readdir(n int) ([]os.FileInfo, error) {
	if n <= 0 {
		out := d.children[d.off:]
		d.off = len(d.children)
		return out, nil
	}
	if d.off >= len(d.children) {
		return nil, io.EOF
	}
	end := d.off + n
	if end > len(d.children) {
		end = len(d.children)
	}
	out := d.children[d.off:end]
	d.off = end
	return out, nil
}

// stubFile is the read handle of a regular file. In this package GET and HEAD
// never reach the webdav handler, so the stub only exists to fail honestly if
// a future path opens a file for reading instead of silently misbehaving.
type stubFile struct {
	fi fileInfo
}

func (s *stubFile) Stat() (os.FileInfo, error) { return s.fi, nil }
func (s *stubFile) Close() error               { return nil }

func (s *stubFile) Read([]byte) (int, error) {
	return 0, &os.PathError{Op: "read", Path: s.fi.name, Err: errors.New("reads are served by the CDN proxy")}
}

func (s *stubFile) Seek(int64, int) (int64, error) {
	return 0, &os.PathError{Op: "seek", Path: s.fi.name, Err: errors.New("reads are served by the CDN proxy")}
}

func (s *stubFile) Write(p []byte) (int, error) {
	return 0, &os.PathError{Op: "write", Path: s.fi.name, Err: errors.New("write opens must not target an existing read handle")}
}

func (s *stubFile) Readdir(int) ([]os.FileInfo, error) {
	return nil, &os.PathError{Op: "readdir", Path: s.fi.name, Err: errNotDir}
}

// writeFile spools one PUT body to a local temp file and uploads it to Quark
// on Close.
//
// The spool exists because Quark's upload contract needs the total size at
// PRE time, while a PUT body's length is only known once the client finishes
// sending. Spooling to disk keeps memory flat for any file size; the temp
// file lives in the user's temp directory and is removed on every exit path.
type writeFile struct {
	fs        *FS
	fi        fileInfo
	davPath   string
	parentFID string
	name      string
	spool     *os.File
	holder    *uploadErrHolder
	ctx       context.Context
	// writeErr records the first body-receive failure. A spool that is not
	// the complete request body must never be uploaded: a partial file that
	// reached the provider would still be allocated a FID and become visible.
	writeErr error
}

// newWriteFile creates the spool. The upload-error holder comes from the
// request context when the server wired one in, which is how a PUT's real
// failure category reaches the status-rewriting middleware.
func newWriteFile(ctx context.Context, fs *FS, p, parentFID, base string) (*writeFile, error) {
	spool, err := os.CreateTemp("", "quark-dav-put-*")
	if err != nil {
		return nil, &os.PathError{Op: "spool", Path: p, Err: err}
	}
	holder, _ := ctx.Value(uploadErrKey{}).(*uploadErrHolder)
	return &writeFile{
		fs:        fs,
		fi:        fileInfo{name: base},
		davPath:   p,
		parentFID: parentFID,
		name:      base,
		spool:     spool,
		holder:    holder,
		ctx:       ctx,
	}, nil
}

func (w *writeFile) Write(p []byte) (int, error) {
	n, err := w.spool.Write(p)
	if err != nil {
		w.writeErr = &os.PathError{Op: "write", Path: w.fi.name, Err: err}
		return n, w.writeErr
	}
	return n, nil
}

func (w *writeFile) Stat() (os.FileInfo, error) {
	fi, err := w.spool.Stat()
	if err != nil {
		return nil, err
	}
	return fileInfo{entry: entry{size: fi.Size()}, name: w.fi.name}, nil
}

func (w *writeFile) Read([]byte) (int, error) {
	return 0, &os.PathError{Op: "read", Path: w.fi.name, Err: errors.New("write handle")}
}

func (w *writeFile) Seek(offset int64, whence int) (int64, error) {
	return w.spool.Seek(offset, whence)
}

func (w *writeFile) Readdir(int) ([]os.FileInfo, error) {
	return nil, &os.PathError{Op: "readdir", Path: w.fi.name, Err: errNotDir}
}

// Close flushes the spool, runs the staged Quark upload, invalidates the
// parent listing and removes the temp file. The returned error carries the
// provider's category so the PUT status fixer can answer 409 for a name
// conflict and 502-class statuses for transient upstream failures, instead of
// the generic 405 the webdav handler maps close errors to.
//
// A body that did not arrive completely is never uploaded: the webdav
// handler still calls Close after a failed body copy, and a partial file that
// reached the provider would hold a live FID and look like a real object.
// The guards are the spool write record and the request context, which the
// server cancels the moment the client or the read deadline ends the body.
func (w *writeFile) Close() error {
	defer os.Remove(w.spool.Name())
	if w.writeErr != nil {
		return w.writeErr
	}
	if err := w.ctx.Err(); err != nil {
		// The body never arrived completely: a stalled client cut by the read
		// deadline, or a client that hung up. Either way the partial spool
		// must not become a drive object.
		return err
	}
	size, err := w.spool.Seek(0, io.SeekEnd)
	if err != nil {
		w.record(err)
		return err
	}
	if _, err := w.spool.Seek(0, io.SeekStart); err != nil {
		w.record(err)
		return err
	}
	ctx := w.ctx
	if w.fs.timeouts.Upload > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, w.fs.timeouts.Upload)
		defer cancel()
	}
	_, err = w.fs.svc.Upload(ctx, adapter.UploadRequest{
		ParentFID:   w.parentFID,
		Name:        w.name,
		Size:        size,
		ContentType: "application/octet-stream",
	}, w.spool, nil)
	if err != nil {
		w.record(err)
		return err
	}
	w.fs.invalidateDir(path.Dir(w.davPath))
	return nil
}

// record stashes the terminal upload error for the status fixer. A client
// that vanished mid-body is not recorded: there is nobody left to answer.
func (w *writeFile) record(err error) {
	if w.holder == nil || errors.Is(err, context.Canceled) {
		return
	}
	w.holder.set(err)
}
