// Package utils provides AutoPack's reusable asset-processing helpers.
package utils

import (
	"errors"
	"fmt"
	"image"
	"math"
)

// Config mirrors the meaningful constants and transforms in AutoPack.txt.
type Config struct {
	PlaneSize      float64
	Thickness      float64
	AlphaThreshold int
	RotateX        float64
	RotateY        float64
	RotateZ        float64
	Center         bool
}

func DefaultConfig() Config {
	return Config{
		PlaneSize: 2.0,
		Thickness: 0.07,
		// Match the original mesh generator: alpha values below 10 are
		// transparent. BuildGreedyMesh can raise this threshold automatically
		// when a broken exporter fills the canvas with low-alpha noise.
		AlphaThreshold: 9,
		RotateX:        90,
		RotateY:        -45,
		RotateZ:        0,
		Center:         true,
	}
}

const maxBackgroundNoiseAlpha = 32

// DetectBackgroundAlphaNoiseThreshold detects the specific broken-export pattern where
// nearly every image pixel has a tiny non-zero alpha value. It is intended for
// generated texture copies and geometry masks. Normal sprites with genuinely
// transparent backgrounds return zero.
func DetectBackgroundAlphaNoiseThreshold(img image.Image) int {
	if img == nil {
		return 0
	}
	bounds := img.Bounds()
	total := bounds.Dx() * bounds.Dy()
	if total <= 0 {
		return 0
	}
	lowAlpha := 0
	borderLowAlpha := 0
	borderPixels := 0
	threshold := 0
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			_, _, _, alpha16 := img.At(x, y).RGBA()
			alpha := int(alpha16 >> 8)
			if alpha > 0 && alpha <= maxBackgroundNoiseAlpha {
				lowAlpha++
				if alpha > threshold {
					threshold = alpha
				}
			}
			if x == bounds.Min.X || x == bounds.Max.X-1 || y == bounds.Min.Y || y == bounds.Max.Y-1 {
				borderPixels++
				if alpha > 0 && alpha <= maxBackgroundNoiseAlpha {
					borderLowAlpha++
				}
			}
		}
	}
	// A broken canvas covers a large part of the image and reaches most of its
	// outside border. Ordinary antialiasing only hugs the item silhouette and
	// therefore cannot satisfy both conditions.
	if threshold == 0 || lowAlpha*100 < total*20 || borderLowAlpha*100 < borderPixels*60 {
		return 0
	}
	return threshold
}

// RemoveBackgroundAlphaNoise clears only canvas-wide low-alpha exporter noise
// from an already copied NRGBA texture. It returns the detected threshold, or
// zero when the image is a normal sprite and was left byte-for-byte unchanged.
func RemoveBackgroundAlphaNoise(img *image.NRGBA) int {
	threshold := DetectBackgroundAlphaNoiseThreshold(img)
	if threshold == 0 {
		return 0
	}
	for offset := 0; offset+3 < len(img.Pix); offset += 4 {
		if alpha := int(img.Pix[offset+3]); alpha > 0 && alpha <= threshold {
			img.Pix[offset] = 0
			img.Pix[offset+1] = 0
			img.Pix[offset+2] = 0
			img.Pix[offset+3] = 0
		}
	}
	return threshold
}

// RemoveTinyAlphaIslands clears detached alpha components smaller than one
// percent of the largest visible component. Eight-way connectivity preserves
// normal pixel-art diagonals, while removing isolated exporter artifacts that
// become large floating blocks after nearest-neighbor upscaling.
func RemoveTinyAlphaIslands(img *image.NRGBA, alphaThreshold int) int {
	if img == nil || alphaThreshold < 0 || alphaThreshold > 255 {
		return 0
	}
	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width == 0 || height == 0 {
		return 0
	}
	visible := func(index int) bool {
		x := bounds.Min.X + index%width
		y := bounds.Min.Y + index/width
		return int(img.Pix[img.PixOffset(x, y)+3]) > alphaThreshold
	}

	visited := make([]bool, width*height)
	components := make([][]int, 0, 4)
	queue := make([]int, 0, 256)
	for start := range visited {
		if visited[start] || !visible(start) {
			continue
		}
		visited[start] = true
		queue = append(queue[:0], start)
		component := make([]int, 0, 256)
		for len(queue) != 0 {
			current := queue[0]
			queue = queue[1:]
			component = append(component, current)
			x, y := current%width, current/width
			for dy := -1; dy <= 1; dy++ {
				for dx := -1; dx <= 1; dx++ {
					if dx == 0 && dy == 0 {
						continue
					}
					nx, ny := x+dx, y+dy
					if nx < 0 || nx >= width || ny < 0 || ny >= height {
						continue
					}
					neighbor := ny*width + nx
					if visited[neighbor] || !visible(neighbor) {
						continue
					}
					visited[neighbor] = true
					queue = append(queue, neighbor)
				}
			}
		}
		components = append(components, component)
	}
	if len(components) < 2 {
		return 0
	}

	largest := 0
	for _, component := range components {
		if len(component) > largest {
			largest = len(component)
		}
	}
	removed := 0
	for _, component := range components {
		if len(component)*100 >= largest {
			continue
		}
		for _, index := range component {
			x := bounds.Min.X + index%width
			y := bounds.Min.Y + index/width
			offset := img.PixOffset(x, y)
			img.Pix[offset] = 0
			img.Pix[offset+1] = 0
			img.Pix[offset+2] = 0
			img.Pix[offset+3] = 0
			removed++
		}
	}
	return removed
}

func (c Config) Validate() error {
	if !finite(c.PlaneSize) || c.PlaneSize <= 0 {
		return errors.New("plane-size must be finite and greater than zero")
	}
	if !finite(c.Thickness) || c.Thickness <= 0 {
		return errors.New("thickness must be finite and greater than zero")
	}
	if c.AlphaThreshold < 0 || c.AlphaThreshold > 255 {
		return errors.New("alpha-threshold must be between 0 and 255")
	}
	if !finite(c.RotateX) || !finite(c.RotateY) || !finite(c.RotateZ) {
		return errors.New("rotations must be finite")
	}
	return nil
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

type Mesh struct {
	Positions []float32
	Normals   []float32
	TexCoords []float32
	Indices   []uint32
	// AlphaBlend is enabled for antialiased source pixels so their RGB values
	// are not rendered as an opaque dark fringe.
	AlphaBlend bool
}

type MeshStats struct {
	GridWidth         int
	GridHeight        int
	OpaqueCells       int
	Quads             int
	AlphaThreshold    int
	BlenderDimensions [3]float64
}

type vec2 struct{ u, v float64 }
type vec3 struct{ x, y, z float64 }

type meshBuilder struct {
	mesh       Mesh
	transform  func(vec3) vec3
	min        vec3
	max        vec3
	haveBounds bool
}

// BuildGreedyMesh turns the sampled alpha mask into one closed slab. Front and
// back rectangles are greedily combined in 2D; each exposed side direction is
// combined into the longest possible run. There are no faces between cells.
func BuildGreedyMesh(img image.Image, cfg Config) (Mesh, MeshStats, error) {
	if err := cfg.Validate(); err != nil {
		return Mesh{}, MeshStats{}, err
	}
	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width == 0 || height == 0 {
		return Mesh{}, MeshStats{}, errors.New("input image is empty")
	}

	gridWidth, gridHeight := width, height
	maxInt := int(^uint(0) >> 1)
	if gridWidth > maxInt/gridHeight {
		return Mesh{}, MeshStats{}, fmt.Errorf("image dimensions %dx%d are too large", width, height)
	}

	effectiveThreshold := cfg.AlphaThreshold
	if detected := DetectBackgroundAlphaNoiseThreshold(img); detected > effectiveThreshold {
		effectiveThreshold = detected
	}
	mask := make([]bool, gridWidth*gridHeight)
	opaque := 0
	hasPartialAlpha := false
	for y := 0; y < gridHeight; y++ {
		py := bounds.Min.Y + int(float64(y)*float64(height)/float64(gridHeight))
		if py >= bounds.Max.Y {
			py = bounds.Max.Y - 1
		}
		for x := 0; x < gridWidth; x++ {
			px := bounds.Min.X + int(float64(x)*float64(width)/float64(gridWidth))
			if px >= bounds.Max.X {
				px = bounds.Max.X - 1
			}
			_, _, _, a := img.At(px, py).RGBA()
			alpha := int(a >> 8)
			if alpha > effectiveThreshold && alpha < 255 {
				hasPartialAlpha = true
			}
			if alpha > effectiveThreshold {
				mask[y*gridWidth+x] = true
				opaque++
			}
		}
	}
	if opaque == 0 {
		return Mesh{}, MeshStats{}, fmt.Errorf("no cells are visible at alpha threshold %d", effectiveThreshold)
	}

	minX, minY, maxX, maxY := gridWidth, gridHeight, -1, -1
	for y := 0; y < gridHeight; y++ {
		for x := 0; x < gridWidth; x++ {
			if !mask[y*gridWidth+x] {
				continue
			}
			if x < minX {
				minX = x
			}
			if x > maxX {
				maxX = x
			}
			if y < minY {
				minY = y
			}
			if y > maxY {
				maxY = y
			}
		}
	}
	// PlaneSize is the image width, which is identical to the old 2x2 behavior
	// for square art. One shared pixel size preserves non-square aspect ratios.
	pixel := cfg.PlaneSize / float64(gridWidth)
	centerX, centerY := 0.0, 0.0
	if cfg.Center {
		left := (float64(minX) - float64(gridWidth)/2) * pixel
		right := (float64(maxX+1) - float64(gridWidth)/2) * pixel
		top := (float64(gridHeight)/2 - float64(minY)) * pixel
		bottom := (float64(gridHeight)/2 - float64(maxY+1)) * pixel
		centerX = (left + right) / 2
		centerY = (top + bottom) / 2
	}
	xCoord := func(gridX int) float64 {
		return (float64(gridX)-float64(gridWidth)/2)*pixel - centerX
	}
	yCoord := func(gridY int) float64 {
		return (float64(gridHeight)/2-float64(gridY))*pixel - centerY
	}
	uCoord := func(gridX int) float64 { return float64(gridX) / float64(gridWidth) }
	vCoord := func(gridY int) float64 { return float64(gridY) / float64(gridHeight) }

	b := meshBuilder{
		mesh:      Mesh{AlphaBlend: hasPartialAlpha},
		transform: blenderTransform(cfg),
	}
	used := make([]bool, gridWidth*gridHeight)
	for y := 0; y < gridHeight; y++ {
		for x := 0; x < gridWidth; x++ {
			i := y*gridWidth + x
			if !mask[i] || used[i] {
				continue
			}
			w := 1
			for x+w < gridWidth && mask[y*gridWidth+x+w] && !used[y*gridWidth+x+w] {
				w++
			}
			h := 1
			for y+h < gridHeight {
				ok := true
				for dx := 0; dx < w; dx++ {
					j := (y+h)*gridWidth + x + dx
					if !mask[j] || used[j] {
						ok = false
						break
					}
				}
				if !ok {
					break
				}
				h++
			}
			for dy := 0; dy < h; dy++ {
				for dx := 0; dx < w; dx++ {
					used[(y+dy)*gridWidth+x+dx] = true
				}
			}

			x0, x1 := xCoord(x), xCoord(x+w)
			y0, y1 := yCoord(y), yCoord(y+h)
			u0, u1 := uCoord(x), uCoord(x+w)
			v0, v1 := vCoord(y), vCoord(y+h)
			// The Python face winding points toward local -Z.
			b.addQuad(
				[4]vec3{{x0, y0, 0}, {x1, y0, 0}, {x1, y1, 0}, {x0, y1, 0}},
				[4]vec2{{u0, v0}, {u1, v0}, {u1, v1}, {u0, v1}},
			)
			// Reverse winding for the opposite shell while retaining its UVs.
			b.addQuad(
				[4]vec3{{x0, y0, cfg.Thickness}, {x0, y1, cfg.Thickness}, {x1, y1, cfg.Thickness}, {x1, y0, cfg.Thickness}},
				[4]vec2{{u0, v0}, {u0, v1}, {u1, v1}, {u1, v0}},
			)
		}
	}

	edge := func(x, y, dx, dy int) bool {
		if !mask[y*gridWidth+x] {
			return false
		}
		nextX, nextY := x+dx, y+dy
		return nextX < 0 || nextX >= gridWidth || nextY < 0 || nextY >= gridHeight ||
			!mask[nextY*gridWidth+nextX]
	}
	// Top and bottom boundaries, greedily combined left-to-right.
	for y := 0; y < gridHeight; y++ {
		for _, side := range []int{-1, 1} {
			for x := 0; x < gridWidth; {
				if !edge(x, y, 0, side) {
					x++
					continue
				}
				x1 := x + 1
				for x1 < gridWidth && edge(x1, y, 0, side) {
					x1++
				}
				px0, px1 := xCoord(x), xCoord(x1)
				uv0, uv1 := uCoord(x), uCoord(x1)
				if side == -1 { // physical top, outward +Y
					py := yCoord(y)
					v := (float64(y) + 0.5) / float64(gridHeight)
					b.addQuad(
						[4]vec3{{px0, py, 0}, {px0, py, cfg.Thickness}, {px1, py, cfg.Thickness}, {px1, py, 0}},
						[4]vec2{{uv0, v}, {uv0, v}, {uv1, v}, {uv1, v}},
					)
				} else { // physical bottom, outward -Y
					py := yCoord(y + 1)
					v := (float64(y) + 0.5) / float64(gridHeight)
					b.addQuad(
						[4]vec3{{px0, py, 0}, {px1, py, 0}, {px1, py, cfg.Thickness}, {px0, py, cfg.Thickness}},
						[4]vec2{{uv0, v}, {uv1, v}, {uv1, v}, {uv0, v}},
					)
				}
				x = x1
			}
		}
	}
	// Left and right boundaries, greedily combined top-to-bottom.
	for x := 0; x < gridWidth; x++ {
		for _, side := range []int{-1, 1} {
			for y := 0; y < gridHeight; {
				if !edge(x, y, side, 0) {
					y++
					continue
				}
				y1 := y + 1
				for y1 < gridHeight && edge(x, y1, side, 0) {
					y1++
				}
				py0, py1 := yCoord(y), yCoord(y1)
				vv0, vv1 := vCoord(y), vCoord(y1)
				if side == -1 { // outward -X
					px := xCoord(x)
					u := (float64(x) + 0.5) / float64(gridWidth)
					b.addQuad(
						[4]vec3{{px, py0, 0}, {px, py1, 0}, {px, py1, cfg.Thickness}, {px, py0, cfg.Thickness}},
						[4]vec2{{u, vv0}, {u, vv1}, {u, vv1}, {u, vv0}},
					)
				} else { // outward +X
					px := xCoord(x + 1)
					u := (float64(x) + 0.5) / float64(gridWidth)
					b.addQuad(
						[4]vec3{{px, py0, 0}, {px, py0, cfg.Thickness}, {px, py1, cfg.Thickness}, {px, py1, 0}},
						[4]vec2{{u, vv0}, {u, vv0}, {u, vv1}, {u, vv1}},
					)
				}
				y = y1
			}
		}
	}

	dimsGLTF := vec3{b.max.x - b.min.x, b.max.y - b.min.y, b.max.z - b.min.z}
	// Inverse of Blender->glTF (x, y, z) -> (x, z, -y). Dimensions
	// therefore map back as X=x, Y=z, Z=y.
	stats := MeshStats{
		GridWidth:         gridWidth,
		GridHeight:        gridHeight,
		OpaqueCells:       opaque,
		Quads:             len(b.mesh.Indices) / 6,
		AlphaThreshold:    effectiveThreshold,
		BlenderDimensions: [3]float64{dimsGLTF.x, dimsGLTF.z, dimsGLTF.y},
	}
	return b.mesh, stats, nil
}

func (b *meshBuilder) addQuad(p [4]vec3, uv [4]vec2) {
	transformed := [4]vec3{}
	for i := range p {
		transformed[i] = b.transform(p[i])
	}
	n := normalize(cross(sub(transformed[1], transformed[0]), sub(transformed[2], transformed[0])))
	base := uint32(len(b.mesh.Positions) / 3)
	for i, point := range transformed {
		b.mesh.Positions = append(b.mesh.Positions, float32(point.x), float32(point.y), float32(point.z))
		b.mesh.Normals = append(b.mesh.Normals, float32(n.x), float32(n.y), float32(n.z))
		b.mesh.TexCoords = append(b.mesh.TexCoords, float32(uv[i].u), float32(uv[i].v))
		if !b.haveBounds {
			b.min, b.max, b.haveBounds = point, point, true
		} else {
			b.min.x, b.min.y, b.min.z = math.Min(b.min.x, point.x), math.Min(b.min.y, point.y), math.Min(b.min.z, point.z)
			b.max.x, b.max.y, b.max.z = math.Max(b.max.x, point.x), math.Max(b.max.y, point.y), math.Max(b.max.z, point.z)
		}
	}
	b.mesh.Indices = append(b.mesh.Indices, base, base+1, base+2, base, base+2, base+3)
}

func blenderTransform(cfg Config) func(vec3) vec3 {
	rx := cfg.RotateX * math.Pi / 180
	ry := cfg.RotateY * math.Pi / 180
	rz := cfg.RotateZ * math.Pi / 180
	sx, cx := math.Sincos(rx)
	sy, cy := math.Sincos(ry)
	sz, cz := math.Sincos(rz)
	return func(p vec3) vec3 {
		// Blender XYZ Euler: Rz * Ry * Rx.
		y1, z1 := cx*p.y-sx*p.z, sx*p.y+cx*p.z
		x2, z2 := cy*p.x+sy*z1, -sy*p.x+cy*z1
		x3, y3 := cz*x2-sz*y1, sz*x2+cz*y1
		// glTF is Y-up; Blender is Z-up. Blender imports this mapping back
		// to exactly (x3, y3, z2).
		return vec3{x3, z2, -y3}
	}
}

func sub(a, b vec3) vec3 { return vec3{a.x - b.x, a.y - b.y, a.z - b.z} }
func cross(a, b vec3) vec3 {
	return vec3{a.y*b.z - a.z*b.y, a.z*b.x - a.x*b.z, a.x*b.y - a.y*b.x}
}
func normalize(v vec3) vec3 {
	l := math.Sqrt(v.x*v.x + v.y*v.y + v.z*v.z)
	return vec3{v.x / l, v.y / l, v.z / l}
}
