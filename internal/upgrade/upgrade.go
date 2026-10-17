// Package upgrade implements the ADR 0006 upgrade staging: a new application
// payload (both executables plus the runtime bundle) is staged under the
// application root, integrity-checked, and swapped in only after the mount
// has stopped — keeping the profile, the DPAPI credential and the per-letter
// dirty cache untouched. A failed post-swap self-check rolls the previous
// version back.
//
// The layout under the application root is:
//
//	quark-tray.exe / quark-adapter.exe   (active)
//	bundle\                              (active runtime bundle)
//	staging\<version>\                   (new payload, staged and hashed)
//	backup\                              (previous payload, kept for rollback)
//
// The swap is rename-based and stays inside the root, so it works on the
// same volume without elevation, and even a running tray executable can be
// renamed out of the way. The caller must stop the mount first (ADR 0006:
// 先停止挂载并等待盘符消失), which is what frees the bundle for renaming.
package upgrade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/muou000/quark-rclone/internal/preflight"
)

// The payload's fixed members. A staged version missing any of them is
// rejected before anything moves.
const (
	TrayExeName    = "quark-tray.exe"
	AdapterExeName = "quark-adapter.exe"
	BundleDirName  = "bundle"

	stagingDirName = "staging"
	backupDirName  = "backup"
	manifestName   = "manifest.json"
)

// requiredFiles is the minimum a staged payload must carry for the swap to
// make sense.
var requiredFiles = []string{
	TrayExeName,
	AdapterExeName,
	filepath.Join(BundleDirName, preflight.RcloneExeName),
}

// FileHash is one payload file's integrity record.
type FileHash struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// Manifest is the staged payload's integrity statement, written by Stage and
// re-verified by Activate before anything moves.
type Manifest struct {
	Version string     `json:"version"`
	Files   []FileHash `json:"files"`
}

// Manager owns one application root's staging and swap.
type Manager struct {
	root string
	// verifyBundle is the runtime-bundle self-check of ADR 0006: rclone's
	// version and built-in SHA-256 manifest. Production passes
	// preflight.CheckRclone; tests inject a scriptable checker.
	verifyBundle func(ctx context.Context, bundleDir string) error
}

// New builds the upgrade manager over an application root.
func New(root string, verifyBundle func(ctx context.Context, bundleDir string) error) *Manager {
	return &Manager{root: root, verifyBundle: verifyBundle}
}

// StagingDir is where staged payloads live.
func (m *Manager) StagingDir() string { return filepath.Join(m.root, stagingDirName) }

// Stage copies a release payload into staging\<version>, hashes every file
// into a manifest and self-checks the staged bundle. A previous staging or
// backup is replaced: the previous active version is still in place, and the
// backup only exists between Activate steps.
func (m *Manager) Stage(ctx context.Context, version, payloadDir string) error {
	if version == "" {
		return errors.New("upgrade: 需要版本号")
	}
	if err := checkPayloadShape(payloadDir); err != nil {
		return err
	}
	if err := os.RemoveAll(m.StagingDir()); err != nil {
		return fmt.Errorf("upgrade: clear staging: %w", err)
	}
	if err := os.RemoveAll(filepath.Join(m.root, backupDirName)); err != nil {
		return fmt.Errorf("upgrade: clear backup: %w", err)
	}

	staged := filepath.Join(m.StagingDir(), version)
	if err := copyTree(payloadDir, staged); err != nil {
		return err
	}
	man, err := hashTree(staged)
	if err != nil {
		return err
	}
	man.Version = version
	data, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(staged, manifestName), append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("upgrade: write manifest: %w", err)
	}
	if err := m.verifyBundle(ctx, filepath.Join(staged, BundleDirName)); err != nil {
		// A payload that fails its own self-check must not sit in staging
		// looking installable.
		_ = os.RemoveAll(m.StagingDir())
		return fmt.Errorf("upgrade: 暂存载荷自检未通过：%w", err)
	}
	return nil
}

// StagedVersion reports the version waiting in staging, or "" when nothing
// is staged. More than one staged version is an operator error and fails
// loudly.
func (m *Manager) StagedVersion() (string, error) {
	entries, err := os.ReadDir(m.StagingDir())
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("upgrade: read staging: %w", err)
	}
	var versions []string
	for _, e := range entries {
		if e.IsDir() {
			versions = append(versions, e.Name())
		}
	}
	switch len(versions) {
	case 0:
		return "", nil
	case 1:
		return versions[0], nil
	default:
		return "", fmt.Errorf("upgrade: staging holds %d versions (%v); 清理 %s 后重试",
			len(versions), versions, m.StagingDir())
	}
}

// Activate swaps the staged payload into the active root. The mount must be
// stopped — that is what frees the bundle for renaming (ADR 0006). The
// previous payload lands in backup\ and comes back on any failure after the
// swap began; a successful swap is followed by the post-swap self-check.
func (m *Manager) Activate(ctx context.Context) error {
	version, err := m.StagedVersion()
	if err != nil {
		return err
	}
	if version == "" {
		return errors.New("upgrade: 暂无已暂存的升级，请先执行 upgrade-stage")
	}
	staged := filepath.Join(m.StagingDir(), version)

	man, err := readManifest(staged)
	if err != nil {
		return err
	}
	if man.Version != version {
		return fmt.Errorf("upgrade: manifest 版本 %q 与目录 %q 不一致", man.Version, version)
	}
	if err := verifyTree(staged, man); err != nil {
		return err
	}
	for _, req := range requiredFiles {
		if _, err := os.Stat(filepath.Join(staged, req)); err != nil {
			return fmt.Errorf("upgrade: 暂存载荷缺少 %s：%w", req, err)
		}
	}
	if err := m.verifyBundle(ctx, filepath.Join(staged, BundleDirName)); err != nil {
		return fmt.Errorf("upgrade: 暂存载荷自检未通过：%w", err)
	}

	// The swap. Backup moves are recorded for restoration; staged installs
	// are recorded for removal — a failure after the first move rolls the
	// whole set back, and the error says honestly whether there was a
	// previous version to roll back to.
	backup := filepath.Join(m.root, backupDirName)
	_ = os.RemoveAll(backup)
	if err := os.MkdirAll(backup, 0o700); err != nil {
		return fmt.Errorf("upgrade: create backup: %w", err)
	}

	var backupMoves []move
	var installed []string
	rollback := func(cause error) error {
		// The staged copies sitting at the active paths are garbage in a
		// failed swap: clear them first (a directory cannot be renamed over
		// an existing directory on Windows), then move the backups back.
		for _, path := range installed {
			if info, err := os.Stat(path); err == nil && info.IsDir() {
				_ = os.RemoveAll(path)
			} else {
				_ = os.Remove(path)
			}
		}
		for i := len(backupMoves) - 1; i >= 0; i-- {
			mv := backupMoves[i]
			if info, err := os.Stat(mv.from); err == nil && info.IsDir() {
				_ = os.RemoveAll(mv.from)
			}
			_ = os.Rename(mv.to, mv.from)
		}
		if len(backupMoves) == 0 {
			// Nothing was active before the swap: there is no previous
			// version to roll back to, and saying otherwise would lie.
			return fmt.Errorf("upgrade: 切换失败：%w", cause)
		}
		return fmt.Errorf("upgrade: 切换失败，已回滚上一版本：%w", cause)
	}

	backUp := func(name string) error {
		from := filepath.Join(m.root, name)
		if _, err := os.Stat(from); errors.Is(err, os.ErrNotExist) {
			return nil // nothing active to back up (first install)
		}
		if err := os.Rename(from, filepath.Join(backup, name)); err != nil {
			return err
		}
		backupMoves = append(backupMoves, move{from: from, to: filepath.Join(backup, name)})
		return nil
	}
	install := func(rel string) error {
		to := filepath.Join(m.root, rel)
		if err := os.Rename(filepath.Join(staged, rel), to); err != nil {
			return err
		}
		installed = append(installed, to)
		return nil
	}

	// Active → backup. Renaming a running tray executable works on NTFS; the
	// bundle is free because the mount was stopped first.
	for _, name := range []string{TrayExeName, AdapterExeName} {
		if err := backUp(name); err != nil {
			return rollback(fmt.Errorf("back up %s: %w", name, err))
		}
	}
	if err := backUp(BundleDirName); err != nil {
		return rollback(fmt.Errorf("back up bundle: %w", err))
	}

	// Staged → active.
	for _, rel := range []string{TrayExeName, AdapterExeName, BundleDirName, manifestName} {
		if err := install(rel); err != nil {
			return rollback(fmt.Errorf("activate %s: %w", rel, err))
		}
	}

	// The post-swap self-check of ADR 0006: the activated bundle must pass
	// the same gate the pre-swap one did. Failure rolls everything back.
	if err := m.verifyBundle(ctx, filepath.Join(m.root, BundleDirName)); err != nil {
		return rollback(fmt.Errorf("升级后自检未通过：%w", err))
	}

	// Success: the staged copy is consumed, the backup stays as the user's
	// rollback until the next Stage replaces it.
	if err := os.RemoveAll(m.StagingDir()); err != nil {
		fmt.Fprintf(os.Stderr, "upgrade: could not clear staging: %v\n", err)
	}
	return nil
}

// move is one performed rename, remembered for rollback.
type move struct{ from, to string }

// VerifyWithPreflight is the production bundle self-check: the same rclone
// version and SHA-256 gate every mount passes (ADR 0006). Both front ends
// hand this to New.
func VerifyWithPreflight(ctx context.Context, bundleDir string) error {
	rc := preflight.CheckRclone(ctx, bundleDir, nil)
	if !rc.OK {
		return errors.New(rc.Detail)
	}
	return nil
}

// readManifest parses a staged payload's manifest.
func readManifest(dir string) (*Manifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, manifestName))
	if err != nil {
		return nil, fmt.Errorf("upgrade: read manifest: %w", err)
	}
	var man Manifest
	if err := json.Unmarshal(data, &man); err != nil {
		return nil, fmt.Errorf("upgrade: manifest corrupt: %w", err)
	}
	if man.Version == "" || len(man.Files) == 0 {
		return nil, errors.New("upgrade: manifest incomplete")
	}
	return &man, nil
}

// verifyTree re-hashes every manifest file against the payload directory.
func verifyTree(dir string, man *Manifest) error {
	for _, f := range man.Files {
		sum, err := fileSHA256(filepath.Join(dir, f.Path))
		if err != nil {
			return fmt.Errorf("upgrade: %s: %w", f.Path, err)
		}
		if sum != f.SHA256 {
			return fmt.Errorf("upgrade: %s 完整性校验失败（暂存载荷已损坏）", f.Path)
		}
	}
	return nil
}

// checkPayloadShape verifies the release payload has the members the swap
// needs, before anything is copied.
func checkPayloadShape(payloadDir string) error {
	info, err := os.Stat(payloadDir)
	if err != nil {
		return fmt.Errorf("upgrade: payload: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("upgrade: payload %s 不是目录", payloadDir)
	}
	for _, req := range requiredFiles {
		if _, err := os.Stat(filepath.Join(payloadDir, req)); err != nil {
			return fmt.Errorf("upgrade: payload 缺少 %s：%w", req, err)
		}
	}
	return nil
}

// copyTree copies a directory tree, preserving relative paths.
func copyTree(from, to string) error {
	return filepath.WalkDir(from, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		target := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		return copyFile(path, target)
	})
}

func copyFile(from, to string) error {
	src, err := os.Open(from)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer dst.Close()
	_, err = io.Copy(dst, src)
	return err
}

// hashTree hashes every file under dir into a manifest.
func hashTree(dir string) (*Manifest, error) {
	man := &Manifest{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == manifestName {
			return nil
		}
		sum, err := fileSHA256(path)
		if err != nil {
			return err
		}
		man.Files = append(man.Files, FileHash{Path: rel, SHA256: sum})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return man, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
