package desktop

// Drawing the tree onto the grid's picture: an outline around every listed
// control and a short tag naming its ref, so the one image the model looks at
// says both where a thing is and what the tree calls it. Yellow on black, a
// pair the grid (cyan lines, white labels) never draws, so the two layers
// stay readable on top of each other and on top of any desktop.

import (
	"fmt"
	"image"
	"image/color"
	"strings"

	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
)

var (
	tagFill    = color.RGBA{255, 212, 0, 255}
	tagInk     = color.RGBA{0, 0, 0, 255}
	tagOutline = color.RGBA{0, 0, 0, 255}
)

// overlayItem is one control to draw, in the image's own pixels.
type overlayItem struct {
	rect image.Rectangle
	tag  string
}

// drawElements outlines each control and tags it with its ref.
func drawElements(img *image.RGBA, items []overlayItem) {
	for _, it := range items {
		r := it.rect.Intersect(img.Bounds())
		if r.Dx() < 2 || r.Dy() < 2 {
			continue
		}
		strokeRect(img, r, 1, tagOutline)
		strokeRect(img, r.Inset(1), 1, tagFill)
	}
	// Tags after every outline, so no outline is drawn across a tag.
	for _, it := range items {
		r := it.rect.Intersect(img.Bounds())
		if r.Dx() < 2 || r.Dy() < 2 {
			continue
		}
		drawTag(img, it.tag, r)
	}
}

// strokeRect draws a rectangle's border, width px thick, inside r.
func strokeRect(img *image.RGBA, r image.Rectangle, width int, c color.RGBA) {
	if r.Empty() {
		return
	}
	fillRect(img, image.Rect(r.Min.X, r.Min.Y, r.Max.X, r.Min.Y+width), c)
	fillRect(img, image.Rect(r.Min.X, r.Max.Y-width, r.Max.X, r.Max.Y), c)
	fillRect(img, image.Rect(r.Min.X, r.Min.Y, r.Min.X+width, r.Max.Y), c)
	fillRect(img, image.Rect(r.Max.X-width, r.Min.Y, r.Max.X, r.Max.Y), c)
}

// drawTag puts a filled label at the control's top-left corner: just above it
// where there is room, so the tag does not sit on the control's own text, and
// inside it at the top edge of the screen.
func drawTag(img *image.RGBA, text string, r image.Rectangle) {
	face := basicfont.Face7x13
	w := font.MeasureString(face, text).Ceil() + 4
	h := face.Height + 2
	x, y := r.Min.X, r.Min.Y-h
	if y < img.Bounds().Min.Y {
		y = r.Min.Y
	}
	if x+w > img.Bounds().Max.X {
		x = img.Bounds().Max.X - w
	}
	box := image.Rect(x, y, x+w, y+h)
	fillRect(img, box, tagOutline)
	fillRect(img, box.Inset(1), tagFill)
	(&font.Drawer{
		Dst:  img,
		Src:  image.NewUniform(tagInk),
		Face: face,
		Dot:  fixed.P(x+2, y+1+face.Ascent),
	}).DrawString(text)
}

// treeSummary is what the model reads about the tree beside the picture: a
// verdict on how far to trust it, and one line per control with its ref, the
// grid cell it sits in, what it is and what state it is in.
func treeSummary(t *Tree, cellOf func(Element) string) string {
	if t == nil {
		return ""
	}
	var b strings.Builder
	where := t.Window
	if where == "" {
		where = "the window in front"
	} else {
		where = fmt.Sprintf("%q", where)
	}
	if t.App != "" {
		where += " (" + t.App + ")"
	}
	switch t.Quality() {
	case QualityNone:
		note := t.Note
		if note == "" {
			note = "it lists no controls — the application draws its interface itself"
		}
		fmt.Fprintf(&b, "\nNo accessibility tree for %s: %s. Point with the grid.", where, note)
		return b.String()
	case QualityThin:
		fmt.Fprintf(&b, "\nAccessibility tree of %s: thin — %d named of %d controls. This application draws most of its interface itself: "+
			"use the listed ones by element=, and point at anything else with the grid.", where, t.named(), len(t.Elements))
	default:
		fmt.Fprintf(&b, "\nAccessibility tree of %s: %d controls.", where, len(t.Elements))
	}
	if t.Positioned {
		fmt.Fprintf(&b, " Each is outlined and tagged in yellow on the image (frame f%d). ", t.Frame)
	} else {
		b.WriteString(" They are listed without positions — this session (Wayland) does not tell an application where its window is — so they are not tagged on the image, but element= still acts on them. ")
	}
	b.WriteString("Prefer mouse element=eN and type_text element=eN over a cell: they act through the application itself, are checked against the live window first, and leave the pointer alone where they can.")
	if !t.Complete {
		fmt.Fprintf(&b, " (The read stopped at its time limit after %d nodes, so controls further in may be missing.)", t.Nodes)
	}
	writeElements(&b, t, cellOf)
	return b.String()
}

// writeElements lists a tree's controls, one a line: ref, cell (when cellOf
// names one), role and name, contents, state.
func writeElements(b *strings.Builder, t *Tree, cellOf func(Element) string) {
	for _, e := range t.Elements {
		b.WriteString("\n" + e.Ref + " ")
		if cell := cellOf(e); cell != "" {
			b.WriteString(cell + " ")
		}
		b.WriteString(e.Label())
		switch {
		case e.Secret:
			b.WriteString(" = (hidden)")
		case e.Value != "":
			fmt.Fprintf(b, " = %q", clipValue(e.Value))
		}
		if st := e.states(); len(st) > 0 {
			b.WriteString(" [" + strings.Join(st, ", ") + "]")
		}
	}
	if t.Withheld > 0 {
		fmt.Fprintf(b, "\n(%d more controls not listed: screen_view window=… on a smaller window, or the grid, reaches them.)", t.Withheld)
	}
}

// controlListing is the tree a run ended on, for a parent to continue from.
func controlListing(t *Tree) string {
	var b strings.Builder
	where := fmt.Sprintf("%q", t.Window)
	if t.App != "" {
		where += " (" + t.App + ")"
	}
	fmt.Fprintf(&b, "\nControls now in %s, frame f%d — act on them with mouse element= and type_text element=, or screen_view to see them:", where, t.Frame)
	writeElements(&b, t, func(Element) string { return "" })
	return b.String()
}
