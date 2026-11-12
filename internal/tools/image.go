package tools

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"net/http"
	"os"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

const (
	// Past 20 images in one request, history included, the API rejects any over 2000 px a side.
	imageMaxEdge = 2000
	// 5 MB once base64-encoded, the stricter of the direct API's and Bedrock/Vertex's limits.
	imageMaxBytes = 3_750_000
	// Checked before decoding, so a small file claiming huge dimensions cannot exhaust memory.
	imageMaxPixels = 100_000_000
	binarySniff    = 8000
)

type Image struct {
	MediaType string
	Data      string // base64
	Width     int
	Height    int
	Bytes     int
}

var imageTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

// sniffImage decides on content, not extension, so a misnamed file is still read for what it is.
func sniffImage(data []byte) (string, bool) {
	kind := http.DetectContentType(data)
	return kind, imageTypes[kind]
}

// git's test: a NUL early on. Invalid UTF-8 alone would also refuse Latin-1 text.
func isBinary(data []byte) bool {
	return bytes.IndexByte(data[:min(len(data), binarySniff)], 0) >= 0
}

func loadImage(data []byte, mediaType string) (*Image, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %v", mediaType, err)
	}
	if cfg.Width*cfg.Height > imageMaxPixels {
		return nil, fmt.Errorf("%d×%d is too large to read", cfg.Width, cfg.Height)
	}
	if max(cfg.Width, cfg.Height) <= imageMaxEdge && len(data) <= imageMaxBytes {
		return newImage(data, mediaType, cfg.Width, cfg.Height), nil
	}

	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("cannot decode %s: %v", mediaType, err)
	}
	w, h := cfg.Width, cfg.Height
	if edge := max(w, h); edge > imageMaxEdge {
		w, h = max(1, w*imageMaxEdge/edge), max(1, h*imageMaxEdge/edge)
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Src, nil)

	// Lossless stays lossless, because screenshots are mostly text, unless that will not fit.
	if mediaType == "image/png" || mediaType == "image/gif" {
		var buf bytes.Buffer
		if err := png.Encode(&buf, dst); err == nil && buf.Len() <= imageMaxBytes {
			return newImage(buf.Bytes(), "image/png", w, h), nil
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 85}); err != nil {
		return nil, err
	}
	if buf.Len() > imageMaxBytes {
		return nil, fmt.Errorf("still %d bytes after scaling to %d×%d", buf.Len(), w, h)
	}
	return newImage(buf.Bytes(), "image/jpeg", w, h), nil
}

// LoadImageFile reads an image the way view does, scaled to fit what the API accepts.
func LoadImageFile(path string) (*Image, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	kind, ok := sniffImage(data)
	if !ok {
		return nil, fmt.Errorf("%s is %s, not a supported image", path, kind)
	}
	return loadImage(data, kind)
}

func newImage(data []byte, mediaType string, w, h int) *Image {
	return &Image{MediaType: mediaType, Data: base64.StdEncoding.EncodeToString(data), Width: w, Height: h, Bytes: len(data)}
}

func (img *Image) Summary() string {
	return fmt.Sprintf("image %d×%d %s, %s", img.Width, img.Height, img.MediaType[len("image/"):], humanBytes(img.Bytes))
}

func humanBytes(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1f MB", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%d KB", n/1_000)
	}
	return fmt.Sprintf("%d B", n)
}
