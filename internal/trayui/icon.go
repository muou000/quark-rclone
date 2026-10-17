package trayui

import (
	"image/color"

	"github.com/muou000/quark-rclone/internal/supervisor"
)

// iconSize is the tray icon's square edge in pixels.
const iconSize = 32

// Icon renders one 32×32 32-bpp ICO filled with a single colour. The tray
// recolours its icon by state — green mounted, amber working, red paused —
// without shipping binary assets (issue #24, ADR 0012).
//
// The encoding is the classic BMP-in-ICO form: an ICONDIR with one entry, a
// BITMAPINFOHEADER whose height counts both the XOR (colour) and the AND
// (mask) planes, bottom-up BGRA pixels, then an all-zero AND mask so the
// square is opaque.
func Icon(c color.RGBA) []byte {
	pixelBytes := iconSize * iconSize * 4
	maskRowBytes := iconSize / 8 // one bit per pixel, rows aligned to 4 bytes
	maskBytes := maskRowBytes * iconSize
	imageBytes := 40 + pixelBytes + maskBytes

	out := make([]byte, 0, 6+16+imageBytes)
	// ICONDIR: reserved, type=icon, one image.
	out = append(out, 0, 0, 1, 0, 1, 0)
	// ICONDIRENTRY: 32×32, no palette, 32 bpp.
	out = append(out, iconSize, iconSize, 0, 0)
	out = append(out, 1, 0, 32, 0)
	out = appendLE32(out, uint32(imageBytes))
	out = appendLE32(out, 22) // data offset: past the two headers
	// BITMAPINFOHEADER. The height is doubled: XOR plane plus AND plane.
	out = appendLE32(out, 40)
	out = appendLE32(out, iconSize)
	out = appendLE32(out, iconSize*2)
	out = append(out, 1, 0, 32, 0) // planes, bpp
	out = appendLE32(out, 0)       // BI_RGB
	out = appendLE32(out, uint32(pixelBytes+maskBytes))
	out = appendLE32(out, 0) // x pixels per metre
	out = appendLE32(out, 0) // y pixels per metre
	out = appendLE32(out, 0) // colours used
	out = appendLE32(out, 0) // important colours
	// Pixel rows bottom-up, BGRA.
	for row := iconSize - 1; row >= 0; row-- {
		for col := 0; col < iconSize; col++ {
			// A subtle two-tone checker keeps the square from reading as a
			// solid bar on dark taskbars while staying one recognisable hue.
			shade := 0
			if (row/4+col/4)%2 == 0 {
				shade = 24
			}
			out = append(out, clamp8(int(c.B)-shade), clamp8(int(c.G)-shade), clamp8(int(c.R)-shade), c.A)
		}
	}
	// AND mask: all zero, i.e. every pixel opaque.
	out = append(out, make([]byte, maskBytes)...)
	return out
}

func clamp8(v int) byte {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return byte(v)
}

func appendLE32(b []byte, v uint32) []byte {
	return append(b, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
}

// Tray colour palette by lifecycle family: mounted is calm, work is amber,
// a pause that needs the user is red.
var (
	IconMounted  = color.RGBA{R: 26, G: 127, B: 55, A: 255}
	IconWorking  = color.RGBA{R: 191, G: 144, B: 0, A: 255}
	IconPaused   = color.RGBA{R: 192, G: 57, B: 43, A: 255}
	IconInactive = color.RGBA{R: 107, G: 114, B: 128, A: 255}
)

// IconForState maps a lifecycle state onto the icon family.
func IconForState(s stateKind) color.RGBA {
	switch s {
	case kindMounted:
		return IconMounted
	case kindWorking:
		return IconWorking
	case kindPaused:
		return IconPaused
	}
	return IconInactive
}

// stateKind classifies the ten supervisor states into the tray's icon
// families. It exists so the shell passes one value, not a switch.
type stateKind int

const (
	kindInactive stateKind = iota
	kindWorking
	kindMounted
	kindPaused
)

// KindOf classifies a supervisor state for the tray renderer. The switch keys
// on the State constants, so renaming a state breaks the build instead of
// silently greying the icon.
func KindOf(s supervisor.State) stateKind {
	switch s {
	case supervisor.StateMounted:
		return kindMounted
	case supervisor.StateStarting, supervisor.StateStopping,
		supervisor.StateWaitingForNetwork, supervisor.StateRecovering:
		return kindWorking
	case supervisor.StateNeedsReauth, supervisor.StateDriveConflict,
		supervisor.StateWinFspRequired, supervisor.StateFailed:
		return kindPaused
	}
	return kindInactive
}
