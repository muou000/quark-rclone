package profile

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Store is the mount profile's on-disk home: one directory holding
// config.json and the credential blob.
type Store struct {
	dir string
}

// DefaultDir is %LocalAppData%\QuarkDriveMount (ADR 0005). Outside Windows it
// falls back to the user config directory so tests and tooling still work.
func DefaultDir() (string, error) {
	if base := os.Getenv("LocalAppData"); base != "" {
		return filepath.Join(base, "QuarkDriveMount"), nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve profile dir: %w", err)
	}
	return filepath.Join(base, "QuarkDriveMount"), nil
}

// Open binds a store to a directory, creating nothing on disk until the
// first write.
func Open(dir string) *Store { return &Store{dir: dir} }

// Dir is the store directory.
func (s *Store) Dir() string { return s.dir }

// ConfigPath is where the non-sensitive profile lives.
func (s *Store) ConfigPath() string { return filepath.Join(s.dir, configFileName) }

// CredPath is where the DPAPI credential blob lives.
func (s *Store) CredPath() string { return filepath.Join(s.dir, credentialFileName) }

// BackupPath is where the pre-migration snapshot lives.
func (s *Store) BackupPath() string { return filepath.Join(s.dir, backupFileName) }

// Outcome reports how a LoadConfig resolved.
type Outcome string

const (
	// OutcomeFresh means no config file existed; the config is the default.
	OutcomeFresh Outcome = "fresh"
	// OutcomeLoaded means the current version was read as-is.
	OutcomeLoaded Outcome = "loaded"
	// OutcomeMigrated means a known older version was upgraded (a backup was
	// taken first).
	OutcomeMigrated Outcome = "migrated"
)

// LoadConfig reads the profile, applying the load semantics of issue #19:
//
//   - no file yet  → the default profile, OutcomeFresh
//   - current      → loaded as-is, OutcomeLoaded
//   - known older  → atomic backup, migrate, atomic replace, OutcomeMigrated
//   - corrupt      → CorruptError, original kept for diagnostics
//   - newer        → UpgradeRequiredError, original kept
//   - migration
//     failure      → MigrationError, original kept; the supervisor enters
//     Failed on this (ADR 0005)
func (s *Store) LoadConfig() (Config, Outcome, error) {
	raw, err := os.ReadFile(s.ConfigPath())
	if errors.Is(err, os.ErrNotExist) {
		return Default(), OutcomeFresh, nil
	}
	if err != nil {
		return Config{}, "", fmt.Errorf("read config: %w", err)
	}
	var probe struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return Config{}, "", &CorruptError{Path: s.ConfigPath(), Size: len(raw), Err: err}
	}
	switch {
	case probe.Version > CurrentVersion:
		return Config{}, "", &UpgradeRequiredError{Found: probe.Version, Current: CurrentVersion}
	case probe.Version == CurrentVersion:
		cfg, err := decodeCurrent(s.ConfigPath(), raw)
		if err != nil {
			return Config{}, "", err
		}
		return cfg, OutcomeLoaded, nil
	}
	migrate, ok := migrations[probe.Version]
	if !ok {
		// Every version below CurrentVersion must be known; this guard keeps
		// a forgotten migration from silently advancing the file.
		return Config{}, "", &MigrationError{From: probe.Version, Err: fmt.Errorf("no migration registered")}
	}
	cfg, err := s.migrateWithBackup(probe.Version, raw, migrate)
	if err != nil {
		return Config{}, "", err
	}
	return cfg, OutcomeMigrated, nil
}

// decodeCurrent unmarshals and validates a current-version profile.
func decodeCurrent(path string, raw []byte) (Config, error) {
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return Config{}, &CorruptError{Path: path, Size: len(raw), Err: err}
	}
	if err := cfg.validate(); err != nil {
		return Config{}, &CorruptError{Path: path, Size: len(raw), Err: err}
	}
	return cfg, nil
}

// migrateWithBackup takes the atomic backup first, then runs the migration
// and atomically replaces the config. The original is never touched on
// failure — that is the rollback guarantee (issue #19).
func (s *Store) migrateWithBackup(from int, raw []byte, migrate func(json.RawMessage) (Config, error)) (Config, error) {
	if err := writeFileAtomic(s.BackupPath(), raw, 0o600); err != nil {
		return Config{}, fmt.Errorf("backup before migration: %w", err)
	}
	cfg, err := migrate(raw)
	if err != nil {
		return Config{}, &MigrationError{From: from, Err: err}
	}
	if err := s.SaveConfig(cfg); err != nil {
		return Config{}, &MigrationError{From: from, Err: err}
	}
	return cfg, nil
}

// migrations holds one function per known old version. The first release
// stored profiles without a version field, which decodes as version 0.
var migrations = map[int]func(json.RawMessage) (Config, error){
	0: migrateV0,
}

// migrateV0 upgrades a version-less profile to the current version. A v0
// file has the same shape minus the version marker; the migration fills the
// defaults for fields v0 did not carry and rejects values that no longer
// name a legal choice.
func migrateV0(raw json.RawMessage) (Config, error) {
	var v0 Config
	if err := json.Unmarshal(raw, &v0); err != nil {
		return Config{}, err
	}
	v0.Version = CurrentVersion
	if v0.Quark.RootFID == "" {
		v0.Quark.RootFID = "0"
	}
	if v0.Quark.RootPath == "" {
		v0.Quark.RootPath = "/"
	}
	if v0.Writes.CacheMode == "" {
		v0.Writes.CacheMode = "writes"
	}
	if v0.Writes.WriteBack == "" {
		v0.Writes.WriteBack = "5s"
	}
	if v0.Writes.WriteChunkSize == "" {
		v0.Writes.WriteChunkSize = "128Mi"
	}
	if v0.Network.Mode == "" {
		v0.Network.Mode = "direct"
	}
	if v0.Drive.Mode == "" {
		v0.Drive.Mode = DriveAuto
	}
	if err := v0.validate(); err != nil {
		return Config{}, err
	}
	return v0, nil
}

// SaveConfig atomically replaces the profile. The config must carry the
// current version: saving an outdated or future version is a caller bug, not
// something to silently repair.
func (s *Store) SaveConfig(cfg Config) error {
	if cfg.Version != CurrentVersion {
		return fmt.Errorf("refusing to save config version %d; this build writes %d", cfg.Version, CurrentVersion)
	}
	if err := cfg.validate(); err != nil {
		return err
	}
	data, err := marshalIndent(cfg)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	return writeFileAtomic(s.ConfigPath(), append(data, '\n'), 0o644)
}

// RestoreConfigBackup rolls the config back to the pre-migration snapshot,
// leaving the backup in place so a repeated rollback stays possible. It is
// the manual half of the migration rollback guarantee (issue #19).
func (s *Store) RestoreConfigBackup() error {
	raw, err := os.ReadFile(s.BackupPath())
	if errors.Is(err, os.ErrNotExist) {
		return ErrNoBackup
	}
	if err != nil {
		return fmt.Errorf("read backup: %w", err)
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	return writeFileAtomic(s.ConfigPath(), raw, 0o644)
}

// CookieExists reports whether a credential blob is present, without
// decrypting it.
func (s *Store) CookieExists() bool {
	_, err := os.Stat(s.CredPath())
	return err == nil
}

// NormalizeDriveLetter canonicalises a user-supplied drive letter or mount
// point: "x:", "x:\dir" and "X" all become "X". An empty input stays empty.
// The supervisor uses it to update the last-used record from the mount point
// rclone was given.
func NormalizeDriveLetter(letter string) string {
	letter = strings.TrimSpace(letter)
	// A mount point may arrive as a full path; the drive letter is the part
	// before the first separator, and only then does the colon suffix come
	// off — trimming first would leave the colon of "x:\dir" behind.
	if i := strings.IndexAny(letter, `\/`); i >= 0 {
		letter = letter[:i]
	}
	letter = strings.TrimSuffix(letter, ":")
	return strings.ToUpper(letter)
}
