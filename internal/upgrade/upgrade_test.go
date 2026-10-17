package upgrade

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/muou000/quark-rclone/internal/preflight"
)

// payload builds a release-shaped payload in a temp dir: both executables and
// a runtime bundle, with distinct content per version.
func payload(t *testing.T, trayBody, adapterBody, rcloneBody string) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(TrayExeName, trayBody)
	write(AdapterExeName, adapterBody)
	write(filepath.Join(BundleDirName, preflight.RcloneExeName), rcloneBody)
	return dir
}

func activeRoot(t *testing.T, trayBody, adapterBody, rcloneBody string) string {
	t.Helper()
	root := t.TempDir()
	p := payload(t, trayBody, adapterBody, rcloneBody)
	for _, rel := range []string{TrayExeName, AdapterExeName, filepath.Join(BundleDirName, preflight.RcloneExeName)} {
		from := filepath.Join(p, rel)
		to := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(from)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(to, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func okBundle(context.Context, string) error { return nil }

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func TestStageWritesManifestAndVerifies(t *testing.T) {
	root := t.TempDir()
	m := New(root, okBundle)

	if err := m.Stage(context.Background(), "v1.2.3", payload(t, "new-tray", "new-adapter", "new-rclone")); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	version, err := m.StagedVersion()
	if err != nil || version != "v1.2.3" {
		t.Fatalf("StagedVersion = %q, %v; want v1.2.3", version, err)
	}

	// The manifest hashes every payload file.
	man, err := readManifest(filepath.Join(m.StagingDir(), version))
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	paths := map[string]bool{}
	for _, f := range man.Files {
		paths[f.Path] = true
	}
	for _, want := range []string{TrayExeName, AdapterExeName, filepath.Join(BundleDirName, preflight.RcloneExeName)} {
		if !paths[want] {
			t.Errorf("manifest missing %s: %v", want, man.Files)
		}
	}
}

func TestStageRejectsIncompletePayload(t *testing.T) {
	root := t.TempDir()
	m := New(root, okBundle)
	bad := t.TempDir()
	if err := os.WriteFile(filepath.Join(bad, TrayExeName), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.Stage(context.Background(), "v1", bad); err == nil {
		t.Fatal("a payload without the adapter and bundle must be rejected")
	}
	if version, _ := m.StagedVersion(); version != "" {
		t.Errorf("staged = %q, want nothing after a rejected payload", version)
	}
}

func TestStageVerifiesTheBundleBeforeCommitting(t *testing.T) {
	root := t.TempDir()
	m := New(root, func(context.Context, string) error { return errors.New("rclone manifest mismatch") })
	if err := m.Stage(context.Background(), "v1", payload(t, "t", "a", "r")); err == nil {
		t.Fatal("a failing bundle self-check must fail the Stage")
	}
	if version, _ := m.StagedVersion(); version != "" {
		t.Errorf("staged = %q, want the failed staging cleaned up", version)
	}
}

func TestActivateSwapsAndKeepsBackup(t *testing.T) {
	root := activeRoot(t, "old-tray", "old-adapter", "old-rclone")
	m := New(root, okBundle)

	if err := m.Stage(context.Background(), "v2", payload(t, "new-tray", "new-adapter", "new-rclone")); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if err := m.Activate(context.Background()); err != nil {
		t.Fatalf("Activate: %v", err)
	}

	// The active payload is the new one.
	if got := readFile(t, filepath.Join(root, TrayExeName)); got != "new-tray" {
		t.Errorf("active tray = %q", got)
	}
	if got := readFile(t, filepath.Join(root, BundleDirName, preflight.RcloneExeName)); got != "new-rclone" {
		t.Errorf("active rclone = %q", got)
	}
	// The previous payload is the user's rollback material.
	if got := readFile(t, filepath.Join(root, backupDirName, TrayExeName)); got != "old-tray" {
		t.Errorf("backup tray = %q", got)
	}
	// Staging is consumed.
	if version, _ := m.StagedVersion(); version != "" {
		t.Errorf("staged = %q, want consumed", version)
	}
	// The active manifest names the new version for the panel.
	if !strings.Contains(readFile(t, filepath.Join(root, manifestName)), `"v2"`) {
		t.Error("active manifest should name the new version")
	}
}

func TestActivateRollsBackWhenPostCheckFails(t *testing.T) {
	root := activeRoot(t, "old-tray", "old-adapter", "old-rclone")

	// The self-check accepts every bundle except the ACTIVE one carrying the
	// new payload: the pre-swap staging check passes, the post-swap one
	// fails, which is exactly the rollback path of ADR 0006.
	m := New(root, func(_ context.Context, dir string) error {
		if dir == filepath.Join(root, BundleDirName) {
			data, err := os.ReadFile(filepath.Join(dir, preflight.RcloneExeName))
			if err != nil {
				return err
			}
			if strings.Contains(string(data), "new-rclone") {
				return errors.New("升级后自检未通过")
			}
		}
		return nil
	})

	if err := m.Stage(context.Background(), "v2", payload(t, "new-tray", "new-adapter", "new-rclone")); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if err := m.Activate(context.Background()); err == nil {
		t.Fatal("a failing post-swap self-check must fail the Activate")
	}

	// Everything is back to the old payload, byte for byte.
	if got := readFile(t, filepath.Join(root, TrayExeName)); got != "old-tray" {
		t.Errorf("active tray after rollback = %q", got)
	}
	if got := readFile(t, filepath.Join(root, AdapterExeName)); got != "old-adapter" {
		t.Errorf("active adapter after rollback = %q", got)
	}
	if got := readFile(t, filepath.Join(root, BundleDirName, preflight.RcloneExeName)); got != "old-rclone" {
		t.Errorf("active rclone after rollback = %q", got)
	}
	// The new manifest left with the swap; the rolled-back root must not
	// claim a version it does not run.
	if _, err := os.Stat(filepath.Join(root, manifestName)); !errors.Is(err, os.ErrNotExist) {
		t.Error("the rolled-back root must not keep the new manifest")
	}
}

func TestActivateWithoutStagingFails(t *testing.T) {
	m := New(t.TempDir(), okBundle)
	if err := m.Activate(context.Background()); err == nil {
		t.Fatal("Activate without staging must fail")
	}
}

// TestFirstInstallRollbackSaysSoHonestly pins the no-previous-version case:
// the swap fails, the staged payload is removed, and the error does not claim
// a rollback that never happened.
func TestFirstInstallRollbackSaysSoHonestly(t *testing.T) {
	root := t.TempDir()
	m := New(root, func(_ context.Context, dir string) error {
		// Reject the activated bundle, but let the staged one pass: the
		// failure is in the post-swap self-check, exactly like a real
		// broken payload would behave.
		if dir == filepath.Join(root, BundleDirName) {
			return errors.New("升级后自检未通过")
		}
		return nil
	})
	if err := m.Stage(context.Background(), "v1", payload(t, "new-tray", "new-adapter", "new-rclone")); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	err := m.Activate(context.Background())
	if err == nil {
		t.Fatal("a failing post-swap self-check must fail the Activate")
	}
	if strings.Contains(err.Error(), "已回滚") {
		t.Errorf("first-install rollback message lies: %v", err)
	}
	for _, rel := range []string{TrayExeName, AdapterExeName, BundleDirName, manifestName} {
		if _, statErr := os.Stat(filepath.Join(root, rel)); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("%s survived the failed first-install swap", rel)
		}
	}
}

func TestActivateDetectsTamperedStaging(t *testing.T) {
	root := t.TempDir()
	m := New(root, okBundle)
	if err := m.Stage(context.Background(), "v2", payload(t, "new-tray", "new-adapter", "new-rclone")); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	// Tamper after staging: the integrity check must stop the swap before
	// anything moves.
	staged := filepath.Join(m.StagingDir(), "v2")
	if err := os.WriteFile(filepath.Join(staged, TrayExeName), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.Activate(context.Background()); err == nil {
		t.Fatal("a tampered staging must fail the Activate")
	}
	if _, err := os.Stat(filepath.Join(root, TrayExeName)); !errors.Is(err, os.ErrNotExist) {
		t.Error("nothing may move on a tampered staging")
	}
}
