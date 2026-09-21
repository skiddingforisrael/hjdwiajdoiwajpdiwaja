package tests

import (
	"image"
	"image/color"
	"math"
	"testing"

	. "github.com/qaustria/AutoPack-Go/utils"
)

func TestEdgeExpandUsesExactSizeDistanceAndFalloff(t *testing.T) {
	source := image.NewNRGBA(image.Rect(0, 0, 512, 512))
	source.SetNRGBA(256, 256, color.NRGBA{R: 12, G: 34, B: 56, A: 255})
	result := EdgeExpand(source)
	if result.Bounds() != image.Rect(0, 0, 512, 512) {
		t.Fatalf("bounds = %v", result.Bounds())
	}
	for _, distance := range []int{0, 1, 24, 45, 46, 47, 48, 49} {
		pixel := result.NRGBAAt(256+distance, 256)
		wantAlpha := uint8(0)
		if distance <= 48 {
			fade := math.Max(0, float64(48-distance)/48)
			wantAlpha = uint8(fade*fade*255 + 0.5)
		}
		if pixel.A != wantAlpha {
			t.Fatalf("alpha at distance %d = %d, want %d", distance, pixel.A, wantAlpha)
		}
		if wantAlpha > 0 && (pixel.R != 12 || pixel.G != 34 || pixel.B != 56) {
			t.Fatalf("RGB at distance %d = %v", distance, pixel)
		}
	}
}

func TestEdgeExpandNearestResizeAndDoesNotMutateSource(t *testing.T) {
	source := image.NewNRGBA(image.Rect(0, 0, 16, 16))
	source.SetNRGBA(0, 0, color.NRGBA{R: 255, A: 255})
	source.SetNRGBA(1, 0, color.NRGBA{G: 255, A: 255})
	original := append([]byte(nil), source.Pix...)
	result := EdgeExpand(source)
	if got := result.NRGBAAt(31, 0); got.R != 255 || got.A != 255 {
		t.Fatalf("last pixel of first nearest block = %v", got)
	}
	if got := result.NRGBAAt(32, 0); got.G != 255 || got.A != 255 {
		t.Fatalf("first pixel of second nearest block = %v", got)
	}
	for index := range original {
		if source.Pix[index] != original[index] {
			t.Fatalf("source changed at byte %d", index)
		}
	}
}

func TestEdgeExpandTieUsesPythonBFSSeedOrder(t *testing.T) {
	source := image.NewNRGBA(image.Rect(0, 0, 512, 512))
	source.SetNRGBA(100, 200, color.NRGBA{R: 255, A: 255})
	source.SetNRGBA(104, 200, color.NRGBA{B: 255, A: 255})
	result := EdgeExpand(source)
	// At the equal-distance center, row-major source initialization reaches the
	// pixel from the left seed first, exactly like the Python deque.
	if got := result.NRGBAAt(102, 200); got.R != 255 || got.B != 0 {
		t.Fatalf("equal-distance source color = %v, want left/red seed", got)
	}
}

func TestResizeTexturePreservesNearTransparentPixels(t *testing.T) {
	source := image.NewNRGBA(image.Rect(0, 0, 2, 1))
	source.SetNRGBA(0, 0, color.NRGBA{R: 230, G: 240, B: 250, A: 4})
	source.SetNRGBA(1, 0, color.NRGBA{R: 12, G: 34, B: 56, A: 255})

	result := ResizeTexture(source)
	if got := result.NRGBAAt(0, 0); got != (color.NRGBA{R: 230, G: 240, B: 250, A: 4}) {
		t.Fatalf("near-transparent source pixel changed: %v", got)
	}
	if got := result.NRGBAAt(511, 0); got != (color.NRGBA{R: 12, G: 34, B: 56, A: 255}) {
		t.Fatalf("visible texture pixel = %v", got)
	}
}

func TestRemoveBackgroundAlphaNoiseOnlyCleansBrokenCanvas(t *testing.T) {
	broken := image.NewNRGBA(image.Rect(0, 0, 16, 16))
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			broken.SetNRGBA(x, y, color.NRGBA{R: 230, G: 240, B: 250, A: 4})
		}
	}
	broken.SetNRGBA(8, 8, color.NRGBA{R: 12, G: 34, B: 56, A: 255})
	if threshold := RemoveBackgroundAlphaNoise(broken); threshold != 4 {
		t.Fatalf("broken canvas threshold = %d, want 4", threshold)
	}
	if got := broken.NRGBAAt(0, 0); got != (color.NRGBA{}) {
		t.Fatalf("broken canvas background = %v, want transparent", got)
	}
	if got := broken.NRGBAAt(8, 8); got != (color.NRGBA{R: 12, G: 34, B: 56, A: 255}) {
		t.Fatalf("real item pixel changed: %v", got)
	}

	normal := image.NewNRGBA(image.Rect(0, 0, 16, 16))
	normal.SetNRGBA(7, 7, color.NRGBA{R: 100, A: 4})
	normal.SetNRGBA(8, 7, color.NRGBA{R: 200, A: 255})
	before := append([]byte(nil), normal.Pix...)
	if threshold := RemoveBackgroundAlphaNoise(normal); threshold != 0 {
		t.Fatalf("normal sprite threshold = %d, want 0", threshold)
	}
	for index := range before {
		if normal.Pix[index] != before[index] {
			t.Fatalf("normal sprite changed at byte %d", index)
		}
	}
}
