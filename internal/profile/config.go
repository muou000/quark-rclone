// Package profile owns the single-account mount profile (ADR 0005) and the
// Windows credential store beside it (ADR 0003).
//
// The split is the one the ADRs name: user choices live in a versioned,
// non-sensitive config.json under %LocalAppData%\QuarkDriveMount; the Quark
// cookie lives in a versioned DPAPI CurrentUser blob that only the current
// Windows user can decrypt. Runtime state — the rclone temp config, the
// adapter port, one-time passwords, running PIDs — is never persisted here.
//
// Loading never destroys information: a corrupt file is kept for diagnostics,
// a migration failure keeps the original, and only a successful migration
// replaces it — with an atomic backup taken first, so an upgrade can roll
// back.
package profile

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// CurrentVersion is the configuration version this build writes and
// understands. Versions below it with a registered migration are upgraded on
// load; anything above is refused with an upgrade hint.
const CurrentVersion = 1

// DriveMode names the drive-letter selection strategy.
type DriveMode string

const (
	// DriveExplicit mounts at the user's chosen letter; if it is taken the
	// session enters DriveConflict instead of picking another.
	DriveExplicit DriveMode = "explicit"
	// DriveAuto prefers the last successfully used letter and falls back to
	// any free one, updating the record (ADR 0005).
	DriveAuto DriveMode = "auto"
)

// NetworkMode names how the adapter reaches the provider.
type NetworkMode string

const (
	// NetworkDirect connects without a proxy.
	NetworkDirect NetworkMode = "direct"
	// NetworkSystem follows the system proxy configuration.
	NetworkSystem NetworkMode = "system"
)

// Config is the whole mount profile. Every field is non-sensitive: the
// credential is stored separately through SaveCookie/LoadCookie and must
// never be added here.
type Config struct {
	Version     int             `json:"version"`
	Drive       DriveSettings   `json:"drive"`
	Quark       QuarkSettings   `json:"quark"`
	Autostart   bool            `json:"autostart"`
	Autorestore bool            `json:"autorestore"`
	CacheDir    string          `json:"cache_dir"`
	Writes      WritesSettings  `json:"writes"`
	Network     NetworkSettings `json:"network"`
}

// NetworkSettings records how the adapter should reach the provider.
type NetworkSettings struct {
	// Mode is direct or system (follow the system proxy). The adapter
	// honours it when building its HTTP transport.
	Mode NetworkMode `json:"mode"`
}

// DriveSettings records the drive-letter strategy and the last letter a mount
// actually used, which is what auto recovery and conflict reporting read.
type DriveSettings struct {
	// Mode selects explicit or auto letter choice.
	Mode DriveMode `json:"mode"`
	// ExplicitLetter is the user's chosen letter, e.g. "X". Empty with auto
	// mode.
	ExplicitLetter string `json:"explicit_letter"`
	// LastLetter is the most recent letter a mount session actually held.
	// Auto mode tries it first; a successful session updates it.
	LastLetter string `json:"last_letter"`
}

// QuarkSettings names the drive subtree the mount exposes.
type QuarkSettings struct {
	// RootFID is the FID of the exposed directory. The drive root is "0".
	RootFID string `json:"root_fid"`
	// RootPath is the display path of that directory, e.g. "/mounts/work".
	RootPath string `json:"root_path"`
}

// WritesSettings carries the rclone write-back parameters for
// vfs-cache-mode writes (ADR 0002/0005). Values are rclone duration and size
// strings so they can be passed through verbatim.
//
// A write_chunk_size field once lived here and was removed: rclone has no
// such flag, so it could never take effect. The drop needs no configuration
// version bump — Go's JSON decode ignores the stale key in an existing
// config.json, and nothing ever read it successfully anyway.
type WritesSettings struct {
	// CacheMode is the rclone --vfs-cache-mode value. The profile pins
	// "writes" for the first release.
	CacheMode string `json:"cache_mode"`
	// WriteBack maps to --vfs-write-back, e.g. "5s".
	WriteBack string `json:"write_back"`
}

// Default returns the profile a fresh install starts from.
func Default() Config {
	return Config{
		Version: CurrentVersion,
		Drive:   DriveSettings{Mode: DriveAuto},
		Quark:   QuarkSettings{RootFID: "0", RootPath: "/"},
		// Autorestore defaults on: a rebooted machine should re-mount what the
		// user had mounted, that is the point of keeping a profile.
		Autostart:   false,
		Autorestore: true,
		// CacheDir is the root under which each mount instance keeps its own
		// VFS cache directory (issue #23); empty uses a directory beside the
		// profile. The instance directory itself is derived per mount point
		// and never deleted automatically.
		CacheDir: "",
		Writes: WritesSettings{
			CacheMode: "writes",
			WriteBack: "5s",
		},
		Network: NetworkSettings{Mode: NetworkDirect},
	}
}

// MountPoint is the mount point this profile asks for when the caller did
// not name one: the explicit letter when pinned, otherwise the last letter a
// session actually held (ADR 0005's auto recovery). Empty means the caller
// must supply one.
func (c Config) MountPoint() string {
	switch c.Drive.Mode {
	case DriveExplicit:
		if c.Drive.ExplicitLetter != "" {
			return c.Drive.ExplicitLetter + ":"
		}
	case DriveAuto:
		if c.Drive.LastLetter != "" {
			return c.Drive.LastLetter + ":"
		}
	}
	return ""
}

// validate checks the invariants a stored profile must hold.
func (c Config) validate() error {
	if c.Version < 0 || c.Version > CurrentVersion {
		return fmt.Errorf("unsupported config version %d (this build writes %d)", c.Version, CurrentVersion)
	}
	switch c.Drive.Mode {
	case DriveExplicit, DriveAuto:
	default:
		return fmt.Errorf("unknown drive mode %q", c.Drive.Mode)
	}
	switch c.Network.Mode {
	case NetworkDirect, NetworkSystem:
	default:
		return fmt.Errorf("unknown network mode %q", c.Network.Mode)
	}
	return nil
}

// configFileName and backupFileName are fixed relative names inside the
// store directory.
const (
	configFileName = "config.json"
	backupFileName = "config.json.bak"
)

// CorruptError reports a config file that is not a readable profile. The
// original file is always preserved — the caller's remedy is diagnosis, not
// deletion (issue #19).
type CorruptError struct {
	Path string
	Size int
	Err  error
}

func (e *CorruptError) Error() string {
	return fmt.Sprintf("config at %s is unreadable (%d bytes); the file is kept for diagnostics: %v", e.Path, e.Size, e.Err)
}

func (e *CorruptError) Unwrap() error { return e.Err }

// UpgradeRequiredError reports a config written by a newer build. The file is
// untouched; the remedy is upgrading this application.
type UpgradeRequiredError struct {
	Found   int
	Current int
}

func (e *UpgradeRequiredError) Error() string {
	return fmt.Sprintf("config version %d is newer than this build supports (%d); upgrade the application", e.Found, e.Current)
}

// MigrationError reports a failed migration of a known older version. The
// original config file is preserved untouched (issue #19), and the supervisor
// answers the failure by entering Failed (ADR 0005).
type MigrationError struct {
	From int
	Err  error
}

func (e *MigrationError) Error() string {
	return fmt.Sprintf("migrating config from version %d failed; the original file is preserved: %v", e.From, e.Err)
}

func (e *MigrationError) Unwrap() error { return e.Err }

// ErrNoBackup is returned when a rollback is requested but no backup exists.
var ErrNoBackup = errors.New("no config backup exists")

// writeFileAtomic replaces path with data via a temp file in the same
// directory and a rename, so a crash mid-write never leaves a truncated file
// behind. The mode applies to the final file (0600 for credentials, 0644 for
// the non-sensitive config).
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		// A no-op once the rename succeeded.
		_ = os.Remove(tmpName)
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// marshalIndent is the one JSON rendering used for config files, so the file
// stays diffable and diagnostics stable.
func marshalIndent(v any) ([]byte, error) {
	return json.MarshalIndent(v, "", "  ")
}
