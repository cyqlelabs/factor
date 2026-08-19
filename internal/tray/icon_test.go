package tray

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/png"
	"testing"
)

func TestPngIconIsTheFactorMark(t *testing.T) {
	img, err := png.Decode(bytes.NewReader(pngIcon()))
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != iconSize || b.Dy() != iconSize {
		t.Fatalf("icon is %dx%d, want %dx%d", b.Dx(), b.Dy(), iconSize, iconSize)
	}
	if _, _, _, a := img.At(0, 0).RGBA(); a != 0 {
		t.Error("the corner outside the round plate is not transparent")
	}
	// The glyph is white: its dot near the top, and the upper bar below it.
	for _, p := range []image.Point{{X: 32, Y: 14}, {X: 30, Y: 26}} {
		r, g, b, a := img.At(p.X, p.Y).RGBA()
		if a == 0 || r < 0xf000 || g < 0xf000 || b < 0xf000 {
			t.Errorf("the glyph at %v is not white: %04x%04x%04x", p, r, g, b)
		}
	}
	// The plate shows between the dot and the bar, and beside the glyph.
	for _, p := range []image.Point{{X: 32, Y: 20}, {X: 8, Y: 32}} {
		r, _, b, a := img.At(p.X, p.Y).RGBA()
		if a == 0 || b <= r {
			t.Errorf("the plate at %v is not blue: %04x..%04x", p, r, b)
		}
	}
	// And it deepens down the diagonal, the way the brand gradient runs.
	nearR, _, nearB, _ := img.At(16, 16).RGBA()
	farR, _, farB, _ := img.At(48, 48).RGBA()
	if nearB <= farB || nearR <= farR {
		t.Errorf("the plate gradient does not deepen: %04x/%04x to %04x/%04x", nearR, nearB, farR, farB)
	}
}

func TestIcoWrapBuildsAValidSingleImageIcon(t *testing.T) {
	payload := pngIcon()
	ico := icoWrap(payload)

	header := []struct {
		name string
		got  uint16
		want uint16
	}{
		{"reserved", binary.LittleEndian.Uint16(ico[0:2]), 0},
		{"type", binary.LittleEndian.Uint16(ico[2:4]), 1},
		{"count", binary.LittleEndian.Uint16(ico[4:6]), 1},
		{"planes", binary.LittleEndian.Uint16(ico[10:12]), 1},
		{"bpp", binary.LittleEndian.Uint16(ico[12:14]), 32},
	}
	for _, h := range header {
		if h.got != h.want {
			t.Errorf("%s = %d, want %d", h.name, h.got, h.want)
		}
	}
	if ico[6] != iconSize || ico[7] != iconSize {
		t.Errorf("directory size = %dx%d", ico[6], ico[7])
	}
	if size := binary.LittleEndian.Uint32(ico[14:18]); int(size) != len(payload) {
		t.Errorf("payload size = %d, want %d", size, len(payload))
	}
	if offset := binary.LittleEndian.Uint32(ico[18:22]); offset != 22 {
		t.Errorf("payload offset = %d, want 22", offset)
	}
	if !bytes.Equal(ico[22:], payload) {
		t.Error("the payload after the directory is not the PNG that went in")
	}
}
