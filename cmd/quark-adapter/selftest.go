package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/muou000/quark-rclone/internal/preflight"
	"github.com/muou000/quark-rclone/internal/supervisor"
)

// selfTestOpts bundles one self-test's inputs, so the builder grows a struct
// instead of a parameter list.
type selfTestOpts struct {
	bundleDir  string
	profileDir string
	cookieFile string
	useProfile bool
	port       int
}

// buildSelfTest assembles one real mount attempt for the pre-check's
// self-test gate: a session on the loopback endpoint plus the bundled rclone
// mounting the probe letter. Its error text is what the classifier reads —
// which is how one attempt comes out labelled WinFspRequired, DriveConflict
// or a far-side network/authentication failure (issue #20).
func buildSelfTest(_ context.Context, o selfTestOpts) (func(ctx context.Context) error, error) {
	cookie, persist, _, cfg, _, err := loadCredential(o.cookieFile, o.useProfile, o.profileDir)
	if err != nil {
		return nil, err
	}

	return func(testCtx context.Context) error {
		sess, err := supervisor.Start(testCtx, supervisor.Config{Port: o.port, Cookie: cookie, Persist: persist})
		if err != nil {
			return err
		}
		defer sess.Stop()

		rcloneExe := filepath.Join(o.bundleDir, preflight.RcloneExeName)
		if _, err := os.Stat(rcloneExe); err != nil {
			return fmt.Errorf("missing bundled rclone: %w", err)
		}
		// The probe letter is the profile's own preference, or Q: — a letter
		// conflict here is part of what the self-test exists to classify.
		probe := cfg.MountPoint()
		if probe == "" {
			probe = "Q:"
		}
		// A letter already held by another volume would make the visibility
		// probe below succeed the instant it ran, reporting a mount that
		// never happened. Detect it before rclone starts so the failure
		// classifies as DriveConflict.
		if _, err := os.Stat(probe + `\`); err == nil {
			return fmt.Errorf("mount: drive letter %s is already in use", probe)
		}

		cmd := exec.Command(rcloneExe, "mount", "quark:", probe, "--config", sess.ConfigPath())
		var stderr strings.Builder
		cmd.Stderr = &stderr
		if err := cmd.Start(); err != nil {
			return err
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()

		// A mounted WebDAV drive is visible through the filesystem within a
		// few seconds; a driver or letter problem shows up as an early exit.
		deadline := time.Now().Add(30 * time.Second)
		for {
			if _, err := os.Stat(probe + `\`); err == nil {
				_ = cmd.Process.Kill()
				<-done
				return nil
			}
			select {
			case err := <-done:
				if err != nil {
					return errors.New(stderr.String())
				}
				return errors.New("rclone exited without mounting")
			case <-testCtx.Done():
				_ = cmd.Process.Kill()
				return testCtx.Err()
			default:
			}
			if time.Now().After(deadline) {
				_ = cmd.Process.Kill()
				<-done
				return fmt.Errorf("mount did not appear within 30s: %s", stderr.String())
			}
			time.Sleep(500 * time.Millisecond)
		}
	}, nil
}
