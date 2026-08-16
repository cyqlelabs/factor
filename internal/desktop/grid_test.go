package desktop

import (
	"fmt"
	"image"
	"strings"
	"testing"
)

func TestColumnLabel(t *testing.T) {
	cases := map[int]string{
		0: "A", 1: "B", 25: "Z", 26: "AA", 51: "AZ", 52: "BA", 701: "ZZ", 702: "AAA",
	}
	for index, want := range cases {
		if got := columnLabel(index); got != want {
			t.Errorf("columnLabel(%d) = %q, want %q", index, got, want)
		}
	}
}

func TestParseCellRoundTrip(t *testing.T) {
	for _, col := range []int{0, 1, 25, 26, 700, 702} {
		for _, row := range []int{0, 4, 99} {
			coord := fmt.Sprintf("%s%d", columnLabel(col), row+1)
			gotCol, gotRow, err := parseCell(coord)
			if err != nil {
				t.Fatalf("parseCell(%q): %v", coord, err)
			}
			if gotCol != col || gotRow != row {
				t.Errorf("parseCell(%q) = (%d,%d), want (%d,%d)", coord, gotCol, gotRow, col, row)
			}
		}
	}
	if col, row, err := parseCell("  d4 "); err != nil || col != 3 || row != 3 {
		t.Errorf("lowercase with spaces: (%d,%d,%v)", col, row, err)
	}
}

func TestParseCellRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"", "4", "D", "4D", "D0", "D 4", "D4X", "-D4"} {
		if _, _, err := parseCell(bad); err == nil {
			t.Errorf("parseCell(%q) accepted, want error", bad)
		}
	}
}

func TestGridCellSize(t *testing.T) {
	if got := gridCellSize(1568, 880, 5); got != 176 {
		t.Errorf("cell size = %d, want 176", got)
	}
	if got := gridCellSize(200, 150, 5); got != minCellPx {
		t.Errorf("small image cell size = %d, want floor %d", got, minCellPx)
	}
}

func TestLayoutGeometry(t *testing.T) {
	g := layoutFor(640, 480, 5) // cell 96
	if g.CellSize != 96 || g.Cols != 7 || g.Rows != 5 {
		t.Fatalf("layout = %+v", g)
	}
	if got := g.lastCell(); got != "G5" {
		t.Errorf("lastCell = %q, want G5", got)
	}
	// Interior cell.
	if r := g.cellRect(1, 1); r != image.Rect(96, 96, 192, 192) {
		t.Errorf("cellRect(1,1) = %v", r)
	}
	// Edge cells clamp to the image.
	if r := g.cellRect(6, 4); r != image.Rect(576, 384, 640, 480) {
		t.Errorf("edge cellRect = %v", r)
	}
	if _, err := g.cellRectByName("G5"); err != nil {
		t.Errorf("G5 should be valid: %v", err)
	}
	if _, err := g.cellRectByName("H1"); err == nil || !strings.Contains(err.Error(), "outside the grid") {
		t.Errorf("H1 should be out of range, got %v", err)
	}
	if _, err := g.cellRectByName("A6"); err == nil {
		t.Error("A6 should be out of range")
	}
	if _, err := g.cellRectByName("!!"); err == nil {
		t.Error("garbage cell should error")
	}
}

func TestDrawGridPixels(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 300, 200))
	g := layoutFor(300, 200, 5) // cell floors to 60
	drawGrid(img, g)

	// The vertical line at x=60: bright core flanked by dark edges, sampled
	// away from any label.
	if got := img.RGBAAt(60, 5); got != gridLineInner {
		t.Errorf("inner line pixel = %v, want %v", got, gridLineInner)
	}
	if got := img.RGBAAt(59, 5); got != gridLineOuter {
		t.Errorf("outer line pixel = %v, want %v", got, gridLineOuter)
	}
	// Horizontal line at y=60.
	if got := img.RGBAAt(5, 60); got != gridLineInner {
		t.Errorf("horizontal inner pixel = %v", got)
	}

	// Each cell center area carries an outlined label: white ink and black
	// outline pixels both present near the center of B2.
	r := g.cellRect(1, 1)
	cx, cy := (r.Min.X+r.Max.X)/2, (r.Min.Y+r.Max.Y)/2
	var ink, outline bool
	for dy := -12; dy <= 12; dy++ {
		for dx := -12; dx <= 12; dx++ {
			switch img.RGBAAt(cx+dx, cy+dy) {
			case labelInk:
				ink = true
			case labelOutline:
				outline = true
			}
		}
	}
	if !ink || !outline {
		t.Errorf("label at B2 center: ink=%v outline=%v, want both", ink, outline)
	}
}

func TestFitForModel(t *testing.T) {
	small := image.NewRGBA(image.Rect(0, 0, 800, 600))
	out, scale := fitForModel(small)
	if scale != 1 || out.Bounds().Dx() != 800 || out.Bounds().Dy() != 600 {
		t.Errorf("small image should be untouched: %v scale %v", out.Bounds(), scale)
	}
	if out == small {
		t.Error("fitForModel must copy — the grid is drawn on the result, the raw frame must stay clean")
	}

	big := image.NewRGBA(image.Rect(0, 0, 3200, 1800))
	out, scale = fitForModel(big)
	if out.Bounds().Dx() != maxViewDim {
		t.Errorf("longest side = %d, want %d", out.Bounds().Dx(), maxViewDim)
	}
	if want := float64(maxViewDim) / 3200; scale != want {
		t.Errorf("scale = %v, want %v", scale, want)
	}
	if got := out.Bounds().Dy(); got != int(1800*scale) {
		t.Errorf("height = %d, want %d", got, int(1800*scale))
	}
}

func TestBuildZoomMapsBackToNativePixels(t *testing.T) {
	raw := image.NewRGBA(image.Rect(0, 0, 1000, 800))
	zoomed, z, err := buildZoom(raw, image.Rect(100, 100, 300, 260))
	if err != nil {
		t.Fatal(err)
	}
	if z.Scale != 2 {
		t.Errorf("scale = %v, want 2", z.Scale)
	}
	if zoomed.Bounds().Dx() != 400 || zoomed.Bounds().Dy() != 320 {
		t.Errorf("zoomed size = %v", zoomed.Bounds())
	}
	// divisions 8 on a 400x320 crop floors to the 60px minimum.
	if z.Grid.CellSize != 60 || z.Grid.Cols != 7 || z.Grid.Rows != 6 {
		t.Fatalf("zoom grid = %+v", z.Grid)
	}
	// A1's center on the zoomed image is (30,30); back to native that is
	// (30/2+100, 30/2+100).
	p, err := z.cellCenterNative("A1")
	if err != nil {
		t.Fatal(err)
	}
	if p.X != 115 || p.Y != 115 {
		t.Errorf("A1 native center = %v, want (115,115)", p)
	}
	if _, err := z.cellCenterNative("Z9"); err == nil {
		t.Error("out-of-range zoom cell should error")
	}
}

func TestBuildZoomClampsAndCaps(t *testing.T) {
	raw := image.NewRGBA(image.Rect(0, 0, 1000, 800))
	// Region reaching past the frame is clamped, not rejected.
	_, z, err := buildZoom(raw, image.Rect(900, 700, 1200, 1000))
	if err != nil {
		t.Fatal(err)
	}
	if z.Crop != image.Rect(900, 700, 1000, 800) {
		t.Errorf("crop = %v", z.Crop)
	}
	// Fully outside is an error.
	if _, _, err := buildZoom(raw, image.Rect(2000, 2000, 2100, 2100)); err == nil {
		t.Error("out-of-frame zoom should error")
	}
	// A huge region cannot exceed maxViewDim after upscale.
	zoomed, z, err := buildZoom(raw, image.Rect(0, 0, 1000, 800))
	if err != nil {
		t.Fatal(err)
	}
	if got := zoomed.Bounds().Dx(); got > maxViewDim {
		t.Errorf("zoomed width = %d exceeds cap", got)
	}
	if want := float64(maxViewDim) / 1000; z.Scale != want {
		t.Errorf("capped scale = %v, want %v", z.Scale, want)
	}
}

func TestToRGBA(t *testing.T) {
	rgba := image.NewRGBA(image.Rect(0, 0, 4, 4))
	if toRGBA(rgba) != rgba {
		t.Error("RGBA input should pass through")
	}
	gray := image.NewGray(image.Rect(2, 2, 6, 6))
	out := toRGBA(gray)
	if out.Bounds() != image.Rect(0, 0, 4, 4) {
		t.Errorf("converted bounds = %v, want origin-normalized 4x4", out.Bounds())
	}
}
