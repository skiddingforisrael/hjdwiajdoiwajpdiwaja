package tests

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"testing"

	. "github.com/qaustria/AutoPack-Go/utils"
)

const (
	testGLBMagic   = 0x46546c67
	testGLBVersion = 2
	testChunkJSON  = 0x4e4f534a
	testChunkBIN   = 0x004e4942
)

func TestEncodeGLBIsSelfContainedAndValidlyStructured(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	img.SetNRGBA(0, 0, color.NRGBA{R: 230, G: 20, B: 40, A: 255})
	mesh, _, err := BuildGreedyMesh(img, Config{
		PlaneSize: 2, Thickness: 0.07, Center: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var texture bytes.Buffer
	if err := png.Encode(&texture, img); err != nil {
		t.Fatal(err)
	}
	glb, err := EncodeGLB(mesh, texture.Bytes(), "one.png")
	if err != nil {
		t.Fatal(err)
	}
	if got := binary.LittleEndian.Uint32(glb[0:4]); got != testGLBMagic {
		t.Fatalf("magic = %#x", got)
	}
	if got := binary.LittleEndian.Uint32(glb[4:8]); got != testGLBVersion {
		t.Fatalf("version = %d", got)
	}
	if got := int(binary.LittleEndian.Uint32(glb[8:12])); got != len(glb) {
		t.Fatalf("declared length %d != %d", got, len(glb))
	}
	jsonLen := int(binary.LittleEndian.Uint32(glb[12:16]))
	if got := binary.LittleEndian.Uint32(glb[16:20]); got != testChunkJSON {
		t.Fatalf("first chunk = %#x", got)
	}
	var doc map[string]any
	if err := json.Unmarshal(glb[20:20+jsonLen], &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc["images"].([]any)) != 1 {
		t.Fatal("embedded image missing")
	}
	if len(doc["buffers"].([]any)) != 1 {
		t.Fatal("single embedded buffer missing")
	}
	material := doc["materials"].([]any)[0].(map[string]any)
	if material["alphaMode"] != "OPAQUE" {
		t.Fatalf("opaque source alpha mode = %v, want OPAQUE", material["alphaMode"])
	}
	binHeader := 20 + jsonLen
	binLen := int(binary.LittleEndian.Uint32(glb[binHeader : binHeader+4]))
	if got := binary.LittleEndian.Uint32(glb[binHeader+4 : binHeader+8]); got != testChunkBIN {
		t.Fatalf("second chunk = %#x", got)
	}
	if binHeader+8+binLen != len(glb) {
		t.Fatal("BIN chunk does not fill GLB")
	}

	mesh.AlphaBlend = true
	glb, err = EncodeGLB(mesh, texture.Bytes(), "one.png")
	if err != nil {
		t.Fatal(err)
	}
	jsonLen = int(binary.LittleEndian.Uint32(glb[12:16]))
	if err := json.Unmarshal(glb[20:20+jsonLen], &doc); err != nil {
		t.Fatal(err)
	}
	material = doc["materials"].([]any)[0].(map[string]any)
	if material["alphaMode"] != "BLEND" {
		t.Fatalf("partial-alpha source alpha mode = %v, want BLEND", material["alphaMode"])
	}
}

func TestEncodeGeometryGLBOmitsTextureAndMaterial(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	img.SetNRGBA(0, 0, color.NRGBA{A: 255})
	mesh, _, err := BuildGreedyMesh(img, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	glb, err := EncodeGeometryGLB(mesh)
	if err != nil {
		t.Fatal(err)
	}
	jsonLen := int(binary.LittleEndian.Uint32(glb[12:16]))
	var doc map[string]any
	if err := json.Unmarshal(glb[20:20+jsonLen], &doc); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"images", "textures", "materials"} {
		if _, exists := doc[key]; exists {
			t.Fatalf("geometry-only GLB unexpectedly contains %q", key)
		}
	}
	primitive := doc["meshes"].([]any)[0].(map[string]any)["primitives"].([]any)[0].(map[string]any)
	if _, exists := primitive["material"]; exists {
		t.Fatal("geometry-only GLB primitive unexpectedly references a material")
	}
}
