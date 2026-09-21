package utils

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// canonicalSkyEntry is the single sky path AutoPack's pipeline actually looks
// for (see requestedTextures' "sky_cloud_sheet" spec). Any other numbered
// cloud sheet a pack ships (cloud2.png, cloud3.png, ...) is otherwise ignored
// entirely by extraction, so a pack's alternate skies need to be detected and
// offered explicitly instead.
const (
	canonicalSkyEntryMCPatcher = "assets/minecraft/mcpatcher/sky/world0/cloud1.png"
	canonicalSkyEntryOptifine  = "assets/minecraft/optifine/sky/world0/cloud1.png"
)

// skyEntryPattern matches any numbered custom-sky sheet under either
// namespace's world0 folder, case-insensitively, regardless of what (if any)
// folder the pack's ZIP wraps everything in.
var skyEntryPattern = regexp.MustCompile(`(?i)assets/minecraft/(?:mcpatcher|optifine)/sky/world0/cloud(\d+)\.png$`)

// skyThumbnailSize is the square side length, in pixels, of the preview
// crop returned for each detected sky option.
const skyThumbnailSize = 160

// SkyOption is one custom-sky sheet found in an uploaded pack, ready to
// offer as either the pack's primary sky or a separate sky-only pack.
type SkyOption struct {
	// ID is the sky sheet's exact ZIP entry name, used later to pick it
	// back out of the same upload.
	ID string
	// Label is a short, human name for the option, such as "Overworld ·
	// Day" or "Overworld · Sky 4".
	Label string
	// ThumbnailPNG is a small PNG crop of the sheet's front face, for
	// display in a picker.
	ThumbnailPNG []byte
}

// skyLabelForNumber names a numbered custom-sky sheet the way most packs use
// them in practice (cloud1 = the default day sky). Anything past 3 just gets
// a numbered label since there's no fixed convention beyond that.
func skyLabelForNumber(number int) string {
	switch number {
	case 1:
		return "Overworld · Day"
	case 2:
		return "Overworld · Sunset"
	case 3:
		return "Overworld · Night"
	default:
		return fmt.Sprintf("Overworld · Sky %d", number)
	}
}

// SkyLabelForEntry returns the same human label DetectSkyOptions would use
// for a given sky entry's ZIP path, for callers that already know the path
// (such as labeling an extra pack after the fact, once the original upload
// isn't being re-scanned).
func SkyLabelForEntry(entryID string) string {
	submatch := skyEntryPattern.FindStringSubmatch(strings.ReplaceAll(entryID, "\\", "/"))
	if submatch == nil {
		return "Overworld · Sky"
	}
	number, err := strconv.Atoi(submatch[1])
	if err != nil || number < 1 {
		return "Overworld · Sky"
	}
	return skyLabelForNumber(number)
}

// DetectSkyOptions scans a texture-pack ZIP for every numbered custom-sky
// sheet it ships (cloud1.png, cloud2.png, ...) and returns one SkyOption per
// sheet, sorted by number. Packs with at most one such sheet - the normal
// case - return a slice of length 0 or 1, signaling callers not to bother
// asking which sky to use.
func DetectSkyOptions(zipPath string) ([]SkyOption, error) {
	if strings.TrimSpace(zipPath) == "" {
		return nil, errors.New("texture-pack ZIP path is empty")
	}
	archive, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, fmt.Errorf("open texture-pack ZIP %q: %w", zipPath, err)
	}
	defer archive.Close()

	type found struct {
		entry  *zip.File
		number int
	}
	var matches []found
	for _, entry := range archive.File {
		if entry.FileInfo().IsDir() {
			continue
		}
		submatch := skyEntryPattern.FindStringSubmatch(strings.ReplaceAll(entry.Name, "\\", "/"))
		if submatch == nil {
			continue
		}
		number, convErr := strconv.Atoi(submatch[1])
		if convErr != nil || number < 1 {
			continue
		}
		if entry.UncompressedSize64 > maxPackFileSize {
			continue
		}
		matches = append(matches, found{entry: entry, number: number})
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].number < matches[j].number })

	options := make([]SkyOption, 0, len(matches))
	for _, match := range matches {
		thumbnail, thumbErr := skySheetThumbnail(match.entry)
		if thumbErr != nil {
			// A sheet that can't be decoded as a proper sky grid just isn't
			// offered as an option instead of failing the whole scan.
			continue
		}
		options = append(options, SkyOption{
			ID:           match.entry.Name,
			Label:        skyLabelForNumber(match.number),
			ThumbnailPNG: thumbnail,
		})
	}
	return options, nil
}

// skySheetThumbnail decodes a 3x2 custom-sky sheet and crops its front face
// (the same face cloudSkyFaceOrder maps to index 0) down to a small square
// PNG suitable for a picker thumbnail.
func skySheetThumbnail(entry *zip.File) ([]byte, error) {
	reader, err := entry.Open()
	if err != nil {
		return nil, fmt.Errorf("open sky sheet %q: %w", entry.Name, err)
	}
	defer reader.Close()

	sheet, _, err := image.Decode(io.LimitReader(reader, int64(maxPackFileSize)+1))
	if err != nil {
		return nil, fmt.Errorf("decode sky sheet %q: %w", entry.Name, err)
	}
	bounds := sheet.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width%3 != 0 || height%2 != 0 || width/3 != height/2 {
		return nil, fmt.Errorf("sky sheet %q is %dx%d, expected a 3x2 grid of equal square faces", entry.Name, width, height)
	}
	tile := width / 3
	face := image.Rect(bounds.Min.X, bounds.Min.Y, bounds.Min.X+tile, bounds.Min.Y+tile)

	thumbnail := image.NewNRGBA(image.Rect(0, 0, skyThumbnailSize, skyThumbnailSize))
	for y := 0; y < skyThumbnailSize; y++ {
		sourceY := face.Min.Y + y*tile/skyThumbnailSize
		for x := 0; x < skyThumbnailSize; x++ {
			sourceX := face.Min.X + x*tile/skyThumbnailSize
			thumbnail.Set(x, y, sheet.At(sourceX, sourceY))
		}
	}

	var buffer bytes.Buffer
	if err := png.Encode(&buffer, thumbnail); err != nil {
		return nil, fmt.Errorf("encode sky sheet thumbnail %q: %w", entry.Name, err)
	}
	return buffer.Bytes(), nil
}

// RewritePrimarySky produces a copy of a pack's ZIP where the chosen sky
// entry (as returned by DetectSkyOptions) has been placed at the one path
// AutoPack's pipeline actually reads for a pack's sky
// (.../mcpatcher/sky/world0/cloud1.png and its OptiFine equivalent), so a
// pack can use any of its alternate skies as its primary one. The caller is
// responsible for calling the returned cleanup func once done with the
// path. If chosenID is empty, the original zipPath is returned unchanged
// with a no-op cleanup.
func RewritePrimarySky(zipPath, chosenID string) (string, func(), error) {
	noop := func() {}
	if strings.TrimSpace(chosenID) == "" {
		return zipPath, noop, nil
	}
	archive, err := zip.OpenReader(zipPath)
	if err != nil {
		return "", noop, fmt.Errorf("open texture-pack ZIP %q: %w", zipPath, err)
	}
	defer archive.Close()

	var chosenBytes []byte
	found := false
	for _, entry := range archive.File {
		if entry.Name != chosenID {
			continue
		}
		reader, openErr := entry.Open()
		if openErr != nil {
			return "", noop, fmt.Errorf("open chosen sky entry %q: %w", chosenID, openErr)
		}
		chosenBytes, err = io.ReadAll(io.LimitReader(reader, int64(maxPackFileSize)+1))
		closeErr := reader.Close()
		if err != nil {
			return "", noop, fmt.Errorf("read chosen sky entry %q: %w", chosenID, err)
		}
		if closeErr != nil {
			return "", noop, fmt.Errorf("close chosen sky entry %q: %w", chosenID, closeErr)
		}
		found = true
		break
	}
	if !found {
		return "", noop, fmt.Errorf("chosen sky entry %q was not found in the uploaded ZIP", chosenID)
	}

	output, err := os.CreateTemp("", "autopack-primary-sky-*.zip")
	if err != nil {
		return "", noop, fmt.Errorf("stage rewritten texture-pack ZIP: %w", err)
	}
	cleanup := func() { _ = os.Remove(output.Name()) }
	writer := zip.NewWriter(output)

	skipCanonical := map[string]bool{
		canonicalSkyEntryMCPatcher: true,
		canonicalSkyEntryOptifine:  true,
	}
	for _, entry := range archive.File {
		if skipCanonical[strings.ReplaceAll(entry.Name, "\\", "/")] {
			continue
		}
		if copyErr := copyZipEntry(writer, entry); copyErr != nil {
			_ = writer.Close()
			_ = output.Close()
			cleanup()
			return "", noop, copyErr
		}
	}
	for _, canonicalPath := range []string{canonicalSkyEntryMCPatcher, canonicalSkyEntryOptifine} {
		entryWriter, createErr := writer.Create(canonicalPath)
		if createErr != nil {
			_ = writer.Close()
			_ = output.Close()
			cleanup()
			return "", noop, fmt.Errorf("write %q into rewritten texture-pack ZIP: %w", canonicalPath, createErr)
		}
		if _, writeErr := entryWriter.Write(chosenBytes); writeErr != nil {
			_ = writer.Close()
			_ = output.Close()
			cleanup()
			return "", noop, fmt.Errorf("write %q into rewritten texture-pack ZIP: %w", canonicalPath, writeErr)
		}
	}
	if err := writer.Close(); err != nil {
		_ = output.Close()
		cleanup()
		return "", noop, fmt.Errorf("finish rewritten texture-pack ZIP: %w", err)
	}
	if err := output.Close(); err != nil {
		cleanup()
		return "", noop, fmt.Errorf("close rewritten texture-pack ZIP: %w", err)
	}
	return output.Name(), cleanup, nil
}

// BuildSingleSkyZip produces a minimal ZIP containing only the chosen sky
// entry, written at the canonical path AutoPack's pipeline reads. Running
// this through the normal pipeline yields a "sky-only" pack: every item
// texture reports missing (falling back to Roblox defaults) and only the
// sky is set, which is exactly what an "extra pack" for one of a pack's
// alternate skies should be.
func BuildSingleSkyZip(zipPath, entryID string) (string, func(), error) {
	noop := func() {}
	archive, err := zip.OpenReader(zipPath)
	if err != nil {
		return "", noop, fmt.Errorf("open texture-pack ZIP %q: %w", zipPath, err)
	}
	defer archive.Close()

	var chosenBytes []byte
	found := false
	for _, entry := range archive.File {
		if entry.Name != entryID {
			continue
		}
		reader, openErr := entry.Open()
		if openErr != nil {
			return "", noop, fmt.Errorf("open sky entry %q: %w", entryID, openErr)
		}
		chosenBytes, err = io.ReadAll(io.LimitReader(reader, int64(maxPackFileSize)+1))
		closeErr := reader.Close()
		if err != nil {
			return "", noop, fmt.Errorf("read sky entry %q: %w", entryID, err)
		}
		if closeErr != nil {
			return "", noop, fmt.Errorf("close sky entry %q: %w", entryID, closeErr)
		}
		found = true
		break
	}
	if !found {
		return "", noop, fmt.Errorf("sky entry %q was not found in the uploaded ZIP", entryID)
	}

	output, err := os.CreateTemp("", "autopack-extra-sky-*.zip")
	if err != nil {
		return "", noop, fmt.Errorf("stage sky-only texture-pack ZIP: %w", err)
	}
	cleanup := func() { _ = os.Remove(output.Name()) }
	writer := zip.NewWriter(output)
	entryWriter, createErr := writer.Create(canonicalSkyEntryMCPatcher)
	if createErr != nil {
		_ = writer.Close()
		_ = output.Close()
		cleanup()
		return "", noop, fmt.Errorf("write sky-only texture-pack ZIP: %w", createErr)
	}
	if _, writeErr := entryWriter.Write(chosenBytes); writeErr != nil {
		_ = writer.Close()
		_ = output.Close()
		cleanup()
		return "", noop, fmt.Errorf("write sky-only texture-pack ZIP: %w", writeErr)
	}
	if err := writer.Close(); err != nil {
		_ = output.Close()
		cleanup()
		return "", noop, fmt.Errorf("finish sky-only texture-pack ZIP: %w", err)
	}
	if err := output.Close(); err != nil {
		cleanup()
		return "", noop, fmt.Errorf("close sky-only texture-pack ZIP: %w", err)
	}
	return output.Name(), cleanup, nil
}

func copyZipEntry(writer *zip.Writer, entry *zip.File) error {
	header := entry.FileHeader
	entryWriter, err := writer.CreateHeader(&header)
	if err != nil {
		return fmt.Errorf("copy texture-pack ZIP entry %q: %w", entry.Name, err)
	}
	reader, err := entry.Open()
	if err != nil {
		return fmt.Errorf("open texture-pack ZIP entry %q: %w", entry.Name, err)
	}
	defer reader.Close()
	if _, err := io.Copy(entryWriter, reader); err != nil {
		return fmt.Errorf("copy texture-pack ZIP entry %q: %w", entry.Name, err)
	}
	return nil
}
