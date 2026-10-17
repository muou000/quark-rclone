package trayui

import (
	"strings"
	"testing"
)

func TestEventLogRedactsAtTheBoundary(t *testing.T) {
	l := NewEventLog()
	l.Add("keepalive: cookie %s; url https://oss.example.com/obj?sign=abc123", "__puus=secret-value")

	entries := l.Entries()
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	text := entries[0].Text
	if strings.Contains(text, "secret-value") || strings.Contains(text, "abc123") {
		t.Fatalf("log entry leaked credential material: %q", text)
	}
	if !strings.Contains(text, "[redacted]") {
		t.Errorf("entry should carry the redaction placeholder: %q", text)
	}
	if entries[0].At.IsZero() {
		t.Error("entry has no timestamp")
	}
}

func TestEventLogCapsAndExports(t *testing.T) {
	l := NewEventLog()
	for i := 0; i < eventLogCap+10; i++ {
		l.Add("event-%d", i)
	}
	if got := len(l.Entries()); got != eventLogCap {
		t.Fatalf("entries = %d, want capped at %d", got, eventLogCap)
	}
	exported := l.Export()
	lines := strings.Split(strings.TrimRight(exported, "\n"), "\n")
	if len(lines) != eventLogCap {
		t.Fatalf("export lines = %d, want %d", len(lines), eventLogCap)
	}
	if !strings.Contains(lines[0], "event-10") || !strings.Contains(lines[len(lines)-1], "event-209") {
		t.Errorf("export window wrong: first %q last %q", lines[0], lines[len(lines)-1])
	}
	if !strings.Contains(lines[0], "\t") {
		t.Errorf("export lines should be timestamp-tab-text: %q", lines[0])
	}

	// A nil log tolerates Add from fire-and-forget callers.
	var nilLog *EventLog
	nilLog.Add("no panic")
	if nilLog.Entries() != nil {
		t.Error("nil log should stay empty")
	}
	if NewEventLog().Export() != "" {
		t.Error("empty export should be empty")
	}
}
