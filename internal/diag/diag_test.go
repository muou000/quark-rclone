package diag

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStaticCapsDeclareMatrix(t *testing.T) {
	caps := StaticCapsOf()
	for name, ok := range map[string]bool{
		"list": caps.List, "read": caps.Read, "write": caps.Write,
		"mkdir": caps.Mkdir, "move": caps.Move, "rename": caps.Rename,
		"delete": caps.Delete,
	} {
		if !ok {
			t.Errorf("static cap %s must be available", name)
		}
	}
	if caps.Copy {
		t.Error("copy must be declared unsupported (ADR 0010)")
	}
	if caps.OverwriteUpload {
		t.Error("overwrite_upload must be declared unsupported (ADR 0010)")
	}
}

func TestRunProbeRecordsOutcome(t *testing.T) {
	r := NewRecorder()

	r.RunProbe(context.Background(), ProbeAuth, func(context.Context) error { return nil })
	r.RunProbe(context.Background(), ProbeRootList, func(context.Context) error {
		return errors.New("HTTP 401: need login")
	})
	r.RunProbe(context.Background(), ProbeReadLink, nil)

	snap := r.Snapshot()
	if len(snap.Probes) != 3 {
		t.Fatalf("probes = %d entries, want 3", len(snap.Probes))
	}
	byName := map[string]Probe{}
	for _, p := range snap.Probes {
		byName[p.Name] = p
	}
	if !byName[ProbeAuth].OK || byName[ProbeAuth].Err != "" {
		t.Errorf("auth probe = %+v, want ok", byName[ProbeAuth])
	}
	if byName[ProbeRootList].OK || byName[ProbeRootList].Err == "" {
		t.Errorf("root_list probe = %+v, want failed with detail", byName[ProbeRootList])
	}
	if !byName[ProbeReadLink].Skipped {
		t.Errorf("read_link probe = %+v, want skipped", byName[ProbeReadLink])
	}
	for _, p := range snap.Probes {
		if p.At.IsZero() {
			t.Errorf("probe %s has no timestamp", p.Name)
		}
	}
}

func TestRunProbeUpsertsByName(t *testing.T) {
	r := NewRecorder()
	r.RunProbe(context.Background(), ProbeAuth, func(context.Context) error {
		return errors.New("dead")
	})
	r.RunProbe(context.Background(), ProbeAuth, func(context.Context) error { return nil })

	snap := r.Snapshot()
	if len(snap.Probes) != 1 {
		t.Fatalf("probes = %d entries, want 1 (upsert)", len(snap.Probes))
	}
	if !snap.Probes[0].OK {
		t.Errorf("latest outcome must win, got %+v", snap.Probes[0])
	}
}

func TestRunProbeRedactsErrorText(t *testing.T) {
	r := NewRecorder()
	r.RunProbe(context.Background(), ProbeRootList, func(context.Context) error {
		return errors.New(`GET failed: cookie __puus=secret-value; url http://cdn/obj?sign=abc123&x=1`)
	})

	errText := ""
	for _, p := range r.Snapshot().Probes {
		errText = p.Err
	}
	if strings.Contains(errText, "secret-value") {
		t.Fatalf("probe error leaked the cookie: %q", errText)
	}
	if strings.Contains(errText, "sign=abc123") {
		t.Fatalf("probe error leaked the signed URL query: %q", errText)
	}
	if !strings.Contains(errText, "[redacted]") {
		t.Errorf("probe error should carry the redaction placeholder: %q", errText)
	}
}

func TestWritebackLedgerAccumulatesAndResolves(t *testing.T) {
	r := NewRecorder()
	r.RecordWritebackFailure("/doc.txt", "upstream 500")
	r.RecordWritebackFailure("/doc.txt", "upstream 500")

	snap := r.Snapshot()
	if len(snap.Writeback) != 1 {
		t.Fatalf("writeback = %d entries, want 1", len(snap.Writeback))
	}
	w := snap.Writeback[0]
	if w.Path != "/doc.txt" || w.Retries != 2 {
		t.Fatalf("entry = %+v, want retries=2", w)
	}
	if w.Event != EventWritebackFailed {
		t.Errorf("event = %q, want %q", w.Event, EventWritebackFailed)
	}
	if w.NextRetry.Before(time.Now()) {
		t.Errorf("next retry %s should be in the future", w.NextRetry)
	}
	if w.Err == "" {
		t.Error("entry should carry the redacted error text")
	}

	r.ResolveWriteback("/doc.txt")
	if got := len(r.Snapshot().Writeback); got != 0 {
		t.Fatalf("after resolve writeback = %d entries, want 0", got)
	}
	// Clearing an absent entry is a no-op.
	r.ResolveWriteback("/doc.txt")
}

func TestWritebackLedgerCapsNextRetryAndSize(t *testing.T) {
	r := NewRecorder()
	for i := 0; i < 50; i++ {
		r.RecordWritebackFailure("/doc.txt", "flaky")
	}
	snap := r.Snapshot()
	if len(snap.Writeback) != 1 {
		t.Fatalf("same path must upsert, got %d entries", len(snap.Writeback))
	}
	if w := snap.Writeback[0]; w.Retries != 50 {
		t.Fatalf("retries = %d, want 50", w.Retries)
	}

	for i := 0; i < maxWriteback+10; i++ {
		r.RecordWritebackFailure("/f"+string(rune('a'+i%26))+string(rune('0'+i/26))+".txt", "flaky")
	}
	if got := len(r.Snapshot().Writeback); got > maxWriteback {
		t.Fatalf("ledger = %d entries, want bounded at %d", got, maxWriteback)
	}
}

func TestRecorderPersistsAndAdopts(t *testing.T) {
	path := filepath.Join(t.TempDir(), SnapshotFile)

	r := NewRecorder()
	r.SetPersistPath(path)
	r.RunProbe(context.Background(), ProbeWrite, func(context.Context) error { return nil })
	r.RecordWritebackFailure("/stuck.bin", "boom")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("snapshot file not written: %v", err)
	}
	var onDisk Snapshot
	if err := json.Unmarshal(data, &onDisk); err != nil {
		t.Fatalf("snapshot file is not valid JSON: %v", err)
	}
	if len(onDisk.Probes) != 1 || onDisk.Probes[0].Name != ProbeWrite {
		t.Errorf("persisted probes = %+v", onDisk.Probes)
	}
	if len(onDisk.Writeback) != 1 || onDisk.Writeback[0].Path != "/stuck.bin" {
		t.Errorf("persisted writeback = %+v", onDisk.Writeback)
	}

	// A fresh recorder adopts what the previous one recorded: the manual write
	// probe's result survives a mount restart.
	next := NewRecorder()
	next.SetPersistPath(path)
	snap := next.Snapshot()
	if len(snap.Probes) != 1 || !snap.Probes[0].OK {
		t.Errorf("adopted probes = %+v, want the write probe", snap.Probes)
	}
	if len(snap.Writeback) != 1 {
		t.Errorf("adopted writeback = %+v", snap.Writeback)
	}

	// A corrupt file heals by starting empty instead of failing the session.
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	healed := NewRecorder()
	healed.SetPersistPath(path)
	if got := len(healed.Snapshot().Probes); got != 0 {
		t.Errorf("corrupt snapshot should start empty, got %d probes", got)
	}
}

func TestRecorderWithoutPathStaysInMemory(t *testing.T) {
	r := NewRecorder()
	r.RunProbe(context.Background(), ProbeAuth, func(context.Context) error { return nil })
	r.RecordWritebackFailure("/x", "e")
	if got := len(r.Snapshot().Probes); got != 1 {
		t.Fatalf("probes = %d, want 1", got)
	}
}

func TestEmptyPathFailuresAreIgnored(t *testing.T) {
	r := NewRecorder()
	r.RecordWritebackFailure("", "no path")
	r.ResolveWriteback("")
	if got := len(r.Snapshot().Writeback); got != 0 {
		t.Fatalf("writeback = %d, want 0", got)
	}
}

func TestSetPersistPathEmptyIsNoop(t *testing.T) {
	r := NewRecorder()
	r.SetPersistPath("") // must not panic or create files
	r.RunProbe(context.Background(), ProbeAuth, func(context.Context) error { return nil })
	if len(r.Snapshot().Probes) != 1 {
		t.Fatal("recorder should still record in memory")
	}
}
