package utils

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

const (
	robloxMeshVersion   = "version 2.00\n"
	robloxMeshHeaderLen = 12
	robloxVertexLen     = 40
	robloxFaceLen       = 12
)

// EncodeRobloxMesh writes Roblox's native indexed mesh stream. The Open Cloud
// Mesh endpoint requires this format; feeding it FBX can create an approved
// asset whose processed mesh contains zero vertices and zero faces.
//
// The portable/library export remains EncodeGLB. This encoder exists solely
// for producing a raw asset ID that Roblox games can assign to MeshPart.MeshId.
func EncodeRobloxMesh(mesh Mesh) ([]byte, error) {
	vertexCount := len(mesh.Positions) / 3
	if vertexCount == 0 || len(mesh.Positions)%3 != 0 {
		return nil, errors.New("mesh has no valid positions")
	}
	if len(mesh.Normals) != len(mesh.Positions) || len(mesh.TexCoords) != vertexCount*2 {
		return nil, errors.New("mesh attribute counts do not match")
	}
	if len(mesh.Indices) == 0 || len(mesh.Indices)%3 != 0 {
		return nil, errors.New("mesh has no valid triangles")
	}
	if uint64(vertexCount) > math.MaxUint32 || uint64(len(mesh.Indices)/3) > math.MaxUint32 {
		return nil, errors.New("mesh exceeds Roblox's 32-bit vertex or face count")
	}
	for _, index := range mesh.Indices {
		if uint64(index) >= uint64(vertexCount) {
			return nil, fmt.Errorf("mesh index %d exceeds vertex count %d", index, vertexCount)
		}
	}

	tangents := robloxTangents(mesh)
	total := len(robloxMeshVersion) + robloxMeshHeaderLen + vertexCount*robloxVertexLen + len(mesh.Indices)/3*robloxFaceLen
	out := bytes.NewBuffer(make([]byte, 0, total))
	_, _ = out.WriteString(robloxMeshVersion)
	_ = binary.Write(out, binary.LittleEndian, uint16(robloxMeshHeaderLen))
	_ = out.WriteByte(robloxVertexLen)
	_ = out.WriteByte(robloxFaceLen)
	_ = binary.Write(out, binary.LittleEndian, uint32(vertexCount))
	_ = binary.Write(out, binary.LittleEndian, uint32(len(mesh.Indices)/3))
	for vertex := 0; vertex < vertexCount; vertex++ {
		for axis := 0; axis < 3; axis++ {
			_ = binary.Write(out, binary.LittleEndian, mesh.Positions[vertex*3+axis])
		}
		for axis := 0; axis < 3; axis++ {
			_ = binary.Write(out, binary.LittleEndian, mesh.Normals[vertex*3+axis])
		}
		_ = binary.Write(out, binary.LittleEndian, mesh.TexCoords[vertex*2])
		_ = binary.Write(out, binary.LittleEndian, mesh.TexCoords[vertex*2+1])
		_, _ = out.Write(tangents[vertex][:])
		_ = binary.Write(out, binary.LittleEndian, uint32(math.MaxUint32))
	}
	for _, index := range mesh.Indices {
		_ = binary.Write(out, binary.LittleEndian, index)
	}
	if out.Len() != total {
		return nil, fmt.Errorf("encoded Roblox mesh has %d bytes, expected %d", out.Len(), total)
	}
	return out.Bytes(), nil
}

// Roblox stores tangent components as biased bytes: -1 => 0, 0 => 127,
// +1 => 254. The fourth component is the bitangent handedness.
func robloxTangents(mesh Mesh) [][4]byte {
	count := len(mesh.Positions) / 3
	tangentSums := make([][3]float64, count)
	bitangentSums := make([][3]float64, count)
	for face := 0; face < len(mesh.Indices); face += 3 {
		i0, i1, i2 := int(mesh.Indices[face]), int(mesh.Indices[face+1]), int(mesh.Indices[face+2])
		p0, p1, p2 := meshPosition(mesh, i0), meshPosition(mesh, i1), meshPosition(mesh, i2)
		uv0, uv1, uv2 := meshUV(mesh, i0), meshUV(mesh, i1), meshUV(mesh, i2)
		e1, e2 := sub3(p1, p0), sub3(p2, p0)
		du1, dv1 := uv1[0]-uv0[0], uv1[1]-uv0[1]
		du2, dv2 := uv2[0]-uv0[0], uv2[1]-uv0[1]
		denominator := du1*dv2 - du2*dv1
		var tangent, bitangent [3]float64
		if math.Abs(denominator) > 1e-12 {
			r := 1 / denominator
			for axis := 0; axis < 3; axis++ {
				tangent[axis] = (e1[axis]*dv2 - e2[axis]*dv1) * r
				bitangent[axis] = (e2[axis]*du1 - e1[axis]*du2) * r
			}
		}
		for _, index := range []int{i0, i1, i2} {
			for axis := 0; axis < 3; axis++ {
				tangentSums[index][axis] += tangent[axis]
				bitangentSums[index][axis] += bitangent[axis]
			}
		}
	}

	encoded := make([][4]byte, count)
	for vertex := 0; vertex < count; vertex++ {
		normal := normalize3(meshNormal(mesh, vertex))
		tangent := tangentSums[vertex]
		// Gram-Schmidt keeps the tangent perpendicular to the normal.
		dot := dot3(normal, tangent)
		for axis := 0; axis < 3; axis++ {
			tangent[axis] -= normal[axis] * dot
		}
		tangent = normalize3(tangent)
		if length3(tangent) == 0 {
			tangent = fallbackTangent(normal)
		}
		handedness := 1.0
		if dot3(cross3(normal, tangent), bitangentSums[vertex]) < 0 {
			handedness = -1
		}
		encoded[vertex] = [4]byte{
			encodeRobloxUnit(tangent[0]), encodeRobloxUnit(tangent[1]),
			encodeRobloxUnit(tangent[2]), encodeRobloxUnit(handedness),
		}
	}
	return encoded
}

func meshPosition(mesh Mesh, index int) [3]float64 {
	return [3]float64{float64(mesh.Positions[index*3]), float64(mesh.Positions[index*3+1]), float64(mesh.Positions[index*3+2])}
}
func meshNormal(mesh Mesh, index int) [3]float64 {
	return [3]float64{float64(mesh.Normals[index*3]), float64(mesh.Normals[index*3+1]), float64(mesh.Normals[index*3+2])}
}
func meshUV(mesh Mesh, index int) [2]float64 {
	return [2]float64{float64(mesh.TexCoords[index*2]), float64(mesh.TexCoords[index*2+1])}
}
func sub3(a, b [3]float64) [3]float64 {
	return [3]float64{a[0] - b[0], a[1] - b[1], a[2] - b[2]}
}
func dot3(a, b [3]float64) float64 { return a[0]*b[0] + a[1]*b[1] + a[2]*b[2] }
func cross3(a, b [3]float64) [3]float64 {
	return [3]float64{a[1]*b[2] - a[2]*b[1], a[2]*b[0] - a[0]*b[2], a[0]*b[1] - a[1]*b[0]}
}
func length3(v [3]float64) float64 { return math.Sqrt(dot3(v, v)) }
func normalize3(v [3]float64) [3]float64 {
	length := length3(v)
	if length == 0 {
		return [3]float64{}
	}
	return [3]float64{v[0] / length, v[1] / length, v[2] / length}
}
func fallbackTangent(normal [3]float64) [3]float64 {
	axis := [3]float64{1, 0, 0}
	if math.Abs(normal[0]) > 0.9 {
		axis = [3]float64{0, 1, 0}
	}
	return normalize3(cross3(axis, normal))
}
func encodeRobloxUnit(value float64) byte {
	value = math.Max(-1, math.Min(1, value))
	return byte(math.Round((value + 1) * 127))
}
