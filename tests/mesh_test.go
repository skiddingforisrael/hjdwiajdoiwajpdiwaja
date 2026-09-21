package tests

import (
	"image"
	"image/color"
	"math"
	"testing"

	. "github.com/qaustria/AutoPack-Go/utils"
)

func TestFullGridGreedyMeshAndBlenderDimensions(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 16, 16))
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(x * 16), G: uint8(y * 16), B: 127, A: 255})
		}
	}
	mesh, stats, err := BuildGreedyMesh(img, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if stats.OpaqueCells != 256 {
		t.Fatalf("opaque cells = %d, want 256", stats.OpaqueCells)
	}
	if stats.Quads != 6 {
		t.Fatalf("quads = %d, want a single box (6)", stats.Quads)
	}
	if got, want := len(mesh.Positions)/3, 24; got != want {
		t.Fatalf("vertices = %d, want %d", got, want)
	}
	if got, want := len(mesh.Indices), 36; got != want {
		t.Fatalf("indices = %d, want %d", got, want)
	}
	wantXZ := 2 * math.Sqrt(2)
	assertNear(t, stats.BlenderDimensions[0], wantXZ, 1e-12)
	assertNear(t, stats.BlenderDimensions[1], 0.07, 1e-12)
	assertNear(t, stats.BlenderDimensions[2], wantXZ, 1e-12)
}

func TestSingleCellPreservesSourceSizingAndCentering(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 16, 16))
	img.SetNRGBA(3, 9, color.NRGBA{R: 255, A: 255})
	mesh, stats, err := BuildGreedyMesh(img, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Quads != 6 {
		t.Fatalf("quads = %d, want 6", stats.Quads)
	}
	wantXZ := 0.125 * math.Sqrt(2)
	assertNear(t, stats.BlenderDimensions[0], wantXZ, 1e-12)
	assertNear(t, stats.BlenderDimensions[1], 0.07, 1e-12)
	assertNear(t, stats.BlenderDimensions[2], wantXZ, 1e-12)
	// Centering is performed before the one-sided solidify thickness, just as in
	// the Python source. The local XY silhouette center therefore lands at zero.
	var minX, maxX float32 = mesh.Positions[0], mesh.Positions[0]
	for i := 3; i < len(mesh.Positions); i += 3 {
		if mesh.Positions[i] < minX {
			minX = mesh.Positions[i]
		}
		if mesh.Positions[i] > maxX {
			maxX = mesh.Positions[i]
		}
	}
	assertNear(t, float64(minX+maxX), 0, 1e-6)
}

func TestGreedyMeshRemovesInternalFaces(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	// A 3-cell L has 18 naive cube quads. Its closed union needs four front/back
	// rectangles and six straight silhouette runs: ten quads.
	for _, p := range [][2]int{{0, 0}, {1, 0}, {0, 1}} {
		img.SetNRGBA(p[0], p[1], color.NRGBA{A: 255})
	}
	cfg := DefaultConfig()
	_, stats, err := BuildGreedyMesh(img, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Quads != 10 {
		t.Fatalf("quads = %d, want 10", stats.Quads)
	}
}

func TestAlphaThreshold(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	img.SetNRGBA(0, 0, color.NRGBA{A: 1})
	cfg := DefaultConfig()
	cfg.AlphaThreshold = 1
	if _, _, err := BuildGreedyMesh(img, cfg); err == nil {
		t.Fatal("expected fully transparent result to fail")
	}
	if _, _, err := BuildGreedyMesh(img, Config{PlaneSize: -1, Thickness: 0.07}); err == nil {
		t.Fatal("expected a negative plane size to fail")
	}
}

func TestDefaultMeshGeneratorNeverMeshesLowAlphaBackground(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 128, 128))
	// Reproduce trauma.zip's broken canvas: almost every background pixel has
	// alpha 2 instead of alpha 0. The item itself is a small opaque silhouette.
	for y := 0; y < 128; y++ {
		for x := 0; x < 128; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 255, G: 255, B: 255, A: 2})
		}
	}
	for y := 24; y < 104; y++ {
		for x := 58; x < 70; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 120, G: 45, B: 20, A: 255})
		}
	}

	mesh, stats, err := BuildGreedyMesh(img, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if stats.OpaqueCells != 12*80 {
		t.Fatalf("opaque cells = %d, want only the %d item pixels", stats.OpaqueCells, 12*80)
	}
	for vertex := 0; vertex < len(mesh.TexCoords); vertex += 2 {
		u := mesh.TexCoords[vertex]
		v := mesh.TexCoords[vertex+1]
		if u <= 0 || u >= 1 || v <= 0 || v >= 1 {
			t.Fatalf("background reached mesh UVs at (%g, %g)", u, v)
		}
	}
}

func TestDetectBackgroundAlphaNoiseThresholdRejectsNoisyFullPlane(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 16, 16))
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 255, A: 4})
		}
	}
	for y := 5; y < 11; y++ {
		for x := 7; x < 9; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 255, A: 255})
		}
	}

	threshold := DetectBackgroundAlphaNoiseThreshold(img)
	if threshold != 4 {
		t.Fatalf("detected alpha threshold = %d, want 4", threshold)
	}
	cfg := DefaultConfig()
	cfg.AlphaThreshold = threshold
	_, stats, err := BuildGreedyMesh(img, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if stats.OpaqueCells != 12 {
		t.Fatalf("opaque cells = %d, want only 12 real item pixels", stats.OpaqueCells)
	}
}

func TestGreedyMeshAutomaticallyRejectsHigherCanvasAlphaNoise(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 245, G: 245, B: 245, A: 24})
		}
	}
	for y := 8; y < 56; y++ {
		for x := 29; x < 35; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 80, G: 30, B: 15, A: 255})
		}
	}

	_, stats, err := BuildGreedyMesh(img, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if stats.AlphaThreshold != 24 {
		t.Fatalf("effective alpha threshold = %d, want detected canvas alpha 24", stats.AlphaThreshold)
	}
	if stats.OpaqueCells != 6*48 {
		t.Fatalf("opaque cells = %d, want only %d item cells", stats.OpaqueCells, 6*48)
	}
}

func TestDetectBackgroundAlphaNoiseThresholdPreservesNormalAntialiasing(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 16, 16))
	img.SetNRGBA(7, 7, color.NRGBA{A: 4})
	img.SetNRGBA(8, 7, color.NRGBA{A: 255})
	if threshold := DetectBackgroundAlphaNoiseThreshold(img); threshold != 0 {
		t.Fatalf("normal sprite threshold = %d, want 0", threshold)
	}
}

func TestRemoveTinyAlphaIslandsDropsOnlyDetachedArtifacts(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 32, 32))
	for y := 5; y < 25; y++ {
		for x := 5; x < 25; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 90, A: 255})
		}
	}
	// Two-pixel exporter defect: 0.5% of the 400-pixel silhouette.
	img.SetNRGBA(29, 1, color.NRGBA{G: 255, A: 255})
	img.SetNRGBA(30, 1, color.NRGBA{G: 255, A: 255})
	// Five pixels are 1.25% and therefore treated as intentional artwork.
	for x := 0; x < 5; x++ {
		img.SetNRGBA(x, 30, color.NRGBA{B: 255, A: 255})
	}

	if removed := RemoveTinyAlphaIslands(img, 9); removed != 2 {
		t.Fatalf("removed island pixels = %d, want 2", removed)
	}
	if img.NRGBAAt(29, 1).A != 0 || img.NRGBAAt(30, 1).A != 0 {
		t.Fatal("tiny detached artifact remains visible")
	}
	for x := 0; x < 5; x++ {
		if img.NRGBAAt(x, 30).A != 255 {
			t.Fatal("intentional detached component was removed")
		}
	}
}

func TestRemoveTinyAlphaIslandsUsesEightWayConnectivity(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 20, 20))
	for index := 0; index < 12; index++ {
		img.SetNRGBA(index, index, color.NRGBA{A: 255})
	}
	if removed := RemoveTinyAlphaIslands(img, 9); removed != 0 {
		t.Fatalf("diagonal pixel art lost %d pixels", removed)
	}
}

func TestNonSquareImageKeepsAspectRatio(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 4, 2))
	for y := 0; y < 2; y++ {
		for x := 0; x < 4; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 255, A: 255})
		}
	}
	_, stats, err := BuildGreedyMesh(img, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if stats.GridWidth != 4 || stats.GridHeight != 2 {
		t.Fatalf("sampling = %dx%d, want 4x2", stats.GridWidth, stats.GridHeight)
	}
	if stats.Quads != 6 {
		t.Fatalf("quads = %d, want a single rectangular box (6)", stats.Quads)
	}
	wantXZ := 3 / math.Sqrt(2) // 2 units wide and 1 unit high after -45 degrees.
	assertNear(t, stats.BlenderDimensions[0], wantXZ, 1e-12)
	assertNear(t, stats.BlenderDimensions[2], wantXZ, 1e-12)
}

func TestUsesEveryPixelAt512(t *testing.T) {
	const size = 512
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			dx, dy := x-size/2, y-size/2
			distanceSquared := dx*dx + dy*dy
			alpha := uint8(0)
			if distanceSquared <= 180*180 {
				alpha = 255
			} else if distanceSquared <= 181*181 {
				alpha = 128
			}
			img.SetNRGBA(x, y, color.NRGBA{R: 255, G: 180, A: alpha})
		}
	}
	mesh, stats, err := BuildGreedyMesh(img, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if stats.GridWidth != size || stats.GridHeight != size {
		t.Fatalf("sampling = %dx%d, want %dx%d", stats.GridWidth, stats.GridHeight, size, size)
	}
	if !mesh.AlphaBlend {
		t.Fatal("partial-alpha edge did not enable GLB alpha blending")
	}
	if stats.Quads >= stats.OpaqueCells {
		t.Fatalf("greedy meshing did not reduce the 512px silhouette: %d quads for %d cells", stats.Quads, stats.OpaqueCells)
	}
}

func assertNear(t *testing.T, got, want, tolerance float64) {
	t.Helper()
	if math.Abs(got-want) > tolerance {
		t.Fatalf("got %.12g, want %.12g (tolerance %.3g)", got, want, tolerance)
	}
}
