package tools

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readTool(t *testing.T) (Tool, string) {
	t.Helper()
	ws := t.TempDir()
	return NewFSTools(NewPathGuard(ws, true, false, nil))[0], ws
}

func picture(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y += 7 {
		for x := 0; x < w; x += 5 {
			img.SetRGBA(x, y, color.RGBA{uint8(x), uint8(y), 200, 255})
		}
	}
	return img
}

func writePNG(t *testing.T, path string, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func attached(t *testing.T, res *Result) (string, image.Image, []byte) {
	t.Helper()
	if res.IsError || len(res.Images) != 1 {
		t.Fatalf("result = %+v", res)
	}
	raw, err := base64.StdEncoding.DecodeString(res.Images[0].Data)
	if err != nil {
		t.Fatal(err)
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	return res.Images[0].MediaType, img, raw
}

// A photo the user sent is shown as it is when it already fits.
func TestReadFileShowsASmallImageAsItIs(t *testing.T) {
	tool, ws := readTool(t)
	want := writePNG(t, filepath.Join(ws, "file_11.jpg"), picture(640, 480))
	res := tool.Execute(t.Context(), map[string]any{"path": "file_11.jpg", "limit": 3})
	mediaType, _, raw := attached(t, res)
	if mediaType != "image/png" || !bytes.Equal(raw, want) {
		t.Errorf("media type %s, bytes passed through: %v", mediaType, bytes.Equal(raw, want))
	}
	if !strings.Contains(res.ForLLM, "640x480") || strings.Contains(res.ForLLM, "shown at") {
		t.Errorf("ForLLM = %q", res.ForLLM)
	}
}

// A large image is scaled to the size a vision model reads; a JPEG stays a
// JPEG and a PNG a PNG.
func TestReadFileScalesALargeImage(t *testing.T) {
	tool, ws := readTool(t)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, picture(3000, 1500), nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "big.jpg"), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	res := tool.Execute(t.Context(), map[string]any{"path": "big.jpg"})
	mediaType, img, _ := attached(t, res)
	if mediaType != "image/jpeg" || img.Bounds().Dx() != imageMaxSide || img.Bounds().Dy() != 784 {
		t.Errorf("%s %v", mediaType, img.Bounds())
	}
	if !strings.Contains(res.ForLLM, "3000x1500, shown at 1568x784") {
		t.Errorf("ForLLM = %q", res.ForLLM)
	}
	writePNG(t, filepath.Join(ws, "shot.png"), picture(1920, 1080))
	if mediaType, img, _ := attached(t, tool.Execute(t.Context(), map[string]any{"path": "shot.png"})); mediaType != "image/png" || img.Bounds().Dx() != imageMaxSide {
		t.Errorf("%s %v", mediaType, img.Bounds())
	}
}

// A header claiming a vast canvas is refused before anything is decoded.
func TestReadFileRefusesAnImageTooLargeToDecode(t *testing.T) {
	tool, ws := readTool(t)
	data := writePNG(t, filepath.Join(ws, "bomb.png"), picture(4, 4))
	binary.BigEndian.PutUint32(data[16:20], 20000)
	binary.BigEndian.PutUint32(data[20:24], 20000)
	binary.BigEndian.PutUint32(data[29:33], crc32.ChecksumIEEE(data[12:29]))
	if err := os.WriteFile(filepath.Join(ws, "bomb.png"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	res := tool.Execute(t.Context(), map[string]any{"path": "bomb.png"})
	if !res.IsError || !strings.Contains(res.ForLLM, "20000x20000") {
		t.Errorf("result = %+v", res)
	}
}

func TestReadFileSaysWhenAnImageDoesNotDecode(t *testing.T) {
	tool, ws := readTool(t)
	if err := os.WriteFile(filepath.Join(ws, "broken.png"), []byte("\x89PNG\r\n\x1a\nnot really"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := tool.Execute(t.Context(), map[string]any{"path": "broken.png"})
	if !res.IsError || !strings.Contains(res.ForLLM, "does not decode") {
		t.Errorf("result = %+v", res)
	}
}

// Text is read as text, whatever the file is called.
func TestReadFileReadsTextNamedLikeAnImage(t *testing.T) {
	tool, ws := readTool(t)
	if err := os.WriteFile(filepath.Join(ws, "notes.png"), []byte("just words"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := tool.Execute(t.Context(), map[string]any{"path": "notes.png"})
	if res.IsError || res.ForLLM != "just words" || len(res.Images) != 0 {
		t.Errorf("result = %+v", res)
	}
}

// An image past the text read's cap is read whole for the picture.
func TestReadFileReadsALargeImageFileWhole(t *testing.T) {
	tool, ws := readTool(t)
	noise := image.NewRGBA(image.Rect(0, 0, 1500, 1500))
	rng := rand.New(rand.NewPCG(1, 2))
	for i := range noise.Pix {
		noise.Pix[i] = uint8(rng.Uint32())
	}
	data := writePNG(t, filepath.Join(ws, "noise.png"), noise)
	if len(data) <= 4<<20 {
		t.Skipf("fixture is %d bytes, not past the 4 MB text cap", len(data))
	}
	if _, img, _ := attached(t, tool.Execute(t.Context(), map[string]any{"path": "noise.png"})); img.Bounds().Dx() != 1500 {
		t.Errorf("bounds = %v", img.Bounds())
	}
	huge := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, imageMaxBytes)...)
	if err := os.WriteFile(filepath.Join(ws, "huge.png"), huge, 0o644); err != nil {
		t.Fatal(err)
	}
	if res := tool.Execute(t.Context(), map[string]any{"path": "huge.png"}); !res.IsError || !strings.Contains(res.ForLLM, "too large to show") {
		t.Errorf("result = %+v", res)
	}
}
