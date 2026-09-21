package tests

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"math"
	"testing"

	. "github.com/qaustria/AutoPack-Go/utils"
)

func TestEncodeRobloxMeshHasNativeVertexAndFaceData(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	for y := 1; y < 3; y++ {
		for x := 1; x < 3; x++ {
			img.SetNRGBA(x, y, color.NRGBA{A: 255})
		}
	}
	mesh, _, err := BuildGreedyMesh(img, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	data, err := EncodeRobloxMesh(mesh)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(data, []byte("version 2.00\n")) {
		t.Fatalf("native mesh header = %q", data[:13])
	}
	if got := binary.LittleEndian.Uint16(data[13:15]); got != 12 {
		t.Fatalf("header size = %d", got)
	}
	if data[15] != 40 || data[16] != 12 {
		t.Fatalf("vertex/face sizes = %d/%d", data[15], data[16])
	}
	vertices := binary.LittleEndian.Uint32(data[17:21])
	faces := binary.LittleEndian.Uint32(data[21:25])
	if vertices != uint32(len(mesh.Positions)/3) || faces != uint32(len(mesh.Indices)/3) || vertices == 0 || faces == 0 {
		t.Fatalf("native counts = %d vertices, %d faces", vertices, faces)
	}
	wantSize := 25 + int(vertices)*40 + int(faces)*12
	if len(data) != wantSize {
		t.Fatalf("native mesh size = %d, want %d", len(data), wantSize)
	}
	// Vertex color is opaque white and tangent bytes use Roblox's biased range.
	firstVertex := data[25 : 25+40]
	if !bytes.Equal(firstVertex[36:40], []byte{255, 255, 255, 255}) {
		t.Fatalf("vertex color = %v", firstVertex[36:40])
	}
	for _, component := range firstVertex[32:36] {
		if component == 255 {
			t.Fatalf("invalid biased tangent component %d", component)
		}
	}
}

func TestEncodeRobloxMeshRejectsInvalidIndices(t *testing.T) {
	mesh := Mesh{
		Positions: []float32{0, 0, 0},
		Normals:   []float32{0, 0, 1},
		TexCoords: []float32{0, 0},
		Indices:   []uint32{0, 0, math.MaxUint32},
	}
	if _, err := EncodeRobloxMesh(mesh); err == nil {
		t.Fatal("out-of-range index was accepted")
	}
}
