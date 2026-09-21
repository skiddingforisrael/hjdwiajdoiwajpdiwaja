package tests

import (
	"archive/zip"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	. "github.com/qaustria/AutoPack-Go/utils"
)

func TestUnzipTexturePackFindsWrappedAssetsAndGeneratesPotions(t *testing.T) {
	zipPath := filepath.Join(t.TempDir(), "pack.zip")
	files := map[string]image.Image{
		"My Pack/assets/minecraft/textures/items/diamond_sword.png":           solidImage(color.NRGBA{R: 5, G: 10, B: 15, A: 255}),
		"My Pack/assets/minecraft/textures/items/wooden_sword.png":            solidImage(color.NRGBA{R: 20, G: 25, B: 30, A: 255}),
		"My Pack/assets/minecraft/textures/items/potion_overlay.png":          solidImage(color.NRGBA{R: 255, G: 255, B: 255, A: 128}),
		"My Pack/assets/minecraft/textures/items/potion_bottle_drinkable.png": transparentImage(),
		"My Pack/assets/minecraft/textures/blocks/wool_colored_red.png":       solidImage(color.NRGBA{R: 150, A: 255}),
	}
	writeTextureZIP(t, zipPath, files, map[string][]byte{
		"My Pack/assets/minecraft/sounds/unrelated.bin": {1, 2, 3},
	})

	pack, err := UnzipTexturePack(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	tempDir := pack.TempDir
	defer pack.Cleanup()
	for _, key := range []string{"diamond_sword", "wood_sword", "wool_red", "jump_pot", "speed_pot"} {
		path := pack.Textures[key]
		if path == "" {
			t.Fatalf("texture %q was not found/generated", key)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("texture %q path is unusable: %v", key, err)
		}
	}
	jumpFile, err := os.Open(pack.Textures["jump_pot"])
	if err != nil {
		t.Fatal(err)
	}
	jump, err := png.Decode(jumpFile)
	_ = jumpFile.Close()
	if err != nil {
		t.Fatal(err)
	}
	pixel := color.NRGBAModel.Convert(jump.At(0, 0)).(color.NRGBA)
	if pixel.R != 0x22 || pixel.G != 0xff || pixel.B != 0x4c || pixel.A != 128 {
		t.Fatalf("jump potion pixel = %#02x %#02x %#02x %#02x", pixel.R, pixel.G, pixel.B, pixel.A)
	}
	if !containsString(pack.Missing, "iron_sword") {
		t.Fatal("missing texture list does not contain iron_sword")
	}
	if _, err := os.Stat(filepath.Join(pack.TempDir, "My Pack/assets/minecraft/sounds/unrelated.bin")); !os.IsNotExist(err) {
		t.Fatalf("unrelated pack file should not be extracted: %v", err)
	}
	if err := pack.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tempDir); !os.IsNotExist(err) {
		t.Fatalf("temporary directory still exists after cleanup: %v", err)
	}
}

func TestUnzipTexturePackRejectsZipSlip(t *testing.T) {
	zipPath := filepath.Join(t.TempDir(), "bad.zip")
	writeTextureZIP(t, zipPath, nil, map[string][]byte{"../../escape.png": {1, 2, 3}})
	pack, err := UnzipTexturePack(zipPath)
	if err == nil {
		if pack != nil {
			_ = pack.Cleanup()
		}
		t.Fatal("expected traversal entry to fail")
	}
}

func TestUnzipTexturePackMissingPotionLayers(t *testing.T) {
	zipPath := filepath.Join(t.TempDir(), "partial.zip")
	writeTextureZIP(t, zipPath, map[string]image.Image{
		"assets/minecraft/textures/items/diamond.png": solidImage(color.NRGBA{B: 255, A: 255}),
	}, nil)
	pack, err := UnzipTexturePack(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	defer pack.Cleanup()
	if !containsString(pack.Missing, "jump_pot") || !containsString(pack.Missing, "speed_pot") {
		t.Fatalf("missing potion layers were not reported: %v", pack.Missing)
	}
}

func writeTextureZIP(t *testing.T, path string, images map[string]image.Image, raw map[string][]byte) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(file)
	for name, img := range images {
		entry, err := archive.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := png.Encode(entry, img); err != nil {
			t.Fatal(err)
		}
	}
	for name, contents := range raw {
		entry, err := archive.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write(contents); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func solidImage(c color.NRGBA) image.Image {
	img := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	for y := 0; y < 2; y++ {
		for x := 0; x < 2; x++ {
			img.SetNRGBA(x, y, c)
		}
	}
	return img
}

func transparentImage() image.Image {
	return image.NewNRGBA(image.Rect(0, 0, 2, 2))
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
