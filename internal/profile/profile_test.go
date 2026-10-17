package profile

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// On Windows these tests exercise the real DPAPI CurrentUser protect /
// unprotect pair: encrypting and decrypting inside one process is exactly the
// runtime contract. Off Windows the DPAPI stub refuses, so the round-trip
// test is skipped there rather than silently testing plaintext storage.

func newStore(t *testing.T) *Store {
	t.Helper()
	return Open(t.TempDir())
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// TestFreshOutcomeYieldsDefaults checks the first-run shape: no files, a
// default profile, and the ADR 0005 pinning (auto drive mode, writes cache).
func TestFreshOutcomeYieldsDefaults(t *testing.T) {
	s := newStore(t)
	cfg, outcome, err := s.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if outcome != OutcomeFresh {
		t.Fatalf("outcome = %s, want fresh", outcome)
	}
	if cfg.Version != CurrentVersion || cfg.Drive.Mode != DriveAuto || cfg.Writes.CacheMode != "writes" {
		t.Fatalf("default profile = %+v", cfg)
	}
	if cfg.Quark.RootFID != "0" {
		t.Fatalf("root fid = %q, want the drive root", cfg.Quark.RootFID)
	}
}

// TestSaveLoadRoundTripAndNoCredentialInConfig pins the separation contract:
// the profile JSON is non-sensitive by construction, and whatever the caller
// does with SaveCookie never leaks into it (issue #19).
func TestSaveLoadRoundTripAndNoCredentialInConfig(t *testing.T) {
	s := newStore(t)
	cfg := Default()
	cfg.Drive.Mode = DriveExplicit
	cfg.Drive.ExplicitLetter = "X"
	cfg.Autostart = true
	if err := s.SaveConfig(cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	if err := s.SaveCookie("__puus=secret-cookie-value"); err != nil {
		t.Fatalf("SaveCookie: %v", err)
	}

	got, outcome, err := s.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if outcome != OutcomeLoaded {
		t.Fatalf("outcome = %s, want loaded", outcome)
	}
	if got.Drive.Mode != DriveExplicit || got.Drive.ExplicitLetter != "X" || !got.Autostart {
		t.Fatalf("round trip lost data: %+v", got)
	}

	configText := readFile(t, s.ConfigPath())
	if strings.Contains(configText, "secret-cookie-value") {
		t.Fatal("the cookie leaked into the profile config")
	}
	if s.CookieExists() != true {
		t.Fatal("credential blob missing")
	}
}

// TestCorruptConfigKeepsOriginalAndDiagnoses checks the corrupt branch: the
// error names the file, and the original bytes survive untouched.
func TestCorruptConfigKeepsOriginalAndDiagnoses(t *testing.T) {
	s := newStore(t)
	junk := "{ this is not json"
	if err := os.MkdirAll(s.Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.ConfigPath(), []byte(junk), 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, err := s.LoadConfig()
	var corrupt *CorruptError
	if !errors.As(err, &corrupt) {
		t.Fatalf("err = %v, want CorruptError", err)
	}
	if corrupt.Path != s.ConfigPath() || corrupt.Size != len(junk) {
		t.Fatalf("diagnostics = %+v", corrupt)
	}
	if got := readFile(t, s.ConfigPath()); got != junk {
		t.Fatal("the corrupt file was modified")
	}
}

// TestUnknownNewerVersionRejected checks the forward-compatibility guard: a
// config from a newer build is refused with an upgrade hint and left alone.
func TestUnknownNewerVersionRejected(t *testing.T) {
	s := newStore(t)
	future := `{"version":99,"drive":{"mode":"auto"}}`
	if err := os.MkdirAll(s.Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.ConfigPath(), []byte(future), 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, err := s.LoadConfig()
	var upgrade *UpgradeRequiredError
	if !errors.As(err, &upgrade) {
		t.Fatalf("err = %v, want UpgradeRequiredError", err)
	}
	if upgrade.Found != 99 || upgrade.Current != CurrentVersion {
		t.Fatalf("upgrade error = %+v", upgrade)
	}
	if !strings.Contains(err.Error(), "upgrade") {
		t.Fatalf("error must tell the user to upgrade: %v", err)
	}
	if got := readFile(t, s.ConfigPath()); got != future {
		t.Fatal("the future config was modified")
	}
}

// TestMigrationFromV0WithBackup walks the happy migration path: the v0
// profile gains the current version and defaults, and the exact original
// bytes are snapshotted to the backup before anything is replaced.
func TestMigrationFromV0WithBackup(t *testing.T) {
	s := newStore(t)
	v0 := `{"drive":{"mode":"explicit","explicit_letter":"Y"},"autostart":true}`
	if err := os.MkdirAll(s.Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.ConfigPath(), []byte(v0), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, outcome, err := s.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if outcome != OutcomeMigrated {
		t.Fatalf("outcome = %s, want migrated", outcome)
	}
	if cfg.Version != CurrentVersion || cfg.Drive.Mode != DriveExplicit ||
		cfg.Drive.ExplicitLetter != "Y" || !cfg.Autostart {
		t.Fatalf("migrated profile = %+v", cfg)
	}
	if cfg.Writes.CacheMode != "writes" || cfg.Network.Mode != "direct" {
		t.Fatalf("migration did not fill defaults: %+v", cfg)
	}
	if got := readFile(t, s.BackupPath()); got != v0 {
		t.Fatalf("backup = %q, want the original v0 bytes", got)
	}
	// The live file now carries the current version.
	if !strings.Contains(readFile(t, s.ConfigPath()), `"version": 1`) {
		t.Fatal("the live config was not upgraded")
	}
}

// TestMigrationFailurePreservesOriginal pins the rollback guarantee: a v0
// profile whose values no longer validate fails the migration, keeps the
// original file exactly as it was, and reports an error the supervisor maps
// to Failed.
func TestMigrationFailurePreservesOriginal(t *testing.T) {
	s := newStore(t)
	v0 := `{"drive":{"mode":"hoverboard"},"autostart":true}`
	if err := os.MkdirAll(s.Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.ConfigPath(), []byte(v0), 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, err := s.LoadConfig()
	var migErr *MigrationError
	if !errors.As(err, &migErr) {
		t.Fatalf("err = %v, want MigrationError", err)
	}
	if migErr.From != 0 {
		t.Fatalf("migration from = %d", migErr.From)
	}
	if got := readFile(t, s.ConfigPath()); got != v0 {
		t.Fatal("the original config was overwritten on failure")
	}
}

// TestRestoreConfigBackup rolls a migrated profile back to the snapshot.
func TestRestoreConfigBackup(t *testing.T) {
	s := newStore(t)
	if err := s.RestoreConfigBackup(); !errors.Is(err, ErrNoBackup) {
		t.Fatalf("rollback without backup = %v, want ErrNoBackup", err)
	}

	v0 := `{"drive":{"mode":"auto"}}`
	if err := os.MkdirAll(s.Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.ConfigPath(), []byte(v0), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.LoadConfig(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := s.RestoreConfigBackup(); err != nil {
		t.Fatalf("RestoreConfigBackup: %v", err)
	}
	if got := readFile(t, s.ConfigPath()); got != v0 {
		t.Fatalf("config after rollback = %q, want the v0 snapshot", got)
	}
}

// TestSaveConfigRefusesForeignVersions checks the save guard: this build only
// writes its own version, so a caller cannot silently downgrade or fabricate
// a future file.
func TestSaveConfigRefusesForeignVersions(t *testing.T) {
	s := newStore(t)
	cfg := Default()
	cfg.Version = 0
	if err := s.SaveConfig(cfg); err == nil {
		t.Fatal("saved a version-0 config")
	}
	cfg.Version = CurrentVersion + 5
	if err := s.SaveConfig(cfg); err == nil {
		t.Fatal("saved a future config")
	}
}

// TestAtomicSaveLeavesNoTempFiles checks the atomic-write hygiene: after a
// save there is no temp file left in the store directory, because a rename
// either completed or the temp was removed.
func TestAtomicSaveLeavesNoTempFiles(t *testing.T) {
	s := newStore(t)
	if err := s.SaveConfig(Default()); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	entries, err := os.ReadDir(s.Dir())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") && strings.Contains(e.Name(), ".tmp") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

// TestCookieRoundTripDPAPI exercises the real DPAPI CurrentUser protect /
// unprotect pair (on Windows) and pins that only the blob file exists.
func TestCookieRoundTripDPAPI(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("DPAPI is a Windows facility; the non-Windows stub refuses to store")
	}
	s := newStore(t)
	if err := s.SaveCookie("  __puus=roundtrip; __pus=abc  "); err != nil {
		t.Fatalf("SaveCookie: %v", err)
	}
	got, err := s.LoadCookie()
	if err != nil {
		t.Fatalf("LoadCookie: %v", err)
	}
	if got != "__puus=roundtrip; __pus=abc" {
		t.Fatalf("cookie = %q", got)
	}
	// The blob must not carry the plaintext.
	if strings.Contains(readFile(t, s.CredPath()), "roundtrip") {
		t.Fatal("the credential blob contains plaintext")
	}
	if err := s.DeleteCookie(); err != nil {
		t.Fatalf("DeleteCookie: %v", err)
	}
	if _, err := s.LoadCookie(); !errors.Is(err, ErrNoCredential) {
		t.Fatalf("load after delete = %v, want ErrNoCredential", err)
	}
	if err := s.DeleteCookie(); err != nil {
		t.Fatalf("second delete: %v", err)
	}
}

// TestSaveCookieRefusesEmpty checks the guard against storing nothing.
func TestSaveCookieRefusesEmpty(t *testing.T) {
	s := newStore(t)
	if err := s.SaveCookie(""); err == nil {
		t.Fatal("saved an empty credential")
	}
	if err := s.SaveCookie("   "); err == nil {
		t.Fatal("saved a whitespace credential")
	}
}

// TestLoadCookieCorruptBlobDiagnoses checks a foreign or truncated blob
// surfaces as a diagnosable corrupt error rather than a DPAPI panic.
func TestLoadCookieCorruptBlobDiagnoses(t *testing.T) {
	s := newStore(t)
	if err := os.MkdirAll(s.Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.CredPath(), []byte("not-a-blob"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := s.LoadCookie()
	var corrupt *CorruptError
	if !errors.As(err, &corrupt) {
		t.Fatalf("err = %v, want CorruptError", err)
	}
	if corrupt.Path != s.CredPath() {
		t.Fatalf("diagnostics path = %s", corrupt.Path)
	}
}

// TestMountPointPinsTheAutoRecoveryPreference checks the profile's own
// drive-letter strategy (ADR 0005): explicit wins when set, auto falls back
// to the last actually-used letter, and nothing yields an empty string the
// caller must fill.
func TestMountPointPinsTheAutoRecoveryPreference(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{
			name: "explicit letter",
			cfg:  Config{Drive: DriveSettings{Mode: DriveExplicit, ExplicitLetter: "X"}},
			want: "X:",
		},
		{
			name: "explicit without letter",
			cfg:  Config{Drive: DriveSettings{Mode: DriveExplicit}},
			want: "",
		},
		{
			name: "auto with last letter",
			cfg:  Config{Drive: DriveSettings{Mode: DriveAuto, LastLetter: "Y"}},
			want: "Y:",
		},
		{
			name: "auto without history",
			cfg:  Config{Drive: DriveSettings{Mode: DriveAuto}},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.MountPoint(); got != tc.want {
				t.Fatalf("MountPoint() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestNormalizeDriveLetter pins the mount-point → letter canonicalisation the
// supervisor uses to update the last-used record.
func TestNormalizeDriveLetter(t *testing.T) {
	cases := map[string]string{
		"x:":        "X",
		"X":         "X",
		`x:\dir`:    "X",
		" x:/":      "X",
		"y:/mount/": "Y",
		"":          "",
	}
	for in, want := range cases {
		if got := NormalizeDriveLetter(in); got != want {
			t.Errorf("NormalizeDriveLetter(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestDefaultDirUnderLocalAppData pins the ADR 0005 location on Windows.
func TestDefaultDirUnderLocalAppData(t *testing.T) {
	t.Setenv("LocalAppData", `C:\Users\test\AppData\Local`)
	dir, err := DefaultDir()
	if err != nil {
		t.Fatalf("DefaultDir: %v", err)
	}
	want := filepath.Join(`C:\Users\test\AppData\Local`, "QuarkDriveMount")
	if dir != want {
		t.Fatalf("dir = %q, want %q", dir, want)
	}
}
