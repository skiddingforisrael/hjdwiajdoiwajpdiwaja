package utils

import (
	"archive/zip"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnzipTexturePackRejectsExcessiveEntryCount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "many.zip")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(file)
	for index := 0; index <= maxPackFiles; index++ {
		if _, err := archive.CreateHeader(&zip.FileHeader{Name: strings.Repeat("a", index%3+1) + string(rune(index+1)), Method: zip.Store}); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if pack, err := UnzipTexturePack(path); err == nil || !strings.Contains(err.Error(), "entries; limit") {
		if pack != nil {
			_ = pack.Cleanup()
		}
		t.Fatalf("entry-count error = %v", err)
	}
}

func TestUnzipTexturePackRejectsDeclaredEntryBomb(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bomb.zip")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(file)
	if _, err := archive.Create("assets/minecraft/textures/items/diamond_sword.png"); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	const centralHeaderSignature = 0x02014b50
	patched := false
	for offset := 0; offset+28 <= len(contents); offset++ {
		if binary.LittleEndian.Uint32(contents[offset:offset+4]) != centralHeaderSignature {
			continue
		}
		binary.LittleEndian.PutUint32(contents[offset+24:offset+28], uint32(maxPackFileSize+1))
		patched = true
		break
	}
	if !patched {
		t.Fatal("ZIP central directory was not found")
	}
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	if pack, err := UnzipTexturePack(path); err == nil || !strings.Contains(err.Error(), "larger than") {
		if pack != nil {
			_ = pack.Cleanup()
		}
		t.Fatalf("declared-size error = %v", err)
	}
}
