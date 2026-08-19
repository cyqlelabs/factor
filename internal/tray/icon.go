// Package tray shows a system-tray icon while the gateway runs: its presence
// is the status light, and its menu carries the one action a tray owes its
// user — quitting the daemon. Linux speaks StatusNotifierItem over the
// session D-Bus and Windows the win32 shell, both pure Go; macOS would need
// cgo and this binary stays CGO-free, so there the stub build applies and the
// gateway simply runs without an icon.
package tray

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"runtime"
)

// iconSize is generous for HiDPI trays; hosts scale it down.
const iconSize = 64

// icon returns the mark in the encoding this platform's tray wants: Windows
// reads ICO, everything else PNG.
func icon() []byte {
	if runtime.GOOS == "windows" {
		return icoWrap(pngIcon())
	}
	return pngIcon()
}

func pngIcon() []byte {
	var b bytes.Buffer
	_ = png.Encode(&b, drawIcon())
	return b.Bytes()
}

// drawIcon paints the mark — a lowercase f built from bars on a dark rounded
// square — in code, so the binary carries no asset and a test can decode
// exactly what the tray will show.
func drawIcon() *image.RGBA {
	bg := color.RGBA{R: 0x1a, G: 0x1b, B: 0x26, A: 0xff}
	fg := color.RGBA{R: 0xe8, G: 0xe6, B: 0xf2, A: 0xff}
	img := image.NewRGBA(image.Rect(0, 0, iconSize, iconSize))
	for y := 0; y < iconSize; y++ {
		for x := 0; x < iconSize; x++ {
			if inRoundedSquare(x, y) {
				img.SetRGBA(x, y, bg)
			}
		}
	}
	for _, bar := range []image.Rectangle{
		image.Rect(30, 10, 46, 18), // the hook, reaching right
		image.Rect(28, 10, 36, 52), // the stem
		image.Rect(20, 26, 44, 33), // the crossbar
	} {
		for y := bar.Min.Y; y < bar.Max.Y; y++ {
			for x := bar.Min.X; x < bar.Max.X; x++ {
				img.SetRGBA(x, y, fg)
			}
		}
	}
	return img
}

// inRoundedSquare reports whether the pixel lands on the icon's rounded
// background rather than in a transparent corner.
func inRoundedSquare(x, y int) bool {
	const radius = 14
	const max = iconSize - 1 - radius
	cx, cy := x, y
	if x < radius {
		cx = radius
	} else if x > max {
		cx = max
	}
	if y < radius {
		cy = radius
	} else if y > max {
		cy = max
	}
	dx, dy := x-cx, y-cy
	return dx*dx+dy*dy <= radius*radius
}

// icoWrap puts PNG bytes into a single-image ICO container, the format the
// Windows tray wants; PNG-compressed entries are valid there since Vista.
func icoWrap(pngData []byte) []byte {
	var b bytes.Buffer
	le := func(v any) { _ = binary.Write(&b, binary.LittleEndian, v) }
	le(uint16(0)) // reserved
	le(uint16(1)) // type: icon
	le(uint16(1)) // one image
	b.WriteByte(iconSize)
	b.WriteByte(iconSize)
	b.WriteByte(0) // no palette
	b.WriteByte(0) // reserved
	le(uint16(1))  // color planes
	le(uint16(32)) // bits per pixel
	le(uint32(len(pngData)))
	le(uint32(22)) // the image starts right after this directory
	b.Write(pngData)
	return b.Bytes()
}
