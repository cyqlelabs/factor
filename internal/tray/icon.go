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

	"golang.org/x/image/vector"
)

// iconSize is generous for HiDPI trays; hosts scale it down.
const iconSize = 64

// artboard is the side of the brand mark's coordinate space (factor-mark.svg
// is a 512-unit square), so the shapes below carry the brand file's own
// numbers and scale converts them to icon pixels.
const artboard = 512.0
const scale = iconSize / artboard

// The plate's diagonal gradient, from the brand's light blue to its deep one.
var (
	plateFrom = color.RGBA{R: 0x0e, G: 0xa5, B: 0xe9, A: 0xff}
	plateTo   = color.RGBA{R: 0x03, G: 0x69, B: 0xa1, A: 0xff}
)

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

// drawIcon paints the Factor mark — the white glyph on its round blue plate —
// straight from the brand artwork's geometry, so the binary carries no asset
// and a test can decode exactly what the tray will show.
func drawIcon() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, iconSize, iconSize))

	plate := vector.NewRasterizer(iconSize, iconSize)
	addCircle(plate, at(256), at(256), at(256))
	plate.Draw(img, img.Bounds(), plateGradient(), image.Point{})

	// The glyph sits inset on the plate: translate(12.8) scale(0.95) in the
	// artwork, which glyphAt applies.
	glyph := vector.NewRasterizer(iconSize, iconSize)
	cx, cy := glyphAt(256, 103)
	addCircle(glyph, cx, cy, at(0.95*38.25))
	addPolygon(glyph, 150.25, 172.75, 361.75, 172.75, 361.75, 202, 323.5, 240.25, 150.25, 240.25)
	addPolygon(glyph, 150.25, 271.75, 361.75, 271.75, 361.75, 301, 323.5, 339.25,
		214.75, 339.25, 214.75, 447.25, 172, 447.25, 150.25, 425.5)
	glyph.Draw(img, img.Bounds(), image.NewUniform(color.White), image.Point{})

	return img
}

// at converts a length on the brand artboard to icon pixels.
func at(v float64) float32 { return float32(v * scale) }

// glyphAt converts a point on the artboard through the glyph's inset
// transform into icon pixels.
func glyphAt(x, y float64) (float32, float32) {
	return at(12.8 + 0.95*x), at(12.8 + 0.95*y)
}

// addPolygon traces one of the glyph's bars, given its artboard vertices as
// x,y pairs.
func addPolygon(z *vector.Rasterizer, pts ...float64) {
	for i := 0; i < len(pts); i += 2 {
		x, y := glyphAt(pts[i], pts[i+1])
		if i == 0 {
			z.MoveTo(x, y)
			continue
		}
		z.LineTo(x, y)
	}
	z.ClosePath()
}

// addCircle traces a circle as four cubic Béziers; kappa is the control-point
// reach that makes one match a quarter arc.
func addCircle(z *vector.Rasterizer, cx, cy, r float32) {
	const kappa = 0.5522847
	k := r * kappa
	z.MoveTo(cx+r, cy)
	z.CubeTo(cx+r, cy+k, cx+k, cy+r, cx, cy+r)
	z.CubeTo(cx-k, cy+r, cx-r, cy+k, cx-r, cy)
	z.CubeTo(cx-r, cy-k, cx-k, cy-r, cx, cy-r)
	z.CubeTo(cx+k, cy-r, cx+r, cy-k, cx+r, cy)
	z.ClosePath()
}

// plateGradient paints the plate's fill: the brand's linear gradient down the
// diagonal, which the plate's own coverage mask then clips to the circle.
func plateGradient() *image.RGBA {
	g := image.NewRGBA(image.Rect(0, 0, iconSize, iconSize))
	for y := 0; y < iconSize; y++ {
		for x := 0; x < iconSize; x++ {
			t := float64(x+y) / float64(2*(iconSize-1))
			g.SetRGBA(x, y, color.RGBA{
				R: mix(plateFrom.R, plateTo.R, t),
				G: mix(plateFrom.G, plateTo.G, t),
				B: mix(plateFrom.B, plateTo.B, t),
				A: 0xff,
			})
		}
	}
	return g
}

// mix blends two channel values, t running 0 at a to 1 at b.
func mix(a, b uint8, t float64) uint8 {
	return uint8(float64(a) + (float64(b)-float64(a))*t + 0.5)
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
