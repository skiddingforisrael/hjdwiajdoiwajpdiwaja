package utils

import (
	"archive/zip"
	"errors"
	"fmt" // already used below by panorama() and error formatting
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
)

const (
	maxPackFiles         = 4_096
	maxPackFileSize      = 32 << 20  // 32 MiB per entry.
	maxPackExtractedSize = 256 << 20 // 256 MiB total declared expansion.
)

// TexturePack is a temporarily extracted Minecraft 1.8.9 texture pack.
// Textures maps stable names such as "diamond_sword", "wool_red",
// "jump_pot", and "speed_pot" to absolute PNG paths inside TempDir.
type TexturePack struct {
	TempDir  string
	Textures map[string]string
	Missing  []string
}

// Cleanup removes the pack's temporary directory. All texture paths become
// invalid after this call.
func (p *TexturePack) Cleanup() error {
	if p == nil || p.TempDir == "" {
		return nil
	}
	if err := os.RemoveAll(p.TempDir); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clean texture-pack temporary directory: %w", err)
	}
	p.TempDir = ""
	p.Textures = nil
	p.Missing = nil
	return nil
}

// UnzipTexturePack safely extracts the relevant contents of a Minecraft 1.8.9
// texture-pack ZIP to a private temporary directory and discovers the textures
// AutoPack uses. Unrelated pack files are validated but never decompressed.
//
// Missing textures are reported in TexturePack.Missing instead of causing an
// error because resource packs often override only some vanilla assets. The
// temporary directory is automatically removed if extraction fails.
func UnzipTexturePack(zipPath string) (_ *TexturePack, err error) {
	if strings.TrimSpace(zipPath) == "" {
		return nil, errors.New("texture-pack ZIP path is empty")
	}
	archive, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, fmt.Errorf("open texture-pack ZIP %q: %w", zipPath, err)
	}
	defer archive.Close()
	if len(archive.File) > maxPackFiles {
		return nil, fmt.Errorf("texture-pack ZIP has %d entries; limit is %d", len(archive.File), maxPackFiles)
	}

	tempDir, err := os.MkdirTemp("", "autopack-textures-*")
	if err != nil {
		return nil, fmt.Errorf("create texture-pack temporary directory: %w", err)
	}
	result := &TexturePack{TempDir: tempDir, Textures: make(map[string]string)}
	defer func() {
		if err != nil {
			_ = result.Cleanup()
		}
	}()
	if err = extractZIP(archive.File, tempDir); err != nil {
		return nil, err
	}
	if err = discoverPackTextures(result); err != nil {
		return nil, err
	}
	return result, nil
}

func extractZIP(files []*zip.File, destination string) error {
	type extractionJob struct {
		entry  *zip.File
		target string
	}
	jobs := make([]extractionJob, 0, len(files))
	wanted := requestedTexturePaths()
	var total uint64
	for _, entry := range files {
		if entry.FileInfo().Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("texture-pack ZIP contains unsupported symbolic link %q", entry.Name)
		}
		if entry.UncompressedSize64 > maxPackFileSize {
			return fmt.Errorf("texture-pack entry %q is larger than %d bytes", entry.Name, maxPackFileSize)
		}
		if total > maxPackExtractedSize || entry.UncompressedSize64 > maxPackExtractedSize-total {
			return fmt.Errorf("texture-pack ZIP expands beyond %d bytes", maxPackExtractedSize)
		}
		total += entry.UncompressedSize64

		relative, err := safeZIPPath(entry.Name)
		if err != nil {
			return err
		}
		if relative == "" {
			continue
		}
		target := filepath.Join(destination, relative)
		if entry.FileInfo().IsDir() {
			continue
		}
		if !entry.Mode().IsRegular() {
			return fmt.Errorf("texture-pack ZIP entry %q is not a regular file", entry.Name)
		}
		if !isRequestedTexturePath(filepath.ToSlash(relative), wanted) {
			continue
		}
		jobs = append(jobs, extractionJob{entry: entry, target: target})
	}

	workers := runtime.GOMAXPROCS(0)
	if workers < 1 {
		workers = 1
	}
	if workers > 32 {
		workers = 32
	}
	if workers > len(jobs) {
		workers = len(jobs)
	}
	if workers == 0 {
		return nil
	}
	queue := make(chan extractionJob, len(jobs))
	for _, job := range jobs {
		queue <- job
	}
	close(queue)
	done := make(chan struct{})
	var cancelOnce sync.Once
	var firstErr error
	var wait sync.WaitGroup
	wait.Add(workers)
	for worker := 0; worker < workers; worker++ {
		go func() {
			defer wait.Done()
			for {
				select {
				case <-done:
					return
				case job, ok := <-queue:
					if !ok {
						return
					}
					if err := os.MkdirAll(filepath.Dir(job.target), 0o755); err != nil {
						err = fmt.Errorf("create extracted directory for %q: %w", job.entry.Name, err)
						cancelOnce.Do(func() {
							firstErr = err
							close(done)
						})
						return
					}
					if err := extractFile(job.entry, job.target); err != nil {
						cancelOnce.Do(func() {
							firstErr = err
							close(done)
						})
						return
					}
				}
			}
		}()
	}
	wait.Wait()
	return firstErr
}

func safeZIPPath(name string) (string, error) {
	// ZIP paths use '/', even on Windows. Normalize '\\' too so archives made
	// by unusual tools cannot bypass traversal checks on another host OS.
	name = strings.ReplaceAll(name, "\\", "/")
	if strings.IndexByte(name, 0) >= 0 {
		return "", errors.New("texture-pack ZIP contains a NUL byte in an entry name")
	}
	if strings.HasPrefix(name, "/") || filepath.IsAbs(name) ||
		(len(name) >= 2 && name[1] == ':') {
		return "", fmt.Errorf("texture-pack ZIP contains absolute path %q", name)
	}
	clean := filepath.Clean(filepath.FromSlash(name))
	if clean == "." {
		return "", nil
	}
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("texture-pack ZIP entry escapes extraction directory: %q", name)
	}
	return clean, nil
}

func extractFile(entry *zip.File, target string) (err error) {
	source, err := entry.Open()
	if err != nil {
		return fmt.Errorf("open texture-pack entry %q: %w", entry.Name, err)
	}
	defer source.Close()
	destination, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("create extracted file for %q: %w", entry.Name, err)
	}
	defer func() {
		closeErr := destination.Close()
		if err == nil && closeErr != nil {
			err = fmt.Errorf("close extracted file for %q: %w", entry.Name, closeErr)
		}
	}()
	written, err := io.Copy(destination, io.LimitReader(source, maxPackFileSize+1))
	if err != nil {
		return fmt.Errorf("extract texture-pack entry %q: %w", entry.Name, err)
	}
	if written > maxPackFileSize || uint64(written) != entry.UncompressedSize64 {
		return fmt.Errorf("texture-pack entry %q has an invalid extracted size", entry.Name)
	}
	return nil
}

type textureSpec struct {
	key        string
	relative   string
	alternates []string
}

func requestedTexturePaths() map[string]struct{} {
	paths := make(map[string]struct{})
	for _, spec := range requestedTextures() {
		paths[strings.ToLower(filepath.ToSlash(spec.relative))] = struct{}{}
		for _, alternate := range spec.alternates {
			paths[strings.ToLower(filepath.ToSlash(alternate))] = struct{}{}
		}
	}
	const itemRoot = "assets/minecraft/textures/items/"
	paths[itemRoot+"potion_overlay.png"] = struct{}{}
	paths[itemRoot+"potion_bottle_drinkable.png"] = struct{}{}
	return paths
}

func isRequestedTexturePath(path string, wanted map[string]struct{}) bool {
	path = strings.ToLower(filepath.ToSlash(path))
	if _, exists := wanted[path]; exists {
		return true
	}
	for candidate := range wanted {
		if strings.HasSuffix(path, "/"+candidate) {
			return true
		}
	}
	return false
}

func requestedTextures() []textureSpec {
	item := func(name string) string { return "assets/minecraft/textures/items/" + name + ".png" }
	block := func(name string) string { return "assets/minecraft/textures/blocks/" + name + ".png" }
	var specs []textureSpec
	for _, material := range []string{"diamond", "iron", "stone", "wood"} {
		for _, tool := range []string{"sword", "axe", "pickaxe"} {
			name := material + "_" + tool
			spec := textureSpec{key: name, relative: item(name)}
			if material == "wood" {
				spec.alternates = []string{item("wooden_" + tool)}
			}
			specs = append(specs, spec)
		}
	}
	specs = append(specs,
		textureSpec{key: "apple_golden", relative: item("apple_golden"), alternates: []string{item("golden_apple")}},
		textureSpec{key: "shears", relative: item("shears"), alternates: []string{item("shear")}},
		textureSpec{key: "emerald", relative: item("emerald")},
		textureSpec{key: "diamond", relative: item("diamond")},
		textureSpec{key: "iron_ingot", relative: item("iron_ingot"), alternates: []string{item("iron")}},
		textureSpec{key: "ender_pearl", relative: item("ender_pearl"), alternates: []string{item("pearl")}},
		textureSpec{key: "fireball", relative: item("fireball")},
		textureSpec{key: "bow_standby", relative: item("bow_standby"), alternates: []string{item("bow")}},
		textureSpec{key: "bow_pulling_0", relative: item("bow_pulling_0")},
		textureSpec{key: "bow_pulling_1", relative: item("bow_pulling_1")},
		textureSpec{key: "bow_pulling_2", relative: item("bow_pulling_2")},
	)

	colors := []string{
		"white", "orange", "magenta", "light_blue", "yellow", "lime", "pink", "gray",
		"silver", "cyan", "purple", "blue", "brown", "green", "red", "black",
	}
	for _, name := range colors {
		specs = append(specs, textureSpec{
			key:        "wool_" + name,
			relative:   block("wool_colored_" + name),
			alternates: []string{block(name + "_wool")},
		})
	}
	for _, name := range []string{"red", "blue", "white"} {
		specs = append(specs, textureSpec{
			key:      "hardened_clay_" + name,
			relative: block("hardened_clay_stained_" + name),
			alternates: []string{
				block(name + "_stained_hardened_clay"),
				block(name + "_terracotta"),
			},
		})
	}

	// The pack's single custom-sky sheet (e.g. cloud1.png): a 3x2 grid of
	// six square faces, sliced below into sky_cloud_0.."sky_cloud_5" and
	// mapped straight onto Roblox's plain Sky object properties.
	specs = append(specs, textureSpec{
		key:        "sky_cloud_sheet",
		relative:   "assets/minecraft/mcpatcher/sky/world0/cloud1.png",
		alternates: []string{"assets/minecraft/optifine/sky/world0/cloud1.png"},
	})
	return specs
}

func discoverPackTextures(result *TexturePack) error {
	index, err := indexExtractedFiles(result.TempDir)
	if err != nil {
		return err
	}
	for _, spec := range requestedTextures() {
		path := findTexture(index, append([]string{spec.relative}, spec.alternates...)...)
		if path == "" {
			result.Missing = append(result.Missing, spec.key)
			continue
		}
		result.Textures[spec.key] = path
	}
	if err := generatePotions(result, index); err != nil {
		return err
	}
	if err := sliceCloudSkySheets(result); err != nil {
		return err
	}
	sort.Strings(result.Missing)
	return nil
}

// cloudSkyFaceOrder is the reading-order index -> Roblox Sky face used when
// slicing a single custom-sky sheet: 0=front, 1=right, 2=back, 3=left,
// 4=top, 5=bottom.
var cloudSkyFaceOrder = [6]struct{ col, row int }{
	{0, 0}, {1, 0}, {2, 0},
	{0, 1}, {1, 1}, {2, 1},
}

// sliceCloudSkySheets finds a discovered "sky_cloud_sheet" single-image
// custom sky (a 3-column by 2-row grid of six square faces, e.g. cloud1.png)
// and slices it into six separate face images, registered as
// "sky_cloud_0".."sky_cloud_5" so the pipeline can map them straight onto
// Roblox's plain Sky object properties (SkyFront, SkyTop, etc). The raw,
// unsliced sheet key is removed afterward since nothing binds to it directly.
func sliceCloudSkySheets(result *TexturePack) error {
	const sheetKey = "sky_cloud_sheet"
	sheetPath, ok := result.Textures[sheetKey]
	if !ok {
		return nil
	}
	delete(result.Textures, sheetKey)
	// A 3x2 sheet is inherently up to 6x the pixels of a single game
	// texture, so it needs its own, larger decode budget rather than
	// the per-face limits DecodeTexturePNG enforces.
	sheet, err := DecodeTexturePNGWithBudget(sheetPath, 3*MaxTextureDimension, 6*MaxTexturePixels)
	if err != nil {
		return fmt.Errorf("decode custom sky sheet: %w", err)
	}
	bounds := sheet.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width%3 != 0 || height%2 != 0 || width/3 != height/2 {
		return fmt.Errorf(
			"custom sky sheet is %dx%d, expected a 3x2 grid of equal square faces (e.g. 6144x4096)",
			width, height,
		)
	}
	tile := width / 3
	generatedDir := filepath.Join(result.TempDir, "autopack_generated")
	if err := os.MkdirAll(generatedDir, 0o755); err != nil {
		return fmt.Errorf("create generated cloud-sky directory: %w", err)
	}
	for i, face := range cloudSkyFaceOrder {
		facePath := filepath.Join(generatedDir, fmt.Sprintf("cloud_%d.png", i))
		region := image.Rect(
			bounds.Min.X+face.col*tile, bounds.Min.Y+face.row*tile,
			bounds.Min.X+(face.col+1)*tile, bounds.Min.Y+(face.row+1)*tile,
		)
		if err := writeCloudSkyFace(sheet, region, facePath); err != nil {
			return fmt.Errorf("slice custom sky face %d: %w", i, err)
		}
		result.Textures[fmt.Sprintf("sky_cloud_%d", i)] = facePath
	}
	return nil
}

func writeCloudSkyFace(sheet image.Image, region image.Rectangle, outputPath string) error {
	face := image.NewNRGBA(image.Rect(0, 0, region.Dx(), region.Dy()))
	draw.Draw(face, face.Bounds(), sheet, region.Min, draw.Src)
	file, err := os.OpenFile(outputPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if err := png.Encode(file, face); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func indexExtractedFiles(root string) (map[string][]string, error) {
	index := make(map[string][]string)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		normalized := strings.ToLower(filepath.ToSlash(relative))
		index[normalized] = append(index[normalized], path)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("index extracted texture pack: %w", err)
	}
	for key := range index {
		sort.Strings(index[key])
	}
	return index, nil
}

func findTexture(index map[string][]string, candidates ...string) string {
	for _, candidate := range candidates {
		candidate = strings.ToLower(filepath.ToSlash(candidate))
		if matches := index[candidate]; len(matches) != 0 {
			return matches[0]
		}
		// Packs are often zipped with an extra top-level directory.
		var wrapped []string
		for indexed, paths := range index {
			if strings.HasSuffix(indexed, "/"+candidate) {
				wrapped = append(wrapped, paths...)
			}
		}
		if len(wrapped) != 0 {
			sort.Strings(wrapped)
			return wrapped[0]
		}
	}
	return ""
}

func generatePotions(result *TexturePack, index map[string][]string) error {
	const itemRoot = "assets/minecraft/textures/items/"
	overlay := findTexture(index, itemRoot+"potion_overlay.png")
	bottle := findTexture(index, itemRoot+"potion_bottle_drinkable.png")
	if overlay == "" || bottle == "" {
		result.Missing = append(result.Missing, "jump_pot", "speed_pot")
		return nil
	}
	overlayImage, err := decodePNG(overlay)
	if err != nil {
		return err
	}
	bottleImage, err := decodePNG(bottle)
	if err != nil {
		return err
	}
	if overlayImage.Bounds().Dx() != bottleImage.Bounds().Dx() || overlayImage.Bounds().Dy() != bottleImage.Bounds().Dy() {
		return fmt.Errorf("potion overlay is %dx%d but bottle is %dx%d",
			overlayImage.Bounds().Dx(), overlayImage.Bounds().Dy(), bottleImage.Bounds().Dx(), bottleImage.Bounds().Dy())
	}
	variants := []struct {
		name string
		tint color.NRGBA
	}{
		{name: "jump_pot", tint: color.NRGBA{R: 0x22, G: 0xff, B: 0x4c, A: 0xff}},
		{name: "speed_pot", tint: color.NRGBA{R: 0x7c, G: 0xaf, B: 0xc6, A: 0xff}},
	}
	generatedDir := filepath.Join(result.TempDir, "autopack_generated")
	if err := os.MkdirAll(generatedDir, 0o755); err != nil {
		return fmt.Errorf("create generated-potion directory: %w", err)
	}
	paths := make([]string, len(variants))
	errorsByVariant := make([]error, len(variants))
	var wait sync.WaitGroup
	wait.Add(len(variants))
	for index, variant := range variants {
		go func() {
			defer wait.Done()
			output := filepath.Join(generatedDir, variant.name+".png")
			errorsByVariant[index] = renderPotion(overlayImage, bottleImage, output, variant.tint)
			paths[index] = output
		}()
	}
	wait.Wait()
	for index, variant := range variants {
		if errorsByVariant[index] != nil {
			return fmt.Errorf("generate %s: %w", variant.name, errorsByVariant[index])
		}
		result.Textures[variant.name] = paths[index]
	}
	return nil
}

func renderPotion(overlay, bottle image.Image, outputPath string, tint color.NRGBA) error {
	bounds := image.Rect(0, 0, overlay.Bounds().Dx(), overlay.Bounds().Dy())
	output := image.NewNRGBA(bounds)
	for y := 0; y < bounds.Dy(); y++ {
		for x := 0; x < bounds.Dx(); x++ {
			source := color.NRGBAModel.Convert(
				overlay.At(overlay.Bounds().Min.X+x, overlay.Bounds().Min.Y+y),
			).(color.NRGBA)
			output.SetNRGBA(x, y, color.NRGBA{
				R: uint8(uint16(source.R) * uint16(tint.R) / 255),
				G: uint8(uint16(source.G) * uint16(tint.G) / 255),
				B: uint8(uint16(source.B) * uint16(tint.B) / 255),
				A: uint8(uint16(source.A) * uint16(tint.A) / 255),
			})
		}
	}
	draw.Draw(output, bounds, bottle, bottle.Bounds().Min, draw.Over)
	file, err := os.OpenFile(outputPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if err := png.Encode(file, output); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func decodePNG(path string) (image.Image, error) {
	return DecodeTexturePNG(path)
}
