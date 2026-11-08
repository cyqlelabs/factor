package tools

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	_ "image/gif" // decoders the sniffed types are read with
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"os"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"

	"github.com/cyqlelabs/factor/internal/provider"
)

// An image a model is shown is held to the size screen_view sends frames at:
// past that a vision model scales it down itself, and the request carries
// the difference for nothing.
const imageMaxSide = 1568

// imageMaxPixels is the most an image may claim in its header before it is
// decoded. A small file can declare a vast canvas, and decoding one on a
// 3.5 GB box is a reboot.
const imageMaxPixels = 50_000_000

// imageMaxBytes bounds the read of an image file; imagePassBytes is the
// largest one sent as it is, under the five megabytes a provider takes.
const (
	imageMaxBytes  = 32 << 20
	imagePassBytes = 4 << 20
)

// imageTypes are the formats every vision dialect accepts.
var imageTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

// imageType names the format of an image file from its first bytes, or ""
// for anything else. The extension is not asked: a Telegram photo is saved
// under whatever name the server gave it.
func imageType(head []byte) string {
	if t := http.DetectContentType(head); imageTypes[t] {
		return t
	}
	return ""
}

// readImage shows the image at path to the model. head is what the caller
// already read of it.
func readImage(path, mediaType string, head []byte, complete bool) *Result {
	data := head
	if !complete {
		var err error
		if data, err = readWholeImage(path); err != nil {
			return Errorf("%v", err)
		}
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return Errorf("%s looks like %s but does not decode: %v", path, mediaType, err)
	}
	if cfg.Width*cfg.Height > imageMaxPixels {
		return Errorf("%s claims %dx%d pixels, more than is safe to decode", path, cfg.Width, cfg.Height)
	}
	w, h := cfg.Width, cfg.Height
	if max(w, h) > imageMaxSide || len(data) > imagePassBytes {
		if data, mediaType, w, h, err = shrinkImage(data, mediaType); err != nil {
			return Errorf("%s: %v", path, err)
		}
	}
	shown := ""
	if w != cfg.Width {
		shown = fmt.Sprintf(", shown at %dx%d", w, h)
	}
	return &Result{
		ForLLM: fmt.Sprintf("Image %s (%dx%d%s), attached for you to look at.", path, cfg.Width, cfg.Height, shown),
		Images: []provider.ImagePart{{MediaType: mediaType, Data: base64.StdEncoding.EncodeToString(data)}},
	}
}

// readWholeImage reads an image file past the text read's cap, up to
// imageMaxBytes.
func readWholeImage(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, imageMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(data) > imageMaxBytes {
		return nil, fmt.Errorf("%s is an image over %d MB; it is too large to show", path, imageMaxBytes>>20)
	}
	return data, nil
}

// shrinkImage scales an image to fit imageMaxSide. A PNG stays a PNG, so text
// in a screenshot stays sharp; anything else becomes a JPEG.
func shrinkImage(data []byte, mediaType string) ([]byte, string, int, int, error) {
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", 0, 0, fmt.Errorf("decode: %w", err)
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if longest := max(w, h); longest > imageMaxSide {
		w, h = max(1, w*imageMaxSide/longest), max(1, h*imageMaxSide/longest)
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Src, nil)
	var buf bytes.Buffer
	if mediaType == "image/png" {
		if err := png.Encode(&buf, dst); err != nil {
			return nil, "", 0, 0, fmt.Errorf("encode: %w", err)
		}
		if buf.Len() <= imagePassBytes {
			return buf.Bytes(), mediaType, w, h, nil
		}
		// A photograph saved as PNG stays too big at any size a model reads.
		buf.Reset()
	}
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 85}); err != nil {
		return nil, "", 0, 0, fmt.Errorf("encode: %w", err)
	}
	if buf.Len() > imagePassBytes {
		return nil, "", 0, 0, fmt.Errorf("the image is still %d KB after scaling, over the %d KB a provider takes", buf.Len()>>10, imagePassBytes>>10)
	}
	return buf.Bytes(), "image/jpeg", w, h, nil
}
