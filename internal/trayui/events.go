package trayui

import (
	"fmt"
	"sync"
	"time"

	"github.com/muou000/quark-rclone/internal/redact"
)

// eventLogCap bounds the in-memory diagnostic log. The exported file is the
// bounded tail, so a long-lived tray never grows without limit.
const eventLogCap = 200

// LogEntry is one diagnostic event: a timestamp and text that was already
// redacted when it was added. Nothing sensitive may reach a LogEntry — the
// export is meant to be pasted into a bug report (issue #24, ADR 0010).
type LogEntry struct {
	At   time.Time `json:"at"`
	Text string    `json:"text"`
}

// EventLog is the thread-safe, bounded, redacted diagnostic log behind the
// panel's log view and the export file.
type EventLog struct {
	mu      sync.Mutex
	entries []LogEntry
}

// NewEventLog returns an empty log.
func NewEventLog() *EventLog { return &EventLog{} }

// Add appends one event. The rendered text is redacted here, at the
// boundary, so a caller cannot forget: a cookie or a signed URL in a log line
// is redacted before it is stored, not before it is shown.
func (l *EventLog) Add(format string, args ...any) {
	if l == nil {
		return
	}
	line := redact.Text(fmt.Sprintf(format, args...))
	l.mu.Lock()
	l.entries = append(l.entries, LogEntry{At: time.Now(), Text: line})
	if len(l.entries) > eventLogCap {
		l.entries = l.entries[len(l.entries)-eventLogCap:]
	}
	l.mu.Unlock()
}

// Entries returns a copy of the log, oldest first.
func (l *EventLog) Entries() []LogEntry {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]LogEntry(nil), l.entries...)
}

// Export renders the log as the plain-text file the user downloads from the
// panel. One event per line, timestamped, oldest first.
func (l *EventLog) Export() string {
	entries := l.Entries()
	if len(entries) == 0 {
		return ""
	}
	out := make([]byte, 0, 64*len(entries))
	for _, e := range entries {
		out = fmt.Appendf(out, "%s\t%s\n", e.At.Format("2006-01-02 15:04:05"), e.Text)
	}
	return string(out)
}
