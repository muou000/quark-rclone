package trayui

import (
	"strings"
	"testing"

	"github.com/muou000/quark-rclone/internal/quarkerr"
	"github.com/muou000/quark-rclone/internal/supervisor"
)

// states is the closed set of ADR 0004; the label tests iterate it so a new
// state without a Chinese label fails loudly.
var states = []supervisor.State{
	supervisor.StateDisabled, supervisor.StateStarting,
	supervisor.StateWaitingForNetwork, supervisor.StateRecovering,
	supervisor.StateMounted, supervisor.StateNeedsReauth,
	supervisor.StateDriveConflict, supervisor.StateWinFspRequired,
	supervisor.StateFailed, supervisor.StateStopping,
}

// categories is the closed set of quarkerr.
var categories = []quarkerr.Category{
	quarkerr.CatAuthRequired, quarkerr.CatPermissionDenied,
	quarkerr.CatObjectNotFound, quarkerr.CatParentNotFound,
	quarkerr.CatTargetConflict, quarkerr.CatLocked,
	quarkerr.CatNotSupported, quarkerr.CatUpstreamTransient,
	quarkerr.CatRateLimited, quarkerr.CatQuotaExceeded,
	quarkerr.CatInvalidRequest, quarkerr.CatInternal,
	quarkerr.CatCancelled, quarkerr.CatUnknown,
}

func TestEveryStateHasChineseLabelAndRemedy(t *testing.T) {
	for _, s := range states {
		label := StateLabel(s)
		if label == "" || label == string(s) {
			t.Errorf("StateLabel(%s) = %q, want a Chinese label", s, label)
		}
		if remedy := StateRemedy(s); remedy == "" {
			t.Errorf("StateRemedy(%s) is empty; every state needs an actionable hint", s)
		}
	}
}

func TestEveryCategoryHasChineseLabel(t *testing.T) {
	for _, c := range categories {
		if label := CategoryLabel(c); label == "" || label == string(c) {
			t.Errorf("CategoryLabel(%s) = %q, want a Chinese label", c, label)
		}
	}
}

func TestPermanenceDrivesTheDisplay(t *testing.T) {
	// AC 5: a transient outage must never be shown as a permanent condition.
	if CategoryIsPermanent(quarkerr.CatUpstreamTransient) {
		t.Error("upstream_transient must not read as permanent")
	}
	if CategoryIsPermanent(quarkerr.CatRateLimited) {
		t.Error("rate_limited must not read as permanent")
	}
	if !CategoryIsPermanent(quarkerr.CatNotSupported) {
		t.Error("operation_not_supported must read as permanent")
	}
	if !CategoryIsPermanent(quarkerr.CatAuthRequired) {
		t.Error("auth_required must read as permanent")
	}
}

func TestPausedStatesArePermanentForTheUser(t *testing.T) {
	for _, s := range states {
		want := s == supervisor.StateNeedsReauth ||
			s == supervisor.StateDriveConflict ||
			s == supervisor.StateWinFspRequired ||
			s == supervisor.StateFailed
		if got := s.Paused(); got != want {
			t.Errorf("%s.Paused() = %t, want %t", s, got, want)
		}
	}
}

func TestUnsupportedNoticeNamesBothGaps(t *testing.T) {
	for _, want := range []string{"复制", "覆盖上传", "目标已存在"} {
		if !strings.Contains(UnsupportedNotice, want) {
			t.Errorf("unsupported notice missing %q: %s", want, UnsupportedNotice)
		}
	}
}
