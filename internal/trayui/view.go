package trayui

import (
	"time"

	"github.com/muou000/quark-rclone/internal/diag"
)

// PanelData is the whole main-panel payload (issue #24): one JSON document
// the embedded page renders into its three views — 状态, 诊断, 设置. Every
// user-facing string in it is already Chinese; the page adds no wording of
// its own beyond static labels.
type PanelData struct {
	Status StatusView `json:"status"`

	// Timeline is the recovery timeline: the mount's transitions, oldest
	// first, capped by the supervisor's history ring.
	Timeline []TimelineView `json:"timeline"`

	// Caps is the capability snapshot: the static operation matrix, the
	// side-effect-free probe outcomes and the manual write probe.
	Caps diag.Snapshot `json:"caps"`

	// Writeback is the dirty-cache ledger with its categories resolved to
	// Chinese labels and permanent/transient hints (AC 5).
	Writeback []WritebackView `json:"writeback"`

	// Unsupported is the fixed statement about copy/overwrite (AC 5).
	Unsupported string `json:"unsupported"`

	// Opts are the wizard's current choices, prefilled for the next apply.
	Opts MountOptions `json:"opts"`

	// HasCredential says a durable credential existed at process start (or
	// was applied since); the wizard banner keys on it.
	HasCredential bool `json:"has_credential"`

	// CacheDir and MountPoint say where this instance's data lives.
	CacheDir   string `json:"cache_dir"`
	MountPoint string `json:"mount_point"`

	// Autostart and UpgradeVersion reflect the shell hooks (issue #25).
	Autostart      bool   `json:"autostart"`
	UpgradeVersion string `json:"upgrade_version,omitempty"`

	// Log is the redacted event tail shown on the diagnostics view; the full
	// export is /api/log.
	Log []LogEntry `json:"log"`
}

// StatusView is the main panel's headline: Chinese state, remedy, and the
// mount's identity.
type StatusView struct {
	State       string    `json:"state"`
	Label       string    `json:"label"`
	Remedy      string    `json:"remedy"`
	Phase       string    `json:"phase"`
	Err         string    `json:"error,omitempty"`
	Since       time.Time `json:"since"`
	NextRetry   time.Time `json:"next_retry,omitempty"`
	Permanent   bool      `json:"permanent"`
	RootPath    string    `json:"root_path"`
	NeedsReauth bool      `json:"needs_reauth"`
}

// TimelineView is one row of the recovery timeline.
type TimelineView struct {
	State string    `json:"state"`
	Label string    `json:"label"`
	Phase string    `json:"phase"`
	Err   string    `json:"error,omitempty"`
	Since time.Time `json:"since"`
}

// WritebackView is one row of the dirty-cache ledger, with the category a
// user can read and the permanent/transient hint (AC 5: a transient failure
// must not read as a permanent condition).
type WritebackView struct {
	Path          string    `json:"path"`
	Category      string    `json:"category"`
	CategoryLabel string    `json:"category_label"`
	Permanent     bool      `json:"permanent"`
	Retries       int       `json:"retries"`
	NextRetry     time.Time `json:"next_retry,omitempty"`
	Err           string    `json:"error,omitempty"`
}
