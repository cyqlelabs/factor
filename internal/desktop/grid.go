package desktop

// Battleship-style coordinate grid for screenshots, built for a single
// static binary: no OpenCV, no OCR, no segmentation — pure Go image math the
// model refines itself with a zoom pass. The grid gives a vision model a
// coarse spatial vocabulary ("the icon is in D4"); zooming a cell overlays a
// finer sub-grid whose cells map back to native pixels, so two rounds take
// pointing precision from ~cell-size down to ~10px.

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"strings"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
)

const (
	// gridDivisions divides the shortest image side on the first pass;
	// zoomDivisions rules the finer sub-grid on a zoomed crop.
	gridDivisions = 5
	zoomDivisions = 8
	// minCellPx floors the cell size so tiny captures don't produce a grid
	// denser than a model can point into.
	minCellPx = 60
	// maxViewDim caps the longest side of any image sent to the model:
	// beyond ~1.5k px vision models gain no accuracy while every extra pixel
	// costs tokens and upload time — exactly the budget a low-resource box
	// cares about.
	maxViewDim = 1568
	// zoomFactor upscales a zoomed crop so small UI text becomes legible.
	zoomFactor = 2
)

var (
	gridLineOuter = color.RGBA{0, 0, 0, 255}
	gridLineInner = color.RGBA{0, 255, 255, 255} // cyan, same as the platform overlay
	labelInk      = color.RGBA{255, 255, 255, 255}
	labelOutline  = color.RGBA{0, 0, 0, 255}
)

// columnLabel converts 0→A, 25→Z, 26→AA, like spreadsheet columns.
func columnLabel(index int) string {
	label := ""
	index++
	for index > 0 {
		index--
		label = string(rune('A'+index%26)) + label
		index /= 26
	}
	return label
}

// parseCell converts "D4" or "ab12" to zero-indexed (col, row).
func parseCell(coord string) (col, row int, err error) {
	var colStr, rowStr strings.Builder
	for _, r := range strings.ToUpper(strings.TrimSpace(coord)) {
		switch {
		case r >= 'A' && r <= 'Z' && rowStr.Len() == 0:
			colStr.WriteRune(r)
		case r >= '0' && r <= '9' && colStr.Len() > 0:
			rowStr.WriteRune(r)
		default:
			return 0, 0, fmt.Errorf("invalid grid cell %q (expected a letter-number pair like B3)", coord)
		}
	}
	if colStr.Len() == 0 || rowStr.Len() == 0 {
		return 0, 0, fmt.Errorf("invalid grid cell %q (expected a letter-number pair like B3)", coord)
	}
	for _, r := range colStr.String() {
		col = col*26 + int(r-'A'+1)
	}
	col--
	for _, r := range rowStr.String() {
		row = row*10 + int(r-'0')
	}
	row--
	if row < 0 {
		return 0, 0, fmt.Errorf("invalid grid cell %q (rows start at 1)", coord)
	}
	return col, row, nil
}

// gridCellSize derives the cell size from the shortest image dimension,
// floored at minCellPx.
func gridCellSize(w, h, divisions int) int {
	shortest := min(w, h)
	if divisions < 1 {
		divisions = 1
	}
	return max(minCellPx, shortest/divisions)
}

// gridLayout describes a grid laid over a W×H image.
type gridLayout struct {
	CellSize   int
	Cols, Rows int
	W, H       int
}

func layoutFor(w, h, divisions int) gridLayout {
	size := gridCellSize(w, h, divisions)
	return gridLayout{
		CellSize: size,
		Cols:     (w + size - 1) / size,
		Rows:     (h + size - 1) / size,
		W:        w,
		H:        h,
	}
}

// cellRect returns the cell's pixel bounds, clamped to the image at the
// right/bottom edges where cells may be partial.
func (g gridLayout) cellRect(col, row int) image.Rectangle {
	x0, y0 := col*g.CellSize, row*g.CellSize
	return image.Rect(x0, y0, min(x0+g.CellSize, g.W), min(y0+g.CellSize, g.H))
}

// cellRectByName resolves a coordinate like "D4" to its bounds, rejecting
// cells outside the grid so a hallucinated coordinate fails loudly instead of
// clamping to an unrelated edge cell.
func (g gridLayout) cellRectByName(coord string) (image.Rectangle, error) {
	col, row, err := parseCell(coord)
	if err != nil {
		return image.Rectangle{}, err
	}
	if col >= g.Cols || row >= g.Rows {
		return image.Rectangle{}, fmt.Errorf("cell %s is outside the grid (columns A-%s, rows 1-%d)",
			strings.ToUpper(strings.TrimSpace(coord)), columnLabel(g.Cols-1), g.Rows)
	}
	return g.cellRect(col, row), nil
}

// lastCell names the bottom-right cell, e.g. "H5" — used in tool output so
// the model knows the grid's extent.
func (g gridLayout) lastCell() string {
	return fmt.Sprintf("%s%d", columnLabel(g.Cols-1), g.Rows)
}

// drawGrid overlays the battleship grid: dark outer lines with a bright core
// so they stay visible on any background, and an outlined coordinate label
// centered in each cell.
func drawGrid(img *image.RGBA, g gridLayout) {
	base := max(1, g.CellSize/50)
	for x := 0; x < g.W; x += g.CellSize {
		fillRect(img, image.Rect(x-(base+1)/2, 0, x+(base+1)/2+1, g.H), gridLineOuter)
		fillRect(img, image.Rect(x-base/2, 0, x+(base+1)/2, g.H), gridLineInner)
	}
	for y := 0; y < g.H; y += g.CellSize {
		fillRect(img, image.Rect(0, y-(base+1)/2, g.W, y+(base+1)/2+1), gridLineOuter)
		fillRect(img, image.Rect(0, y-base/2, g.W, y+(base+1)/2), gridLineInner)
	}

	scale := 1
	if g.CellSize >= 140 {
		scale = 2
	}
	for row := 0; row < g.Rows; row++ {
		for col := 0; col < g.Cols; col++ {
			r := g.cellRect(col, row)
			cx, cy := (r.Min.X+r.Max.X)/2, (r.Min.Y+r.Max.Y)/2
			drawLabel(img, fmt.Sprintf("%s%d", columnLabel(col), row+1), cx, cy, scale)
		}
	}
}

func fillRect(img *image.RGBA, r image.Rectangle, c color.RGBA) {
	draw.Draw(img, r.Intersect(img.Bounds()), image.NewUniform(c), image.Point{}, draw.Src)
}

// drawLabel renders outlined text centered at (cx, cy). The glyphs come from
// the built-in bitmap face (no font files, no CGO) drawn at 1:1 with a 1px
// outline, then integer-upscaled — the outline scales with the text and the
// result stays crisp.
func drawLabel(dst *image.RGBA, text string, cx, cy, scale int) {
	face := basicfont.Face7x13
	tw := font.MeasureString(face, text).Ceil()
	th := face.Height
	pad := 2 // room for the outline
	tmp := image.NewRGBA(image.Rect(0, 0, tw+2*pad, th+2*pad))

	drawText := func(x, y int, c color.RGBA) {
		(&font.Drawer{
			Dst:  tmp,
			Src:  image.NewUniform(c),
			Face: face,
			Dot:  fixed.P(x, y+face.Ascent),
		}).DrawString(text)
	}
	for dy := -1; dy <= 1; dy++ {
		for dx := -1; dx <= 1; dx++ {
			if dx != 0 || dy != 0 {
				drawText(pad+dx, pad+dy, labelOutline)
			}
		}
	}
	drawText(pad, pad, labelInk)

	w, h := tmp.Bounds().Dx()*scale, tmp.Bounds().Dy()*scale
	target := image.Rect(cx-w/2, cy-h/2, cx-w/2+w, cy-h/2+h)
	xdraw.NearestNeighbor.Scale(dst, target, tmp, tmp.Bounds(), xdraw.Over, nil)
}

// toRGBA returns src as *image.RGBA without copying when it already is one.
func toRGBA(src image.Image) *image.RGBA {
	if rgba, ok := src.(*image.RGBA); ok {
		return rgba
	}
	b := src.Bounds()
	out := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(out, out.Bounds(), src, b.Min, draw.Src)
	return out
}

// fitForModel returns a copy of src scaled so its longest side is at most
// maxViewDim, plus the applied scale (view px per native px, ≤ 1).
func fitForModel(src *image.RGBA) (*image.RGBA, float64) {
	w, h := src.Bounds().Dx(), src.Bounds().Dy()
	longest := max(w, h)
	if longest <= maxViewDim {
		out := image.NewRGBA(src.Bounds())
		draw.Draw(out, out.Bounds(), src, src.Bounds().Min, draw.Src)
		return out, 1
	}
	scale := float64(maxViewDim) / float64(longest)
	out := image.NewRGBA(image.Rect(0, 0, max(1, int(float64(w)*scale)), max(1, int(float64(h)*scale))))
	xdraw.ApproxBiLinear.Scale(out, out.Bounds(), src, src.Bounds(), xdraw.Src, nil)
	return out, scale
}

// zoomView records how a zoomed crop maps back to native screen pixels.
type zoomView struct {
	Crop  image.Rectangle // native pixels on the raw frame
	Scale float64         // zoomed px per native px
	Grid  gridLayout      // laid over the zoomed image
}

// cellCenterNative maps a sub-grid cell on the zoomed image to the native
// pixel at its center.
func (z zoomView) cellCenterNative(coord string) (image.Point, error) {
	r, err := z.Grid.cellRectByName(coord)
	if err != nil {
		return image.Point{}, err
	}
	cx := float64(r.Min.X+r.Max.X) / 2
	cy := float64(r.Min.Y+r.Max.Y) / 2
	return image.Point{
		X: z.Crop.Min.X + int(cx/z.Scale),
		Y: z.Crop.Min.Y + int(cy/z.Scale),
	}, nil
}

// buildZoom crops rect (native px) out of raw, upscales it by zoomFactor
// (bounded by maxViewDim), and overlays a finer grid. Returns the annotated
// image and the mapping back to native pixels.
func buildZoom(raw *image.RGBA, rect image.Rectangle) (*image.RGBA, zoomView, error) {
	rect = rect.Intersect(raw.Bounds())
	if rect.Dx() < 1 || rect.Dy() < 1 {
		return nil, zoomView{}, fmt.Errorf("zoom region is outside the screen")
	}
	scale := float64(zoomFactor)
	if longest := max(rect.Dx(), rect.Dy()); longest*zoomFactor > maxViewDim {
		scale = max(1, float64(maxViewDim)/float64(longest))
	}
	out := image.NewRGBA(image.Rect(0, 0,
		max(1, int(float64(rect.Dx())*scale)), max(1, int(float64(rect.Dy())*scale))))
	xdraw.ApproxBiLinear.Scale(out, out.Bounds(), raw, rect, xdraw.Src, nil)

	grid := layoutFor(out.Bounds().Dx(), out.Bounds().Dy(), zoomDivisions)
	drawGrid(out, grid)
	return out, zoomView{Crop: rect, Scale: scale, Grid: grid}, nil
}
