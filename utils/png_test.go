package utils

import (
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDecodeTexturePNGAcceptsNormalTexture(t *testing.T) {
	path := writePNGFixture(t, image.NewNRGBA(image.Rect(0, 0, 512, 512)))
	decoded, err := DecodeTexturePNG(path)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Bounds().Dx() != 512 || decoded.Bounds().Dy() != 512 {
		t.Fatalf("decoded bounds = %v", decoded.Bounds())
	}
}

func TestDecodeTexturePNGRejectsOversizedDimensionsBeforeDecode(t *testing.T) {
	// This image is cheap to construct but exceeds the one-side safety limit.
	path := writePNGFixture(t, image.NewUniform(color.NRGBA{R: 255, A: 255}), image.Rect(0, 0, MaxTextureDimension+1, 1))
	_, err := DecodeTexturePNG(path)
	if err == nil || !strings.Contains(err.Error(), "limit is") {
		t.Fatalf("oversized PNG error = %v", err)
	}
}

func writePNGFixture(t *testing.T, source image.Image, bounds ...image.Rectangle) string {
	t.Helper()
	img := source
	if len(bounds) != 0 {
		// Uniform images have effectively infinite bounds. Wrap only the requested
		// rectangle without allocating its entire pixel buffer.
		img = boundedImage{Image: source, bounds: bounds[0]}
	}
	path := filepath.Join(t.TempDir(), "texture.png")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(file, img); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

type boundedImage struct {
	image.Image
	bounds image.Rectangle
}

func (img boundedImage) Bounds() image.Rectangle { return img.bounds }
