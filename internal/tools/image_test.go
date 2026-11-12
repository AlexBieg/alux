package tools

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writePNG(t *testing.T, path string, w, h int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := range w {
		img.Set(x, x%h, color.RGBA{R: 200, A: 255})
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Sniffed from the bytes, so a screenshot saved without its extension is still an image.
func TestViewReturnsAnImageFromItsContentNotItsName(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "shot")
	writePNG(t, path, 40, 30)

	res := RunEdit(EditInput{Command: "view", Path: path})
	if res.IsError || res.Image == nil {
		t.Fatalf("no image: %+v", res)
	}
	if res.Image.MediaType != "image/png" || res.Image.Width != 40 || res.Image.Height != 30 {
		t.Errorf("got %s %d×%d", res.Image.MediaType, res.Image.Width, res.Image.Height)
	}
	if _, err := base64.StdEncoding.DecodeString(res.Image.Data); err != nil {
		t.Errorf("data is not base64: %v", err)
	}
	if !strings.Contains(res.Display, "image 40×30 png") {
		t.Errorf("display is not a summary: %q", res.Display)
	}
}

// Past 20 images in a request the API rejects any over 2000 px, so a large one is scaled on read.
func TestViewScalesALargeImageToTheManyImageLimit(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "big.png")
	writePNG(t, path, 4000, 1000)

	res := RunEdit(EditInput{Command: "view", Path: path})
	if res.IsError || res.Image == nil {
		t.Fatalf("no image: %+v", res)
	}
	if res.Image.Width != 2000 || res.Image.Height != 500 {
		t.Errorf("scaled to %d×%d, want 2000×500", res.Image.Width, res.Image.Height)
	}
	raw, _ := base64.StdEncoding.DecodeString(res.Image.Data)
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || cfg.Width != 2000 || cfg.Height != 500 {
		t.Errorf("encoded image is %d×%d (%v)", cfg.Width, cfg.Height, err)
	}
}

func TestViewRefusesABinaryFileRatherThanNumberingItsBytes(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "blob.bin")
	if err := os.WriteFile(path, []byte("\x7fELF\x02\x01\x00\x00\x01\x02\x03"), 0o644); err != nil {
		t.Fatal(err)
	}

	res := RunEdit(EditInput{Command: "view", Path: path})
	if !res.IsError || res.Image != nil {
		t.Fatalf("binary viewed as %+v", res)
	}
	if !strings.Contains(res.Output, "binary") {
		t.Errorf("error does not say binary: %q", res.Output)
	}
}

func TestViewRangeOnAnImageIsRefused(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "a.png")
	writePNG(t, path, 10, 10)

	res := RunEdit(EditInput{Command: "view", Path: path, ViewRange: []int{1, 5}})
	if !res.IsError {
		t.Fatalf("view_range on an image succeeded: %+v", res)
	}
}
