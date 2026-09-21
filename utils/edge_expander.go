package utils

import (
	"image"
	"image/color"
	"image/draw"
	"math"
)

const (
	// EdgeExpandedTextureSize and EdgeExpansionMaxDistance intentionally match
	// the original Python edge-expander values.
	EdgeExpandedTextureSize  = 512
	EdgeExpansionMaxDistance = 48
)

// EdgeExpand returns the edge-expanded version used for a Roblox *Texture.
// VPImage uploads should continue using the original, unmodified PNG.
//
// The behavior mirrors the supplied Python implementation: resize to 512x512
// using nearest-neighbor sampling, find the nearest non-transparent pixel with
// a four-direction breadth-first search, propagate its RGB up to Manhattan
// distance 48, and apply a squared alpha falloff.
func EdgeExpand(img image.Image) *image.NRGBA {
	return edgeExpand(img, EdgeExpandedTextureSize, EdgeExpansionMaxDistance)
}

// ResizeTexture returns the non-expanded 512x512 nearest-neighbor version used
// for a Roblox *VPImage upload.
func ResizeTexture(img image.Image) *image.NRGBA {
	return resizeNearestNRGBA(img, EdgeExpandedTextureSize, EdgeExpandedTextureSize)
}

// ResizeSkyFace returns the sliced custom-sky face converted to NRGBA with
// no resizing at all - kept at whatever native resolution the source sheet's
// tiles actually are (e.g. 2048x2048 for a 6144x4096 sheet), since any
// scaling in either direction loses quality Minecraft's original art had.
func ResizeSkyFace(img image.Image) *image.NRGBA {
	if img == nil {
		return image.NewNRGBA(image.Rectangle{})
	}
	if native, ok := img.(*image.NRGBA); ok {
		return native
	}
	bounds := img.Bounds()
	native := image.NewNRGBA(bounds)
	draw.Draw(native, bounds, img, bounds.Min, draw.Src)
	return native
}

func edgeExpand(img image.Image, size, maxDistance int) *image.NRGBA {
	if size <= 0 {
		return image.NewNRGBA(image.Rectangle{})
	}
	// The pipeline normally passes an already normalized image. Reuse it
	// directly: edge expansion only reads the source, and avoiding another full
	// 512x512 conversion saves work for every generated texture.
	source, reusable := img.(*image.NRGBA)
	if !reusable || source.Bounds() != image.Rect(0, 0, size, size) {
		source = resizeNearestNRGBA(img, size, size)
	}
	output := image.NewNRGBA(source.Bounds())
	if maxDistance <= 0 {
		copy(output.Pix, source.Pix)
		return output
	}

	pixelCount := size * size
	const unvisited = int32(math.MaxInt32)
	distance := make([]int32, pixelCount)
	near := make([]int32, pixelCount)
	queue := make([]int32, 0, pixelCount)
	for index := range pixelCount {
		distance[index] = unvisited
		near[index] = -1
		if source.Pix[index*4+3] > 0 {
			distance[index] = 0
			near[index] = int32(index)
			queue = append(queue, int32(index))
		}
	}

	// Neighbor order is right, left, down, up, matching the supplied deque BFS.
	for head := 0; head < len(queue); head++ {
		current := int(queue[head])
		currentDistance := distance[current]
		if currentDistance >= int32(maxDistance) {
			continue
		}
		x, y := current%size, current/size
		if x+1 < size {
			expandEdgePixel(current+1, current, currentDistance, distance, near, &queue)
		}
		if x > 0 {
			expandEdgePixel(current-1, current, currentDistance, distance, near, &queue)
		}
		if y+1 < size {
			expandEdgePixel(current+size, current, currentDistance, distance, near, &queue)
		}
		if y > 0 {
			expandEdgePixel(current-size, current, currentDistance, distance, near, &queue)
		}
	}

	inverseDistance := 1.0 / float64(maxDistance)
	for index, pixelDistance := range distance {
		if pixelDistance > int32(maxDistance) || near[index] < 0 {
			continue
		}
		sourceOffset := int(near[index]) * 4
		fade := math.Max(0, float64(maxDistance-int(pixelDistance))*inverseDistance)
		fade *= fade
		alpha := int(fade*float64(source.Pix[sourceOffset+3]) + 0.5)
		if alpha <= 0 {
			continue
		}
		outputOffset := index * 4
		output.Pix[outputOffset] = source.Pix[sourceOffset]
		output.Pix[outputOffset+1] = source.Pix[sourceOffset+1]
		output.Pix[outputOffset+2] = source.Pix[sourceOffset+2]
		output.Pix[outputOffset+3] = uint8(alpha)
	}
	return output
}

func expandEdgePixel(
	next, current int,
	currentDistance int32,
	distance, near []int32,
	queue *[]int32,
) {
	nextDistance := currentDistance + 1
	if nextDistance >= distance[next] {
		return
	}
	distance[next] = nextDistance
	near[next] = near[current]
	*queue = append(*queue, int32(next))
}

func resizeNearestNRGBA(img image.Image, width, height int) *image.NRGBA {
	destination := image.NewNRGBA(image.Rect(0, 0, width, height))
	if img == nil || width == 0 || height == 0 {
		return destination
	}
	bounds := img.Bounds()
	sourceWidth, sourceHeight := bounds.Dx(), bounds.Dy()
	if sourceWidth == 0 || sourceHeight == 0 {
		return destination
	}
	for y := 0; y < height; y++ {
		// Sampling pixel centers reproduces Pillow's nearest-neighbor resizing.
		sourceY := bounds.Min.Y + ((2*y+1)*sourceHeight)/(2*height)
		if sourceY >= bounds.Max.Y {
			sourceY = bounds.Max.Y - 1
		}
		for x := 0; x < width; x++ {
			sourceX := bounds.Min.X + ((2*x+1)*sourceWidth)/(2*width)
			if sourceX >= bounds.Max.X {
				sourceX = bounds.Max.X - 1
			}
			pixel := color.NRGBAModel.Convert(img.At(sourceX, sourceY)).(color.NRGBA)
			destination.SetNRGBA(x, y, pixel)
		}
	}
	return destination
}
