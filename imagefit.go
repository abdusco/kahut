package main

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	_ "image/png" // registers the PNG decoder for image.Decode

	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // registers the WebP decoder
)

const (
	jpegQuality = 85
	// Larger images are left alone rather than decoded, to bound memory.
	maxDecodePixels = 24 << 20
	// Generated images fit in this box (swapped for portrait images).
	fitWidth, fitHeight = 800, 600
)

// normalizeImage shrinks a generated image to fit 800x600 (or 600x800), never
// enlarging, and converts PNGs to JPEG. Transparent areas become white. JPEG
// and WebP images that already fit are returned as they are, as is anything
// that cannot be decoded or would only get bigger.
func normalizeImage(data []byte) []byte {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width < 1 || cfg.Height < 1 || cfg.Width*cfg.Height > maxDecodePixels {
		return data
	}
	boxW, boxH := fitWidth, fitHeight
	if cfg.Height > cfg.Width {
		boxW, boxH = boxH, boxW
	}
	w, h := cfg.Width, cfg.Height
	if w > boxW || h > boxH {
		scale := min(float64(boxW)/float64(w), float64(boxH)/float64(h))
		w, h = max(1, int(float64(w)*scale)), max(1, int(float64(h)*scale))
	}
	resized := w != cfg.Width || h != cfg.Height
	if !resized && format != "png" {
		return data
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return data
	}
	canvas := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(canvas, canvas.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	xdraw.CatmullRom.Scale(canvas, canvas.Bounds(), src, src.Bounds(), xdraw.Over, nil)
	var out bytes.Buffer
	if err := jpeg.Encode(&out, canvas, &jpeg.Options{Quality: jpegQuality}); err != nil {
		return data
	}
	// A resized image is always smaller in pixels; otherwise only convert when
	// the JPEG is actually smaller than the PNG.
	if !resized && out.Len() >= len(data) {
		return data
	}
	return out.Bytes()
}
