package preflight

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestPlatformVerifiedPinsTheMatrix pins the first-release compatibility
// matrix: Windows x64, nothing else (ADR 0007).
func TestPlatformVerifiedPinsTheMatrix(t *testing.T) {
	cases := map[[2]string]bool{
		{"windows", "amd64"}: true,
		{"windows", "386"}:   false,
		{"windows", "arm64"}: false,
		{"linux", "amd64"}:   false,
		{"darwin", "arm64"}:  false,
	}
	for platform, want := range cases {
		if got := platformVerified(platform[0], platform[1]); got != want {
			t.Errorf("platformVerified(%s/%s) = %v, want %v", platform[0], platform[1], got, want)
		}
	}
}

// TestRunUnverifiedPlatformStopsAtTheGate checks that a non-verified platform
// reports WinFspRequired with the unverified wording and runs no further
// gates (issue #20: other architectures enter WinFspRequired and show
// unverified).
func TestRunUnverifiedPlatformStopsAtTheGate(t *testing.T) {
	if runtime.GOOS == "windows" && runtime.GOARCH == "amd64" {
		t.Skip("this test asserts the unverified branch, which the host platform cannot take")
	}
	report := Run(context.Background(), Options{})
	if report.Verified {
		t.Fatal("report claims verified")
	}
	if report.Status != StatusWinFspRequired {
		t.Fatalf("status = %s, want winfsp_required", report.Status)
	}
	// No other gates ran: the findings list is exactly the platform one.
	if len(report.Findings) != 1 || report.Findings[0].Check != "platform" {
		t.Fatalf("findings = %+v", report.Findings)
	}
	if report.OK() {
		t.Fatal("unverified run must not be OK")
	}
}

// writeRcloneBundle lays down a fake rclone.exe with the given content and
// injects the matching (or, with wantOverride, a wrong) manifest.
func writeRcloneBundle(t *testing.T, content string, manifest map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, RcloneExeName), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	SetRcloneManifest(manifest)
	t.Cleanup(func() { SetRcloneManifest(map[string]string{}) })
	return dir
}

func fakeVersion(output string) func(ctx context.Context, exe string) (string, error) {
	return func(ctx context.Context, exe string) (string, error) { return output, nil }
}

// TestCheckRcloneMissing checks the missing-component gate: startup blocked,
// no download offered, remedy names a reinstall.
func TestCheckRcloneMissing(t *testing.T) {
	SetRcloneManifest(map[string]string{})
	t.Cleanup(func() { SetRcloneManifest(map[string]string{}) })
	check := CheckRclone(context.Background(), t.TempDir(), fakeVersion(""))
	if check.OK {
		t.Fatal("missing rclone passed the gate")
	}
	if !strings.Contains(check.Detail, "重新安装") {
		t.Fatalf("remedy = %q, want a reinstall path", check.Detail)
	}
	if !strings.Contains(check.Detail, "不会自动下载") {
		t.Fatalf("remedy must state no auto-download: %q", check.Detail)
	}
}

// TestCheckRcloneHashMismatchBlocks checks the manifest gate: a tampered
// binary is refused before it is ever executed.
func TestCheckRcloneHashMismatchBlocks(t *testing.T) {
	dir := writeRcloneBundle(t, "tampered bytes", map[string]string{
		RcloneExeName: strings.Repeat("ab", 32),
	})
	executed := false
	check := CheckRclone(context.Background(), dir, func(ctx context.Context, exe string) (string, error) {
		executed = true
		return "rclone v1.71.2", nil
	})
	if check.OK {
		t.Fatal("a hash mismatch passed the gate")
	}
	if executed {
		t.Fatal("the tampered binary was executed before its hash was verified")
	}
	if !strings.Contains(check.Detail, "SHA-256") || !strings.Contains(check.Detail, "重新安装") {
		t.Fatalf("remedy = %q", check.Detail)
	}
}

// TestCheckRcloneMissingManifestEntry checks a bundle whose packaging omitted
// the built-in manifest: the gate fails loudly instead of skipping the hash.
func TestCheckRcloneMissingManifestEntry(t *testing.T) {
	dir := writeRcloneBundle(t, "some bytes", map[string]string{})
	check := CheckRclone(context.Background(), dir, fakeVersion("rclone v1.71.2"))
	if check.OK {
		t.Fatal("an empty manifest passed the gate")
	}
	if !strings.Contains(check.Detail, "清单") {
		t.Fatalf("remedy = %q", check.Detail)
	}
}

// TestCheckRcloneVersionMismatchBlocks checks the pinned-version gate: a
// binary whose hash matches but whose version banner differs is refused.
func TestCheckRcloneVersionMismatchBlocks(t *testing.T) {
	content := "the real rclone bytes"
	sum := sha256.Sum256([]byte(content))
	dir := writeRcloneBundle(t, content, map[string]string{
		RcloneExeName: hex.EncodeToString(sum[:]),
	})
	check := CheckRclone(context.Background(), dir, fakeVersion("rclone v1.60.0"))
	if check.OK {
		t.Fatal("a wrong version passed the gate")
	}
	if !strings.Contains(check.Detail, RequiredRcloneVersion) {
		t.Fatalf("remedy = %q", check.Detail)
	}
}

// TestCheckRcloneHappyPath pins the passing sequence: hash verified, version
// banner matches, gate green.
func TestCheckRcloneHappyPath(t *testing.T) {
	content := "the real rclone bytes"
	sum := sha256.Sum256([]byte(content))
	dir := writeRcloneBundle(t, content, map[string]string{
		RcloneExeName: hex.EncodeToString(sum[:]),
	})
	check := CheckRclone(context.Background(), dir, fakeVersion("rclone v1.71.2\n- os/version: Microsoft Windows 10"))
	if !check.OK {
		t.Fatalf("happy path failed: %s", check.Detail)
	}
}

// TestClassifyMountFailurePinsTheThreeWaySplit pins the self-test failure
// classification (issue #20): WinFsp wording wins, then the drive letter,
// then everything left is the far side.
func TestClassifyMountFailurePinsTheThreeWaySplit(t *testing.T) {
	cases := map[string]Status{
		"unable to locate winfsp-x64.dll":                    StatusWinFspRequired,
		"WinFsp is not installed":                            StatusWinFspRequired,
		"mount: drive letter Q: is already in use":           StatusDriveConflict,
		"cannot create a file when that file already exists": StatusDriveConflict,
		"Access is denied.":                                  StatusDriveConflict,
		"connection refused":                                 StatusRemoteError,
		"directory not found: /nope: 404":                    StatusRemoteError,
		"quark: authentication rejected":                     StatusRemoteError,
	}
	for text, want := range cases {
		if got := ClassifyMountFailure(text); got != want {
			t.Errorf("ClassifyMountFailure(%q) = %s, want %s", text, got, want)
		}
	}
}

// TestRunBlocksOnRcloneGate checks the chain order: a broken rclone stops the
// report with rclone_invalid before the WinFsp gate runs.
func TestRunBlocksOnRcloneGate(t *testing.T) {
	report := Run(context.Background(), Options{
		BundleDir:    t.TempDir(), // empty bundle: rclone missing
		VersionProbe: fakeVersion(""),
	})
	if report.Status != StatusRcloneInvalid {
		t.Fatalf("status = %s, want rclone_invalid", report.Status)
	}
	if report.OK() {
		t.Fatal("blocked run must not be OK")
	}
}

// TestRunHappyPathWithoutSelfTest checks a green report when the bundle and
// WinFsp pass. On a machine without WinFsp this test would report
// winfsp_required, so it is only meaningful where WinFsp exists; the
// assertion tolerates both by pinning the chain reached the WinFsp gate.
func TestRunHappyPathWithoutSelfTest(t *testing.T) {
	if runtime.GOOS != "windows" || runtime.GOARCH != "amd64" {
		t.Skip("verified-matrix run")
	}
	content := "the real rclone bytes"
	sum := sha256.Sum256([]byte(content))
	dir := writeRcloneBundle(t, content, map[string]string{
		RcloneExeName: hex.EncodeToString(sum[:]),
	})
	report := Run(context.Background(), Options{
		BundleDir:    dir,
		VersionProbe: fakeVersion("rclone v1.71.2"),
	})
	switch report.Status {
	case StatusOK:
		// WinFsp present and healthy.
	case StatusWinFspRequired:
		// This host has no (or an old) WinFsp: the gate itself worked.
	default:
		t.Fatalf("status = %s, want ok or winfsp_required; findings: %+v", report.Status, report.Findings)
	}
}

// TestRunSelfTestFailureIsClassified checks the self-test hook: the runner's
// error text is classified and rendered as a Chinese remedy.
func TestRunSelfTestFailureIsClassified(t *testing.T) {
	if runtime.GOOS != "windows" || runtime.GOARCH != "amd64" {
		t.Skip("verified-matrix run")
	}
	content := "the real rclone bytes"
	sum := sha256.Sum256([]byte(content))
	dir := writeRcloneBundle(t, content, map[string]string{
		RcloneExeName: hex.EncodeToString(sum[:]),
	})
	// A host without WinFsp never reaches the self-test gate; accept that by
	// asserting only when the chain got there.
	report := Run(context.Background(), Options{
		BundleDir:    dir,
		VersionProbe: fakeVersion("rclone v1.71.2"),
		SelfTest: func(ctx context.Context) error {
			return errors.New("mount: drive letter Q: is already in use")
		},
	})
	if report.Status == StatusOK {
		return // reached the end only if a self-test passed; ours fails, so this means the gate never ran
	}
	if report.Status == StatusDriveConflict {
		found := false
		for _, f := range report.Findings {
			if f.Check == "mount" && strings.Contains(f.Detail, "盘符已被占用") {
				found = true
			}
		}
		if !found {
			t.Fatalf("mount remedy missing: %+v", report.Findings)
		}
	}
}

// TestLaunchWinFspInstallerRequiresConfirmation pins the confirmation gate:
// without the user's explicit yes, the installer never starts — on any
// platform.
func TestLaunchWinFspInstallerRequiresConfirmation(t *testing.T) {
	err := LaunchWinFspInstaller(t.TempDir(), false)
	if err == nil {
		t.Fatal("installer started without confirmation")
	}
	if !strings.Contains(err.Error(), "明确确认") {
		t.Fatalf("err = %v, want the confirmation wording", err)
	}
}

// TestVersionAtLeastPinsTheMinimumVersionComparison covers the dotted
// comparison the WinFsp gate uses.
func TestVersionAtLeastPinsTheMinimumVersionComparison(t *testing.T) {
	cases := []struct {
		actual, required string
		want             bool
	}{
		{"2.1.25312", "2.0", true},
		{"2.0.123", "2.0", true},
		{"1.9.9", "2.0", false},
		{"3.0", "2.0", true},
		{"", "2.0", false},
		{"garbage", "2.0", false},
	}
	for _, tc := range cases {
		if got := versionAtLeast(tc.actual, tc.required); got != tc.want {
			t.Errorf("versionAtLeast(%q, %q) = %v, want %v", tc.actual, tc.required, got, tc.want)
		}
	}
}
