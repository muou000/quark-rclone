// Package diag holds the mount session's capability snapshot and the
// writeback-failure ledger (issue #22, ADR 0010).
//
// The capability snapshot is the answer to "what does this mount actually
// support, and how do we know": the adapter's static operation matrix, plus
// the outcome of the side-effect-free probes that run on startup and after
// re-authentication, plus the write probe a user triggers manually. Nothing in
// a probe ever creates, modifies or deletes a drive object — only the manual
// write diagnostic does, and it cleans up after itself.
//
// The writeback ledger is the dirty-cache contract made observable: a
// writeback the provider refused keeps the local file visible, readable and
// writable, and records the file, the retry count and the next retry time
// instead of hiding or locking anything.
//
// Everything recorded passes redaction before it is stored, and the snapshot
// never contains a credential, a signed URL or a raw provider message.
package diag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/muou000/quark-rclone/internal/adapter"
	"github.com/muou000/quark-rclone/internal/quarkerr"
	"github.com/muou000/quark-rclone/internal/redact"
)

// Probe names. They are part of the snapshot's shape: a reader of diag.json
// or /diagnostics keys on them.
const (
	// ProbeAuth verifies the credential without touching drive data.
	ProbeAuth = "auth"
	// ProbeRootList lists the drive root without writing anything.
	ProbeRootList = "root_list"
	// ProbeReadLink mints a download URL for an existing root file. The URL is
	// used for nothing and is discarded; minting is the read-link capability.
	ProbeReadLink = "read_link"
	// ProbeWrite is the manual write diagnostic: upload a small file, verify
	// it, delete it. It is the only probe with a side effect and runs only
	// when the user asks for it.
	ProbeWrite = "write"
)

// SnapshotFile is the well-known filename of the persisted capability
// snapshot, written beside the profile's state.json.
const SnapshotFile = "diag.json"

// maxWriteback bounds the ledger. A wedged upload retried forever must not
// grow the snapshot without limit; the oldest entry falls off.
const maxWriteback = 32

// writebackRetry is the cadence the ledger advertises for a failing
// writeback's next attempt, indexed by consecutive failure count. It mirrors
// the ADR 0004 network cadence so the two retry schedules a user can observe
// do not contradict each other.
var writebackRetry = []time.Duration{
	5 * time.Second, 15 * time.Second, 30 * time.Second, time.Minute, 5 * time.Minute,
}

// Probe is one capability probe's outcome. A probe that could not run for lack
// of its precondition is Skipped, not failed — "nothing to mint a link for"
// says nothing about whether link minting works.
type Probe struct {
	Name    string    `json:"name"`
	OK      bool      `json:"ok"`
	Skipped bool      `json:"skipped,omitempty"`
	At      time.Time `json:"at"`
	Err     string    `json:"error,omitempty"`
}

// EventWritebackFailed is the stable event token of a writeback ledger entry
// (ADR 0010): a dirty-cache writeback the provider refused. A tray keys on
// this token instead of parsing free-text errors.
const EventWritebackFailed = "writeback_failed"

// WritebackFailure is one file whose provider writeback the endpoint refused.
// The file itself stays visible, readable and writable — the ledger is a
// diagnosis, never a lock.
type WritebackFailure struct {
	// Event is always EventWritebackFailed; it exists so a consumer reads one
	// stable token rather than inferring the event from the JSON key.
	Event string `json:"event"`
	// Category is the stable error class behind the failure (issue #23): what
	// the tray shows instead of a raw errno, and the same vocabulary every
	// other layer speaks. Never empty for a recorded entry — unclassified
	// errors are recorded as "unknown".
	Category  string    `json:"category"`
	Path      string    `json:"path"`
	Retries   int       `json:"retries"`
	NextRetry time.Time `json:"next_retry,omitempty"`
	At        time.Time `json:"at"`
	Err       string    `json:"error,omitempty"`
}

// StaticCaps is the adapter's declared operation matrix (ADR 0010). It is a
// statement, not a probe result: everything except copy and overwrite is
// available, and the two gaps are permanent properties of the backend.
type StaticCaps struct {
	List            bool `json:"list"`
	Read            bool `json:"read"`
	Write           bool `json:"write"`
	Mkdir           bool `json:"mkdir"`
	Move            bool `json:"move"`
	Rename          bool `json:"rename"`
	Delete          bool `json:"delete"`
	Copy            bool `json:"copy"`
	OverwriteUpload bool `json:"overwrite_upload"`
}

// StaticCapsOf declares the matrix from the adapter's capability statement.
func StaticCapsOf() StaticCaps {
	caps := adapter.CapabilitiesOf()
	return StaticCaps{
		List: true, Read: true, Write: true, Mkdir: true,
		Move: true, Rename: true, Delete: true,
		Copy:            caps.Copy,
		OverwriteUpload: caps.OverwriteUpload,
	}
}

// Snapshot is the capability statement as data: what the backend declares,
// what the probes observed and which writebacks are currently failing. It is
// safe to expose — every error text in it was redacted on the way in.
type Snapshot struct {
	Static    StaticCaps         `json:"static"`
	Probes    []Probe            `json:"probes"`
	Writeback []WritebackFailure `json:"writeback_failures,omitempty"`
	Updated   time.Time          `json:"updated"`
}

// Recorder collects probe outcomes and writeback failures, thread-safely, and
// persists the snapshot when given a path. A caller with nothing to record
// simply never builds one; consumers that share a Recorder by pointer (the
// endpoint, the supervisor) treat nil as recording disabled.
type Recorder struct {
	mu        sync.Mutex
	static    StaticCaps
	probes    []Probe
	writeback []WritebackFailure
	path      string
}

// NewRecorder builds an empty recorder over the static capability matrix.
func NewRecorder() *Recorder {
	return &Recorder{static: StaticCapsOf()}
}

// SetPersistPath points the recorder at the snapshot file and adopts whatever
// a previous session recorded there. Adoption keeps a manual write probe's
// result visible across a mount restart; every probe the new session re-runs
// overwrites its own entry, so a stale result can only ever show an old
// timestamp, never a wrong one.
func (r *Recorder) SetPersistPath(path string) {
	if path == "" {
		return
	}
	r.mu.Lock()
	r.path = path
	r.mu.Unlock()
	data, err := os.ReadFile(path)
	if err != nil {
		return // first run, or an unreadable file: start empty
	}
	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		// A truncated file from a crash mid-write heals by starting empty;
		// the next record rewrites it whole.
		return
	}
	r.mu.Lock()
	r.probes = snap.Probes
	r.writeback = snap.Writeback
	r.mu.Unlock()
}

// RunProbe executes one capability probe and records the outcome. A nil fn
// records a skipped probe. The error text is redacted before storage, so a
// probe failure can never smuggle a credential into the snapshot.
func (r *Recorder) RunProbe(ctx context.Context, name string, fn func(context.Context) error) {
	p := Probe{Name: name, At: time.Now()}
	switch {
	case fn == nil:
		p.Skipped = true
	default:
		if err := fn(ctx); err != nil {
			p.Err = redact.Text(err.Error())
		} else {
			p.OK = true
		}
	}
	r.recordProbe(p)
}

// recordProbe upserts one outcome by name: the snapshot holds at most one
// entry per probe, the latest one.
func (r *Recorder) recordProbe(p Probe) {
	r.mu.Lock()
	for i := range r.probes {
		if r.probes[i].Name == p.Name {
			r.probes[i] = p
			r.mu.Unlock()
			r.persist()
			return
		}
	}
	r.probes = append(r.probes, p)
	r.mu.Unlock()
	r.persist()
}

// RecordWritebackFailure adds one refused writeback for path. The error is
// classified here — the category travels with the entry so a tray never has
// to re-derive it — and its text is redacted before storage. Repeated
// failures for the same path accumulate: the retry count grows by one and the
// next-retry time follows the diagnostic cadence. The ledger is bounded; when
// full, the oldest entry falls off.
func (r *Recorder) RecordWritebackFailure(path string, err error) {
	if path == "" || err == nil {
		return
	}
	now := time.Now()
	errText := err.Error()
	entry := WritebackFailure{
		Event:    EventWritebackFailed,
		Category: string(quarkerr.CategoryOf(err)),
		Path:     path,
		At:       now,
		Err:      redact.Text(errText),
		Retries:  1,
	}
	r.mu.Lock()
	idx := -1
	for i := range r.writeback {
		if r.writeback[i].Path == path {
			idx = i
			break
		}
	}
	if idx >= 0 {
		entry.Retries = r.writeback[idx].Retries + 1
		r.writeback = append(r.writeback[:idx], r.writeback[idx+1:]...)
	}
	retryIdx := entry.Retries - 1
	if retryIdx >= len(writebackRetry) {
		retryIdx = len(writebackRetry) - 1
	}
	entry.NextRetry = now.Add(writebackRetry[retryIdx])
	r.writeback = append(r.writeback, entry)
	if len(r.writeback) > maxWriteback {
		r.writeback = r.writeback[len(r.writeback)-maxWriteback:]
	}
	r.mu.Unlock()
	r.persist()
}

// ResolveWriteback clears a path's ledger entry: the writeback went through,
// so the file must stop looking broken. Clearing an absent entry is a no-op.
func (r *Recorder) ResolveWriteback(path string) {
	if path == "" {
		return
	}
	r.mu.Lock()
	for i := range r.writeback {
		if r.writeback[i].Path == path {
			r.writeback = append(r.writeback[:i], r.writeback[i+1:]...)
			r.mu.Unlock()
			r.persist()
			return
		}
	}
	r.mu.Unlock()
}

// Snapshot returns the current statement. The Updated timestamp is the read
// time, so a consumer can tell how fresh what it is looking at is.
func (r *Recorder) Snapshot() Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return Snapshot{
		Static:    r.static,
		Probes:    append([]Probe(nil), r.probes...),
		Writeback: append([]WritebackFailure(nil), r.writeback...),
		Updated:   time.Now(),
	}
}

// persist rewrites the snapshot file when the recorder has a path. Failures
// are reported on stderr and swallowed: a diagnostic write must never break
// the request that produced the diagnostic.
func (r *Recorder) persist() {
	r.mu.Lock()
	path := r.path
	snap := Snapshot{
		Static:    r.static,
		Probes:    append([]Probe(nil), r.probes...),
		Writeback: append([]WritebackFailure(nil), r.writeback...),
		Updated:   time.Now(),
	}
	r.mu.Unlock()
	if path == "" {
		return
	}
	if err := writeSnapshot(path, snap); err != nil {
		fmt.Fprintf(os.Stderr, "diag: could not persist capability snapshot: %v\n", err)
	}
}

// writeSnapshot replaces the snapshot file atomically: temp file in the same
// directory, then a rename that retries through the Windows sharing violation
// a concurrent reader can produce. The snapshot is diagnostic data, so a lost
// rename falls back to a direct write rather than surfacing.
func writeSnapshot(path string, snap Snapshot) error {
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		err := os.Rename(tmpName, path)
		if err == nil {
			return nil
		}
		// ERROR_SHARING_VIOLATION: a reader holds the target open.
		if attempt < 20 && isSharingViolation(err) {
			time.Sleep(5 * time.Millisecond)
			continue
		}
		return os.WriteFile(path, data, 0o600)
	}
}

// isSharingViolation reports the Windows errno a concurrent reader produces
// when the atomic rename tries to replace the file it has open. On other
// platforms the errno value is meaningless but also unreachable through
// os.Rename failure modes this helper cares about.
func isSharingViolation(err error) bool {
	return errors.Is(err, syscall.Errno(32))
}
