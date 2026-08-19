package tray

import (
	"bytes"
	"encoding/binary"
	"image/png"
	"testing"
)

func TestPngIconIsTheDrawnMark(t *testing.T) {
	img, err := png.Decode(bytes.NewReader(pngIcon()))
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != iconSize || b.Dy() != iconSize {
		t.Fatalf("icon is %dx%d, want %dx%d", b.Dx(), b.Dy(), iconSize, iconSize)
	}
	// The corner is transparent (the square is rounded), the stem is light on
	// a dark ground — the three tones the mark is made of.
	if _, _, _, a := img.At(0, 0).RGBA(); a != 0 {
		t.Error("the corner outside the rounded square is not transparent")
	}
	if r, _, _, a := img.At(32, 40).RGBA(); a == 0 || r < 0x8000 {
		t.Error("the stem is missing its light foreground")
	}
	if r, _, _, a := img.At(10, 50).RGBA(); a == 0 || r > 0x8000 {
		t.Error("the ground under the mark is not dark")
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
