// Package mountapp is the shared assembly of one mount session's front end:
// what the mount CLI and the tray both need before a lifecycle can run — the
// capability probes, the diagnostics recorder and the supervisor wiring
// (issue #24).
//
// The split keeps every front end thin: a front end resolves its own
// credential and drive settings, hands them here, and gets back a running
// supervisor plus the shared recorder it can read for diagnostics. Everything
// below this package stays front-end-agnostic.
package mountapp

import (
	"context"
	"path/filepath"

	"github.com/muou000/quark-rclone/internal/adapter"
	"github.com/muou000/quark-rclone/internal/diag"
	"github.com/muou000/quark-rclone/internal/profile"
	"github.com/muou000/quark-rclone/internal/quarkapi"
	"github.com/muou000/quark-rclone/internal/session"
	"github.com/muou000/quark-rclone/internal/supervisor"
)

// Options is one front end's mount wiring. It is exactly what
// supervisor.SupervisorConfig needs plus the diagnostics file location; the
// caller resolved credential and drive choices before handing it over.
type Options struct {
	// Endpoint and session.
	Port       int
	Cookie     string
	Persist    func(cookie string)
	RootFID    string
	MountPoint string

	// rclone child and its write path (issue #23). CacheRoot is the base the
	// supervisor derives the instance cache directory from.
	CacheMode      string
	WriteBack      string
	WriteChunkSize string
	CacheRoot      string
	RclonePath     string
	BundleDir      string

	// Behaviour.
	StatePath string

	// Diagnostics. DiagPath overrides the capability snapshot file; when
	// empty it defaults to diag.json beside ProfileDir.
	ProfileDir string
	DiagPath   string
}

// Start wires the probes, the recorder and the supervised lifecycle, and
// returns both to the caller. The supervisor runs immediately; the caller
// subscribes to its state stream. The recorder is shared with the adapter
// endpoint and persists the capability snapshot at the resolved diag path.
func Start(ctx context.Context, o Options) (*supervisor.Supervisor, *diag.Recorder) {
	rec := diag.NewRecorder()
	rec.SetPersistPath(ResolveDiagPath(o.ProfileDir, o.DiagPath))

	// The probes run against a throwaway store: a probe must never persist a
	// cookie rotation, that is the live session's job.
	probeStore := session.NewStore(o.Cookie, nil)
	probeAPI := quarkapi.New(probeStore, quarkapi.Config{}, nil)
	probeSvc := adapter.New(probeAPI, probeStore)

	// The side-effect-free probe trio of ADR 0010 — auth, root list, read
	// link — runs after readiness succeeds; the write probe never runs here,
	// it is manual only.
	probeCaps := func(ctx context.Context) {
		rec.RunProbe(ctx, diag.ProbeAuth, probeAPI.ProbeConfig)
		files, listErr := probeSvc.List(ctx, quarkapi.RootFID)
		rec.RunProbe(ctx, diag.ProbeRootList, func(context.Context) error { return listErr })
		var linkTarget string
		if listErr == nil {
			for _, f := range files {
				if !f.IsDir() {
					linkTarget = f.FID
					break
				}
			}
		}
		if linkTarget == "" {
			// Nothing in the root to mint a read link against: skipped, not
			// failed — the precondition was absent, not the capability.
			rec.RunProbe(ctx, diag.ProbeReadLink, nil)
			return
		}
		rec.RunProbe(ctx, diag.ProbeReadLink, func(ctx context.Context) error {
			_, err := probeSvc.DownloadURL(ctx, linkTarget)
			return err
		})
	}

	sup := supervisor.Supervise(ctx, supervisor.SupervisorConfig{
		Port:           o.Port,
		Cookie:         o.Cookie,
		Persist:        o.Persist,
		RootFID:        o.RootFID,
		BundleDir:      o.BundleDir,
		RclonePath:     o.RclonePath,
		MountPoint:     o.MountPoint,
		CacheMode:      o.CacheMode,
		WriteBack:      o.WriteBack,
		WriteChunkSize: o.WriteChunkSize,
		CacheRoot:      o.CacheRoot,
		StatePath:      o.StatePath,
		ReadyProbe:     probeAPI.ProbeConfig,
		Recorder:       rec,
		ProbeCaps:      probeCaps,
	})
	return sup, rec
}

// ResolveDiagPath resolves the capability snapshot file: the explicit
// override, or diag.json beside the profile directory (the package default
// when none was named). An unresolvable default returns "" — the recorder
// stays in memory only, which degrades diagnostics, not the mount.
func ResolveDiagPath(profileDir, override string) string {
	if override != "" {
		return override
	}
	dir := profileDir
	if dir == "" {
		var err error
		if dir, err = profile.DefaultDir(); err != nil {
			return ""
		}
	}
	return filepath.Join(dir, diag.SnapshotFile)
}

// ResolveCacheRoot resolves the VFS cache root: the profile's explicit
// setting, else a directory beside the profile, else empty — the supervisor
// then falls back to a per-user temp location. The per-instance directory
// beneath the root is derived by the supervisor (issue #23).
func ResolveCacheRoot(profileDir, explicit string) string {
	if explicit != "" {
		return explicit
	}
	if profileDir != "" {
		return filepath.Join(profileDir, "cache")
	}
	return ""
}
