package supervisor

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// State is the unified mount-supervisor state set of ADR 0004/0005. The tray
// maps each repair action onto one state, so the set is closed: nothing here
// invents a new name for an existing condition.
type State string

const (
	StateDisabled          State = "Disabled"
	StateStarting          State = "Starting"
	StateWaitingForNetwork State = "WaitingForNetwork"
	StateRecovering        State = "Recovering"
	StateMounted           State = "Mounted"
	StateNeedsReauth       State = "NeedsReauth"
	StateDriveConflict     State = "DriveConflict"
	StateWinFspRequired    State = "WinFspRequired"
	StateFailed            State = "Failed"
	StateStopping          State = "Stopping"
)

// Paused reports whether the state suspends automatic retries and waits for
// a human action (ADR 0004: DriveConflict and WinFspRequired pause retrying;
// NeedsReauth waits for re-authentication; Failed waits for diagnosis).
func (s State) Paused() bool {
	switch s {
	case StateNeedsReauth, StateDriveConflict, StateWinFspRequired, StateFailed:
		return true
	}
	return false
}

// Status is one snapshot of the mount lifecycle: the state plus the phase it
// was reached in, the last non-sensitive error, when it was reached and when
// the next retry fires (ADR 0005's status contract).
type Status struct {
	State     State     `json:"state"`
	Phase     string    `json:"phase"`
	Err       string    `json:"error,omitempty"`
	Since     time.Time `json:"since"`
	NextRetry time.Time `json:"next_retry,omitempty"`
}

// stateFile is the persisted status, written atomically on every transition
// when the supervisor was given a state path.
type stateFile struct {
	Status
	WrittenAt time.Time `json:"written_at"`
}

// persistStatus atomically replaces the state file. Failures are the
// caller's to log: a status write must never break the lifecycle it
// describes.
func persistStatus(path string, st Status) error {
	if path == "" {
		return nil
	}
	data, err := json.MarshalIndent(stateFile{Status: st, WrittenAt: time.Now()}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'), 0o644)
}

// writeFileAtomic replaces path with data via a temp file in the same
// directory and a rename, so a crash mid-write never leaves a truncated file.
// On Windows a concurrent reader briefly locks the target, so the rename
// retries through sharing violations instead of surfacing them.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
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
	for attempt := 0; ; attempt++ {
		err := os.Rename(tmpName, path)
		if err == nil {
			return nil
		}
		// ERROR_SHARING_VIOLATION: a reader holds the target open. A short
		// retry usually outlives the reader; if it does not, the ordered
		// direct write below keeps the file correct at the cost of the
		// rename's atomicity — the status file is diagnostic data, and a
		// lost rename must not wedge the lifecycle.
		if attempt < 20 && errors.Is(err, errSharingViolation) {
			time.Sleep(5 * time.Millisecond)
			continue
		}
		return os.WriteFile(path, data, mode)
	}
}

// errSharingViolation is the Windows errno a concurrent reader produces when
// the atomic rename tries to replace the file it has open. On other platforms
// the errno value is meaningless but also unreachable through os.Rename
// failure modes this helper cares about.
const errSharingViolation = syscall.Errno(32)
