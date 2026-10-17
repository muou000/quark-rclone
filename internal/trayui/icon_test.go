package trayui

import (
	"encoding/binary"
	"testing"

	"github.com/muou000/quark-rclone/internal/supervisor"
)

// TestIconEncoding checks the hand-built ICO: one 32×32 32-bpp image whose
// BITMAPINFOHEADER doubles the height for the AND mask, with the declared
// sizes consistent end to end.
func TestIconEncoding(t *testing.T) {
	ico := Icon(IconMounted)

	if got := binary.LittleEndian.Uint16(ico[2:4]); got != 1 {
		t.Fatalf("icon type = %d, want 1", got)
	}
	if got := binary.LittleEndian.Uint16(ico[4:6]); got != 1 {
		t.Fatalf("image count = %d, want 1", got)
	}
	if ico[6] != iconSize || ico[7] != iconSize {
		t.Fatalf("entry size = %dx%d, want 32x32", ico[6], ico[7])
	}
	// The 16-byte entry: width, height, colours, reserved, planes, bpp, size,
	// offset — all little-endian beyond the single bytes.
	if got := binary.LittleEndian.Uint16(ico[10:12]); got != 1 {
		t.Fatalf("planes = %d, want 1", got)
	}
	if got := binary.LittleEndian.Uint16(ico[12:14]); got != 32 {
		t.Fatalf("bpp = %d, want 32", got)
	}
	offset := binary.LittleEndian.Uint32(ico[18:22])
	size := binary.LittleEndian.Uint32(ico[14:18])
	if int(offset+size) != len(ico) {
		t.Fatalf("entry claims data at %d for %d bytes, but the file is %d", offset, size, len(ico))
	}
	// The BITMAPINFOHEADER starts at the data offset; its height is doubled
	// and the bpp rides two bytes behind the planes.
	if got := int32(binary.LittleEndian.Uint32(ico[offset+8 : offset+12])); got != iconSize*2 {
		t.Fatalf("bitmap height = %d, want %d (XOR + AND)", got, iconSize*2)
	}
	if got := binary.LittleEndian.Uint16(ico[offset+12 : offset+14]); got != 1 {
		t.Fatalf("bitmap planes = %d, want 1", got)
	}
	if got := binary.LittleEndian.Uint16(ico[offset+14 : offset+16]); got != 32 {
		t.Fatalf("bitmap bpp = %d, want 32", got)
	}
	// The AND mask is all zero: every pixel opaque.
	for _, b := range ico[len(ico)-iconSize*4:] {
		if b != 0 {
			t.Fatalf("AND mask not empty at the tail")
		}
	}
}

func TestIconColourFollowsState(t *testing.T) {
	green := Icon(IconMounted)
	red := Icon(IconPaused)
	if len(green) != len(red) {
		t.Fatalf("icon sizes differ: %d vs %d", len(green), len(red))
	}
	same := true
	for i := range green {
		if green[i] != red[i] {
			same = false
			break
		}
	}
	if same {
		t.Error("different colours produced identical icons")
	}
}

func TestKindOfCoversEveryState(t *testing.T) {
	// The tray's icon must be defined for all ten states.
	for _, s := range []supervisor.State{
		supervisor.StateDisabled, supervisor.StateStarting,
		supervisor.StateWaitingForNetwork, supervisor.StateRecovering,
		supervisor.StateMounted, supervisor.StateNeedsReauth,
		supervisor.StateDriveConflict, supervisor.StateWinFspRequired,
		supervisor.StateFailed, supervisor.StateStopping,
	} {
		if got := Icon(IconForState(KindOf(s))); len(got) == 0 {
			t.Errorf("no icon for state %s", s)
		}
	}
	if KindOf(supervisor.StateMounted) != kindMounted {
		t.Error("Mounted should use the mounted icon family")
	}
	if KindOf(supervisor.StateNeedsReauth) != kindPaused {
		t.Error("NeedsReauth should use the paused icon family")
	}
	if KindOf(supervisor.StateStarting) != kindWorking {
		t.Error("Starting should use the working icon family")
	}
}
