package utils

import (
	"archive/zip"
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

// buildTestSkySheet makes a tiny 3x2 grid PNG (each face solid-colored,
// distinguishable by index) to stand in for a real cloud sheet.
func buildTestSkySheet(t *testing.T, tile int, fill color.NRGBA) []byte {
	t.Helper()
	sheet := image.NewNRGBA(image.Rect(0, 0, tile*3, tile*2))
	for y := 0; y < sheet.Bounds().Dy(); y++ {
		for x := 0; x < sheet.Bounds().Dx(); x++ {
			sheet.SetNRGBA(x, y, fill)
		}
	}
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, sheet); err != nil {
		t.Fatalf("encode test sky sheet: %v", err)
	}
	return buffer.Bytes()
}

func buildTestSkyZip(t *testing.T, entries map[string][]byte) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "pack.zip")
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("create test zip: %v", err)
	}
	defer file.Close()
	writer := zip.NewWriter(file)
	for name, data := range entries {
		entryWriter, err := writer.Create(name)
		if err != nil {
			t.Fatalf("create zip entry %q: %v", name, err)
		}
		if _, err := entryWriter.Write(data); err != nil {
			t.Fatalf("write zip entry %q: %v", name, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close test zip: %v", err)
	}
	return path
}

func TestDetectSkyOptionsSingleSkyReturnsAtMostOne(t *testing.T) {
	day := buildTestSkySheet(t, 16, color.NRGBA{135, 206, 235, 255})
	zipPath := buildTestSkyZip(t, map[string][]byte{
		"assets/minecraft/mcpatcher/sky/world0/cloud1.png":  day,
		"assets/minecraft/textures/items/diamond_sword.png": []byte("not a sky, ignored by the sky scanner"),
	})
	options, err := DetectSkyOptions(zipPath)
	if err != nil {
		t.Fatalf("DetectSkyOptions: %v", err)
	}
	if len(options) != 1 {
		t.Fatalf("len(options) = %d, want 1", len(options))
	}
	if options[0].Label != "Overworld · Day" {
		t.Fatalf("options[0].Label = %q, want %q", options[0].Label, "Overworld · Day")
	}
	if len(options[0].ThumbnailPNG) == 0 {
		t.Fatal("options[0].ThumbnailPNG is empty")
	}
}

func TestDetectSkyOptionsMultipleSkiesSortedAndLabeled(t *testing.T) {
	day := buildTestSkySheet(t, 16, color.NRGBA{135, 206, 235, 255})
	sunset := buildTestSkySheet(t, 16, color.NRGBA{255, 120, 40, 255})
	night := buildTestSkySheet(t, 16, color.NRGBA{10, 10, 40, 255})
	zipPath := buildTestSkyZip(t, map[string][]byte{
		"assets/minecraft/mcpatcher/sky/world0/cloud3.png": night,
		"assets/minecraft/mcpatcher/sky/world0/cloud1.png": day,
		"assets/minecraft/mcpatcher/sky/world0/cloud2.png": sunset,
	})
	options, err := DetectSkyOptions(zipPath)
	if err != nil {
		t.Fatalf("DetectSkyOptions: %v", err)
	}
	wantLabels := []string{"Overworld · Day", "Overworld · Sunset", "Overworld · Night"}
	if len(options) != len(wantLabels) {
		t.Fatalf("len(options) = %d, want %d", len(options), len(wantLabels))
	}
	for index, want := range wantLabels {
		if options[index].Label != want {
			t.Errorf("options[%d].Label = %q, want %q", index, options[index].Label, want)
		}
	}
}

func TestRewritePrimarySkyPointsPipelineAtChosenSky(t *testing.T) {
	day := buildTestSkySheet(t, 16, color.NRGBA{135, 206, 235, 255})
	night := buildTestSkySheet(t, 16, color.NRGBA{10, 10, 40, 255})
	zipPath := buildTestSkyZip(t, map[string][]byte{
		"assets/minecraft/mcpatcher/sky/world0/cloud1.png":  day,
		"assets/minecraft/mcpatcher/sky/world0/cloud3.png":  night,
		"assets/minecraft/textures/items/diamond_sword.png": []byte("sword texture"),
	})

	rewrittenPath, cleanup, err := RewritePrimarySky(zipPath, "assets/minecraft/mcpatcher/sky/world0/cloud3.png")
	if err != nil {
		t.Fatalf("RewritePrimarySky: %v", err)
	}
	defer cleanup()

	pack, err := UnzipTexturePack(rewrittenPath)
	if err != nil {
		t.Fatalf("UnzipTexturePack(rewritten): %v", err)
	}
	defer pack.Cleanup()
	if pack.Textures["sky_cloud_0"] == "" {
		t.Fatal("rewritten pack has no sliced sky faces")
	}
	facePNG, err := os.ReadFile(pack.Textures["sky_cloud_0"])
	if err != nil {
		t.Fatalf("read sliced sky face: %v", err)
	}
	nightFace, _, err := image.Decode(bytes.NewReader(night))
	if err != nil {
		t.Fatalf("decode night sheet: %v", err)
	}
	decodedFace, err := png.Decode(bytes.NewReader(facePNG))
	if err != nil {
		t.Fatalf("decode sliced face: %v", err)
	}
	wantR, wantG, wantB, _ := nightFace.At(0, 0).RGBA()
	gotR, gotG, gotB, _ := decodedFace.At(0, 0).RGBA()
	if wantR != gotR || wantG != gotG || wantB != gotB {
		t.Fatalf("rewritten pack's sky face color = (%d,%d,%d), want the chosen night sky's color (%d,%d,%d)",
			gotR, gotG, gotB, wantR, wantG, wantB)
	}
	if pack.Textures["diamond_sword"] == "" {
		t.Fatal("rewriting the sky should not drop unrelated textures like diamond_sword")
	}
}

func TestBuildSingleSkyZipProducesSkyOnlyPack(t *testing.T) {
	sunset := buildTestSkySheet(t, 16, color.NRGBA{255, 120, 40, 255})
	zipPath := buildTestSkyZip(t, map[string][]byte{
		"assets/minecraft/mcpatcher/sky/world0/cloud2.png":  sunset,
		"assets/minecraft/textures/items/diamond_sword.png": []byte("sword texture"),
	})

	extraZipPath, cleanup, err := BuildSingleSkyZip(zipPath, "assets/minecraft/mcpatcher/sky/world0/cloud2.png")
	if err != nil {
		t.Fatalf("BuildSingleSkyZip: %v", err)
	}
	defer cleanup()

	pack, err := UnzipTexturePack(extraZipPath)
	if err != nil {
		t.Fatalf("UnzipTexturePack(extra): %v", err)
	}
	defer pack.Cleanup()
	if pack.Textures["sky_cloud_0"] == "" {
		t.Fatal("sky-only pack has no sliced sky faces")
	}
	if pack.Textures["diamond_sword"] != "" {
		t.Fatal("sky-only pack should not carry over the original pack's item textures")
	}
}
