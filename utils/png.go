package utils

import (
	"fmt"
	"image"
	"image/png"
	"io"
	"os"
)

const (
	// MaxTextureDimension rejects forged PNG headers before the decoder can
	// allocate an unreasonable image. The pixel cap is the primary memory
	// bound; the side cap also rejects pathological one-pixel-wide images.
	MaxTextureDimension = 8192
	MaxTexturePixels    = 16 * 1024 * 1024
)

// DecodeTexturePNG validates dimensions from the PNG header before decoding
// pixel storage. It is intended for untrusted texture-pack files.
func DecodeTexturePNG(path string) (image.Image, error) {
	return DecodeTexturePNGWithBudget(path, MaxTextureDimension, MaxTexturePixels)
}

// DecodeTexturePNGWithBudget behaves like DecodeTexturePNG but with caller-
// supplied dimension and pixel limits, for the rare texture-pack inputs
// (such as a multi-face custom-sky sheet) that are legitimately larger than
// a single game texture while still needing a bound against decompression
// bombs.
func DecodeTexturePNGWithBudget(path string, maxDimension int, maxPixels uint64) (image.Image, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	config, err := png.DecodeConfig(file)
	if err != nil {
		return nil, fmt.Errorf("decode PNG header %q: %w", path, err)
	}
	if config.Width <= 0 || config.Height <= 0 ||
		config.Width > maxDimension || config.Height > maxDimension ||
		uint64(config.Width)*uint64(config.Height) > maxPixels {
		return nil, fmt.Errorf(
			"PNG %q is %dx%d; limit is %d pixels and %d pixels per side",
			path, config.Width, config.Height, maxPixels, maxDimension,
		)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("rewind PNG %q: %w", path, err)
	}
	img, err := png.Decode(file)
	if err != nil {
		return nil, fmt.Errorf("decode PNG %q: %w", path, err)
	}
	return img, nil
}
