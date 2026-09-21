package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/qaustria/AutoPack-Go/utils"
)

// UploadBatcher is the upload boundary used by the processing pipeline.
// AssetUploader implements it, while tests and future frontends can provide
// their own implementation without handling Roblox credentials here.
type UploadBatcher interface {
	UploadMany(context.Context, []utils.UploadRequest) []utils.UploadResult
}

// PipelineProgress receives one final event for each prepared upload.
type PipelineProgress func(done, total int, name string, err error)

// PipelineLog receives sanitized, user-facing messages for completed pipeline
// milestones. It never receives credentials or temporary filesystem paths.
type PipelineLog func(message string)

// PipelineResult is the strict JSON object consumed by the target game.
type PipelineResult struct {
	Values     map[string]any
	PackID     string
	PackName   string
	PreviewPNG []byte
}

type textureBinding struct {
	SourceKey     string
	TextureFields []string
	VPFields      []string
}

type meshBinding struct {
	Name       string
	SourceKeys []string
	JSONFields []string
	Config     utils.Config
}

type preparedUpload struct {
	Request    utils.UploadRequest
	JSONFields []string
	Required   bool
}

type cachedTexture struct {
	Resized             *image.NRGBA
	Expanded            *image.NRGBA
	AlphaNoiseThreshold int
	RemovedIslandPixels int
}

// preparationConcurrency keeps every CPU core busy without allowing machines
// with very large core counts to allocate an excessive number of simultaneous
// 512x512 edge-expansion workspaces.
func preparationConcurrency() int {
	workers := runtime.GOMAXPROCS(0)
	if workers < 1 {
		return 1
	}
	if workers > 32 {
		return 32
	}
	return workers
}

// Uploads are mostly network waits, so using more workers than CPU-bound image
// preparation is useful. The cap prevents a large machine from flooding the
// Roblox endpoint with all assets at once.
func uploadConcurrency() int {
	workers := runtime.GOMAXPROCS(0) * 2
	if workers < 16 {
		workers = 16
	}
	if workers > 32 {
		workers = 32
	}
	return workers
}

// JSON field mapping from Minecraft 1.8.9 texture keys. Iron tools deliberately
// populate the game's Gold fields, matching Example_Texturepack.jsonc.
var pipelineTextures = []textureBinding{
	{SourceKey: "stone_sword", TextureFields: []string{"SwordTexture"}, VPFields: []string{"SwordVPImage"}},
	{SourceKey: "diamond_sword", TextureFields: []string{"DiamondSwordTexture"}, VPFields: []string{"DiamondSwordVPImage"}},
	{SourceKey: "iron_sword", TextureFields: []string{"GoldSwordTexture"}, VPFields: []string{"GoldSwordVPImage"}},
	{SourceKey: "wood_sword", TextureFields: []string{"WoodenSwordTexture"}, VPFields: []string{"WoodenSwordVPImage"}},

	{SourceKey: "stone_pickaxe", TextureFields: []string{"PickaxeTexture"}, VPFields: []string{"PickaxeVPImage"}},
	{SourceKey: "diamond_pickaxe", TextureFields: []string{"DiamondPickaxeTexture"}, VPFields: []string{"DiamondPickaxeVPImage"}},
	{SourceKey: "iron_pickaxe", TextureFields: []string{"GoldPickaxeTexture"}, VPFields: []string{"GoldPickaxeVPImage"}},
	{SourceKey: "wood_pickaxe", TextureFields: []string{"WoodenPickaxeTexture"}, VPFields: []string{"WoodenPickaxeVPImage"}},

	{SourceKey: "stone_axe", TextureFields: []string{"AxeTexture"}, VPFields: []string{"AxeVPImage"}},
	{SourceKey: "diamond_axe", TextureFields: []string{"DiamondAxeTexture"}, VPFields: []string{"DiamondAxeVPImage"}},
	{SourceKey: "iron_axe", TextureFields: []string{"GoldAxeTexture"}, VPFields: []string{"GoldAxeVPImage"}},
	{SourceKey: "wood_axe", TextureFields: []string{"WoodenAxeTexture"}, VPFields: []string{"WoodenAxeVPImage"}},

	{SourceKey: "bow_standby", TextureFields: []string{"Bow0Texture"}, VPFields: []string{"DefaultBowVPImage"}},
	{SourceKey: "bow_pulling_0", TextureFields: []string{"Bow1Texture"}},
	{SourceKey: "bow_pulling_1", TextureFields: []string{"Bow2Texture"}},
	{SourceKey: "bow_pulling_2", TextureFields: []string{"Bow3Texture"}},

	{SourceKey: "apple_golden", TextureFields: []string{"GoldAppleTexture"}, VPFields: []string{"GoldAppleVPImage"}},
	{SourceKey: "iron_ingot", TextureFields: []string{"IronTexture"}, VPFields: []string{"IronVPImage"}},
	{SourceKey: "diamond", TextureFields: []string{"DiamondTexture"}, VPFields: []string{"DiamondVPImage"}},
	{SourceKey: "emerald", TextureFields: []string{"EmeraldTexture"}, VPFields: []string{"EmeraldVPImage"}},
	{SourceKey: "ender_pearl", TextureFields: []string{"PearlTexture"}, VPFields: []string{"PearlVPImage"}},
	{SourceKey: "shears", TextureFields: []string{"ShearsTexture"}, VPFields: []string{"ShearsVPImage"}},
	{SourceKey: "fireball", TextureFields: []string{"FireballTexture"}, VPFields: []string{"FireballVPImage"}},
	{SourceKey: "jump_pot", TextureFields: []string{"JumpPotionTexture"}, VPFields: []string{"JumpPotionVPImage"}},
	{SourceKey: "speed_pot", TextureFields: []string{"SpeedPotionTexture"}, VPFields: []string{"SpeedPotionVPImage"}},

	// The game calls these Clay fields, but they intentionally use wool.
	{SourceKey: "wool_blue", TextureFields: []string{"ClayBlue"}},
	{SourceKey: "wool_cyan", TextureFields: []string{"ClayCyan"}},
	{SourceKey: "wool_green", TextureFields: []string{"ClayGreen"}},
	{SourceKey: "wool_gray", TextureFields: []string{"ClayGrey"}},
	{SourceKey: "wool_orange", TextureFields: []string{"ClayOrange"}},
	{SourceKey: "wool_purple", TextureFields: []string{"ClayPurple"}},
	{SourceKey: "wool_red", TextureFields: []string{"ClayRed"}},
	{SourceKey: "wool_white", TextureFields: []string{"ClayWhite"}},
	{SourceKey: "wool_yellow", TextureFields: []string{"ClayYellow"}},

	// The single custom-sky sheet (e.g. cloud1.png) is pre-sliced by
	// utils.UnzipTexturePack into 6 reading-order faces under
	// "sky_cloud_0".."sky_cloud_5" (top-left, top-mid, top-right,
	// bottom-left, bottom-mid, bottom-right), which map onto Roblox's plain
	// Sky object properties. Base mapping is reading order: 0=Front,
	// 1=Right, 2=Back, 3=Left, 4=Top, 5=Bottom - swapped per an in-game
	// report that 0 (bound to Front) actually showed Bottom's content.
	{SourceKey: "sky_cloud_0", VPFields: []string{"SkyBottom"}},
	{SourceKey: "sky_cloud_1", VPFields: []string{"SkyTop"}},
	{SourceKey: "sky_cloud_2", VPFields: []string{"SkyLeft"}},
	{SourceKey: "sky_cloud_3", VPFields: []string{"SkyBack"}},
	{SourceKey: "sky_cloud_4", VPFields: []string{"SkyRight"}},
	{SourceKey: "sky_cloud_5", VPFields: []string{"SkyFront"}},
}

func pipelineMeshes() []meshBinding {
	standard := utils.DefaultConfig()
	flat := standard
	flat.RotateY = 0
	// Axes need the opposite horizontal direction from swords and pickaxes.
	// Keep the original sword tilt and apply only this item-specific half turn.
	axe := standard
	axe.RotateZ += 180
	return []meshBinding{
		{Name: "sword", SourceKeys: []string{"stone_sword", "diamond_sword", "iron_sword", "wood_sword"}, JSONFields: []string{"SwordMesh"}, Config: standard},
		{Name: "pickaxe", SourceKeys: []string{"stone_pickaxe", "diamond_pickaxe", "iron_pickaxe", "wood_pickaxe"}, JSONFields: []string{"PickaxeMesh"}, Config: standard},
		{Name: "axe", SourceKeys: []string{"stone_axe", "diamond_axe", "iron_axe", "wood_axe"}, JSONFields: []string{"AxeMesh"}, Config: axe},
		{Name: "bow_0", SourceKeys: []string{"bow_standby"}, JSONFields: []string{"Bow0Mesh"}, Config: standard},
		{Name: "bow_1", SourceKeys: []string{"bow_pulling_0"}, JSONFields: []string{"Bow1Mesh"}, Config: standard},
		{Name: "bow_2", SourceKeys: []string{"bow_pulling_1"}, JSONFields: []string{"Bow2Mesh"}, Config: standard},
		{Name: "bow_3", SourceKeys: []string{"bow_pulling_2"}, JSONFields: []string{"Bow3Mesh"}, Config: standard},
		{Name: "gold_apple", SourceKeys: []string{"apple_golden"}, JSONFields: []string{"GoldAppleMesh"}, Config: flat},
		{Name: "iron", SourceKeys: []string{"iron_ingot"}, JSONFields: []string{"IronMesh"}, Config: flat},
		{Name: "diamond", SourceKeys: []string{"diamond"}, JSONFields: []string{"DiamondMesh"}, Config: flat},
		{Name: "emerald", SourceKeys: []string{"emerald"}, JSONFields: []string{"EmeraldMesh"}, Config: flat},
		{Name: "pearl", SourceKeys: []string{"ender_pearl"}, JSONFields: []string{"PearlMesh"}, Config: flat},
		{Name: "shears", SourceKeys: []string{"shears"}, JSONFields: []string{"ShearsMesh"}, Config: standard},
		// Like potions, the fireball is displayed by a view model that expects an
		// upright flat mesh; the standard tool yaw tips it onto its side.
		{Name: "fireball", SourceKeys: []string{"fireball"}, JSONFields: []string{"FireballMesh"}, Config: flat},
		// Potions must remain vertically upright in the target view model. The
		// standard -45-degree yaw makes their bottle axis lie on its side.
		{Name: "potion", SourceKeys: []string{"jump_pot", "speed_pot"}, JSONFields: []string{"JumpPotionMesh", "SpeedPotionMesh"}, Config: flat},
	}
}

// RunPipeline extracts, prepares, meshes, uploads, and maps one texture pack.
// It never writes credentials or intermediate files outside its private temp
// folder, which is removed before this function returns.
func RunPipeline(ctx context.Context, zipPath string, uploader UploadBatcher, progress PipelineProgress) (PipelineResult, error) {
	return runPipeline(ctx, zipPath, uploader, progress, nil)
}

func runPipeline(ctx context.Context, zipPath string, uploader UploadBatcher, progress PipelineProgress, log PipelineLog) (PipelineResult, error) {
	if ctx == nil {
		return PipelineResult{}, errors.New("pipeline context is nil")
	}
	if uploader == nil {
		return PipelineResult{}, errors.New("upload client is nil")
	}
	logPipeline(log, "Cone engine "+utils.Version)
	logPipeline(log, "Reading Minecraft texture pack")
	pack, err := utils.UnzipTexturePack(zipPath)
	if err != nil {
		return PipelineResult{}, err
	}
	defer pack.Cleanup()
	if pack.Textures["sky_cloud_0"] != "" {
		logPipeline(log, "Found and sliced custom sky sheet into 6 faces")
	}
	if missing := missingOptionalTextures(pack.Textures); len(missing) != 0 {
		logPipeline(log, "No texture provided, using 0 (no asset) for: "+strings.Join(missing, ", "))
	}
	logPipeline(log, fmt.Sprintf("Found %d mapped textures", len(pack.Textures)))
	previewPNG, err := buildHotbarPreview(pack.Textures)
	if err != nil {
		return PipelineResult{}, err
	}
	logPipeline(log, "Rendered hotbar preview")

	workDir := filepath.Join(pack.TempDir, "autopack_pipeline")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return PipelineResult{}, fmt.Errorf("create pipeline work directory: %w", err)
	}
	prepared, err := preparePipelineAssets(ctx, pack.Textures, workDir, log)
	if err != nil {
		return PipelineResult{}, err
	}
	requests := make([]utils.UploadRequest, len(prepared))
	for index := range prepared {
		requests[index] = prepared[index].Request
	}
	logPipeline(log, fmt.Sprintf("Uploading %d assets to Roblox", len(requests)))
	results, streamed := uploadPipelineAssets(ctx, uploader, requests, progress)
	if len(results) != len(prepared) {
		return PipelineResult{}, fmt.Errorf("upload client returned %d results for %d requests", len(results), len(prepared))
	}
	values, err := defaultPipelineValues()
	if err != nil {
		return PipelineResult{}, err
	}
	zeroMissingTextureFields(values, pack.Textures)
	accepted := 0
	skipped := 0
	var requiredFailures []string
	for index, result := range results {
		var resultErr error
		if result.Error != "" {
			resultErr = errors.New(result.Error)
		} else if result.Asset == nil || result.Asset.AssetID == "" {
			resultErr = errors.New("upload returned no asset ID")
		} else {
			accepted++
			for _, field := range prepared[index].JSONFields {
				values[field] = result.Asset.AssetID
			}
		}
		if resultErr != nil {
			skipped++
			if prepared[index].Required {
				requiredFailures = append(requiredFailures, fmt.Sprintf("%s: %s", prepared[index].Request.DisplayName, resultErr))
				logPipeline(log, fmt.Sprintf("Required %s failed. Roblox error: %s", prepared[index].Request.DisplayName, resultErr))
			} else {
				logPipeline(log, fmt.Sprintf(
					"Skipped %s; using default for %s. Roblox error: %s",
					prepared[index].Request.DisplayName,
					strings.Join(prepared[index].JSONFields, ", "),
					resultErr,
				))
			}
		}
		if progress != nil && !streamed {
			progress(index+1, len(results), prepared[index].Request.DisplayName, resultErr)
		}
	}
	if err := ctx.Err(); err != nil {
		return PipelineResult{}, err
	}
	if len(requiredFailures) != 0 {
		logPipeline(log, fmt.Sprintf("Roblox accepted %d assets, but %d required tool meshes failed", accepted, len(requiredFailures)))
		return PipelineResult{}, fmt.Errorf("required generated mesh upload failed; Cone refused to return a broken pack: %s", strings.Join(requiredFailures, "; "))
	}
	if skipped == 0 {
		logPipeline(log, fmt.Sprintf("Roblox accepted all %d assets and applied permissions", accepted))
	} else {
		logPipeline(log, fmt.Sprintf(
			"Roblox accepted %d assets; skipped %d and kept their default asset IDs",
			accepted, skipped,
		))
	}
	applySkyRotation(values, skyRotationSteps())
	return PipelineResult{Values: values, PreviewPNG: previewPNG}, nil
}

// zeroMissingTextureFields resets every JSON field whose source texture is
// absent from the pack to "0" (Roblox's "no asset" value), so a texture the
// pack never shipped comes out as a plain "0" instead of silently
// borrowing Cone's own placeholder skin for it. Fields whose texture *is*
// present in the pack are left as the loaded default: if the upload loop
// below successfully uploads it, that default gets overwritten with the
// real asset ID; if the upload fails (a transient Roblox error, say), the
// field keeps the default rather than going blank, since the pack did
// genuinely ship art for it. Fields that aren't tied to any texture at all
// (mesh transform values like AxeRotation/AxeScale) are left untouched.
func zeroMissingTextureFields(values map[string]any, textures map[string]string) {
	for _, binding := range pipelineTextures {
		if textures[binding.SourceKey] != "" {
			continue
		}
		for _, field := range binding.TextureFields {
			values[field] = "0"
		}
		for _, field := range binding.VPFields {
			values[field] = "0"
		}
	}
	for _, binding := range pipelineMeshes() {
		available := false
		for _, sourceKey := range binding.SourceKeys {
			if textures[sourceKey] != "" {
				available = true
				break
			}
		}
		if available {
			continue
		}
		for _, field := range binding.JSONFields {
			values[field] = "0"
		}
	}
}

// Minecraft's compass-based panorama order (north/east/south/west) has no
// fixed relationship to Roblox's world-axis-based Sky faces (Ft=-Z, Rt=+X,
// Bk=+Z, Lf=-X); the "correct" alignment depends entirely on how a given
// Roblox place is oriented and can only be judged by eye in Studio. Rather
// than guess, Cone lets CONE_SKY_ROTATION relabel which uploaded face goes
// into which JSON field after everything is already uploaded, so fixing a
// sky that looks rotated never requires re-uploading any images.
//
// skyRotationSteps reads CONE_SKY_ROTATION as a number of 90-degree
// clockwise steps (0-3, or any integer - it wraps). It defaults to 0.
func skyRotationSteps() int {
	raw := strings.TrimSpace(os.Getenv("CONE_SKY_ROTATION"))
	if raw == "" {
		return 0
	}
	steps, err := strconv.Atoi(raw)
	if err != nil {
		return 0
	}
	steps %= 4
	if steps < 0 {
		steps += 4
	}
	return steps
}

// applySkyRotation cyclically relabels the horizontal ring of face fields
// (Ft -> Rt -> Bk -> Lf -> Ft, i.e. one 90-degree clockwise step per call)
// for the vanilla panorama and every MCPatcher/OptiFine sky state, leaving
// Up/Dn untouched. It is a pure relabeling of already-uploaded asset IDs.
func applySkyRotation(values map[string]any, steps int) {
	if steps == 0 {
		return
	}
	ring := []string{"Front", "Right", "Back", "Left"}
	fields := make([]string, len(ring))
	originals := make([]any, len(ring))
	haveAny := false
	for i, suffix := range ring {
		fields[i] = "Sky" + suffix
		originals[i] = values[fields[i]]
		if originals[i] != nil {
			haveAny = true
		}
	}
	if !haveAny {
		return
	}
	for i, field := range fields {
		values[field] = originals[(i-steps%4+4)%4]
	}
}

type streamingUploadBatcher interface {
	UploadManyWithProgress(context.Context, []utils.UploadRequest, func(index int, result utils.UploadResult)) []utils.UploadResult
}

func uploadPipelineAssets(ctx context.Context, uploader UploadBatcher, requests []utils.UploadRequest, progress PipelineProgress) ([]utils.UploadResult, bool) {
	streaming, ok := uploader.(streamingUploadBatcher)
	if !ok {
		return uploader.UploadMany(ctx, requests), false
	}
	completed := 0
	var callbackMu sync.Mutex
	results := streaming.UploadManyWithProgress(ctx, requests, func(_ int, result utils.UploadResult) {
		callbackMu.Lock()
		defer callbackMu.Unlock()
		completed++
		if progress == nil {
			return
		}
		var resultErr error
		if result.Error != "" {
			resultErr = errors.New(result.Error)
		} else if result.Asset == nil || result.Asset.AssetID == "" {
			resultErr = errors.New("upload returned no asset ID")
		}
		progress(completed, len(requests), result.Request.DisplayName, resultErr)
	})
	return results, true
}

func logPipeline(log PipelineLog, message string) {
	if log != nil {
		log(message)
	}
}

// missingOptionalTextures reports every known pipeline texture key that isn't
// present in the uploaded pack. Nothing in this pipeline is a hard
// requirement anymore: a pack containing only, say, a sword will port only
// that sword, and every other slot silently falls back to the target game's
// default asset (see defaultPipelineValues / default_config.json).
func missingOptionalTextures(textures map[string]string) []string {
	known := make(map[string]struct{})
	for _, binding := range pipelineTextures {
		known[binding.SourceKey] = struct{}{}
	}
	for _, binding := range pipelineMeshes() {
		for _, sourceKey := range binding.SourceKeys {
			known[sourceKey] = struct{}{}
		}
	}
	missing := make([]string, 0, len(known))
	for key := range known {
		if textures[key] == "" {
			missing = append(missing, key)
		}
	}
	sort.Strings(missing)
	return missing
}

func preparePipelineAssets(ctx context.Context, textures map[string]string, workDir string, log PipelineLog) ([]preparedUpload, error) {
	cache, err := cachePipelineTextures(ctx, textures, log)
	if err != nil {
		return nil, err
	}

	// Keep the old deterministic upload order while letting every independent
	// PNG and mesh preparation job execute through the same CPU-sized pool.
	type assetJob func() (preparedUpload, error)
	jobs := make([]assetJob, 0, len(pipelineTextures)*2+len(pipelineMeshes()))
	for _, binding := range pipelineTextures {
		binding := binding
		if textures[binding.SourceKey] == "" {
			continue
		}
		if len(binding.TextureFields) != 0 {
			jobs = append(jobs, func() (preparedUpload, error) {
				return prepareTextureUpload(cache[binding.SourceKey].Expanded, binding, false, workDir)
			})
		}
		if len(binding.VPFields) != 0 {
			jobs = append(jobs, func() (preparedUpload, error) {
				return prepareTextureUpload(cache[binding.SourceKey].Resized, binding, true, workDir)
			})
		}
	}
	for _, binding := range pipelineMeshes() {
		binding := binding
		available := false
		for _, sourceKey := range binding.SourceKeys {
			if textures[sourceKey] != "" {
				available = true
				break
			}
		}
		if !available {
			continue
		}
		jobs = append(jobs, func() (preparedUpload, error) {
			return prepareMeshBinding(cache, binding, workDir)
		})
	}
	meshCount := 0
	for _, binding := range pipelineMeshes() {
		for _, sourceKey := range binding.SourceKeys {
			if textures[sourceKey] != "" {
				meshCount++
				break
			}
		}
	}
	imageCount := len(jobs) - meshCount
	logPipeline(log, fmt.Sprintf("Encoding %d images and %d greedy meshes", imageCount, meshCount))

	prepared := make([]preparedUpload, len(jobs))
	err = parallelFor(ctx, len(jobs), preparationConcurrency(), func(index int) error {
		upload, err := jobs[index]()
		if err == nil {
			prepared[index] = upload
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	logPipeline(log, fmt.Sprintf("Prepared %d upload files", len(prepared)))
	return prepared, nil
}

func cachePipelineTextures(ctx context.Context, textures map[string]string, log PipelineLog) (map[string]cachedTexture, error) {
	required := make(map[string]bool)
	for _, binding := range pipelineTextures {
		if textures[binding.SourceKey] == "" {
			continue
		}
		required[binding.SourceKey] = len(binding.TextureFields) != 0
	}
	for _, binding := range pipelineMeshes() {
		for _, sourceKey := range binding.SourceKeys {
			if textures[sourceKey] == "" {
				continue
			}
			if _, exists := required[sourceKey]; !exists {
				required[sourceKey] = false
			}
		}
	}
	keys := make([]string, 0, len(required))
	for key := range required {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	entries := make([]cachedTexture, len(keys))
	alphaThreshold := utils.DefaultConfig().AlphaThreshold
	logPipeline(log, fmt.Sprintf("Decoding and resizing %d textures", len(keys)))
	if err := parallelFor(ctx, len(keys), preparationConcurrency(), func(index int) error {
		key := keys[index]
		img, err := utils.DecodeTexturePNG(textures[key])
		if err != nil {
			return fmt.Errorf("decode texture %q: %w", key, err)
		}
		if strings.HasPrefix(key, "sky_cloud_") {
			// Sky faces are smooth painterly art, not pixel-art item
			// textures, so they skip ResizeTexture's 512x512
			// nearest-neighbor path (built for crisp pixel edges) in
			// favor of a larger, bilinear-resized one that preserves
			// gradients. They also have no transparency to clean up.
			entries[index].Resized = utils.ResizeSkyFace(img)
			return nil
		}
		entries[index].Resized = utils.ResizeTexture(img)
		entries[index].AlphaNoiseThreshold = utils.RemoveBackgroundAlphaNoise(entries[index].Resized)
		entries[index].RemovedIslandPixels = utils.RemoveTinyAlphaIslands(entries[index].Resized, alphaThreshold)
		return nil
	}); err != nil {
		return nil, err
	}
	logPipeline(log, fmt.Sprintf("Resized %d textures to 512x512", len(keys)))
	var cleanedAlpha []string
	for index, entry := range entries {
		if entry.AlphaNoiseThreshold > 0 {
			cleanedAlpha = append(cleanedAlpha, fmt.Sprintf("%s (alpha <= %d)", keys[index], entry.AlphaNoiseThreshold))
		}
	}
	if len(cleanedAlpha) > 0 {
		logPipeline(log, "Removed invalid transparent-background noise from "+strings.Join(cleanedAlpha, ", "))
	}
	var cleanedIslands []string
	for index, entry := range entries {
		if entry.RemovedIslandPixels > 0 {
			cleanedIslands = append(cleanedIslands, fmt.Sprintf("%s (%d pixels)", keys[index], entry.RemovedIslandPixels))
		}
	}
	if len(cleanedIslands) > 0 {
		logPipeline(log, "Removed detached texture artifacts from "+strings.Join(cleanedIslands, ", "))
	}

	// Expansion is the most expensive image operation. Run it on all cores only
	// once per source and share the immutable result with PNG and GLB encoders.
	expandedCount := 0
	for _, expands := range required {
		if expands {
			expandedCount++
		}
	}
	logPipeline(log, fmt.Sprintf("Edge-expanding %d textures", expandedCount))
	if err := parallelFor(ctx, len(keys), preparationConcurrency(), func(index int) error {
		if required[keys[index]] {
			entries[index].Expanded = utils.EdgeExpand(entries[index].Resized)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	logPipeline(log, fmt.Sprintf("Edge-expanded %d textures", expandedCount))
	cache := make(map[string]cachedTexture, len(keys))
	for index, key := range keys {
		cache[key] = entries[index]
	}
	return cache, nil
}

func prepareTextureUpload(img image.Image, binding textureBinding, vpImage bool, workDir string) (preparedUpload, error) {
	suffix := "_texture.png"
	displaySuffix := " texture"
	fields := binding.TextureFields
	if vpImage {
		suffix = "_vp.png"
		displaySuffix = " VPImage"
		fields = binding.VPFields
	}
	path := filepath.Join(workDir, binding.SourceKey+suffix)
	if err := writePNG(path, img); err != nil {
		return preparedUpload{}, err
	}
	return preparedUpload{
		Request: utils.UploadRequest{
			FilePath: path, DisplayName: "Cone " + binding.SourceKey + displaySuffix,
			AssetType: utils.AssetTypeImage,
		},
		JSONFields: fields,
	}, nil
}

func prepareMeshBinding(textures map[string]cachedTexture, binding meshBinding, workDir string) (preparedUpload, error) {
	images := make([]image.Image, 0, len(binding.SourceKeys))
	for _, sourceKey := range binding.SourceKeys {
		texture, exists := textures[sourceKey]
		if !exists || texture.Resized == nil {
			continue
		}
		images = append(images, texture.Resized)
	}
	if len(images) == 0 {
		return preparedUpload{}, fmt.Errorf("build %s mesh: no source textures", binding.Name)
	}
	union := unionAlpha(images, binding.Config.AlphaThreshold)
	mesh, _, err := utils.BuildGreedyMesh(union, binding.Config)
	if err != nil {
		return preparedUpload{}, fmt.Errorf("build %s mesh: %w", binding.Name, err)
	}
	// Open Cloud only accepts native .mesh bytes downloaded from Asset Delivery;
	// it does not import generated geometry through the Mesh endpoint. Import the
	// generated GLB as a Model, then the uploader resolves the contained
	// MeshPart's actual MeshId for the output JSON.
	robloxModel, err := utils.EncodeGeometryGLB(mesh)
	if err != nil {
		return preparedUpload{}, fmt.Errorf("encode %s Roblox GLB: %w", binding.Name, err)
	}
	path := filepath.Join(workDir, binding.Name+".glb")
	if err := os.WriteFile(path, robloxModel, 0o644); err != nil {
		return preparedUpload{}, fmt.Errorf("write %s Roblox GLB: %w", binding.Name, err)
	}
	return preparedUpload{
		Request: utils.UploadRequest{
			FilePath: path, DisplayName: "Cone " + binding.Name + " mesh",
			AssetType: utils.AssetTypeModel, ResolveMeshID: true,
		},
		JSONFields: binding.JSONFields,
		Required:   binding.Name == "sword" || binding.Name == "pickaxe" || binding.Name == "axe",
	}, nil
}

func unionAlpha(images []image.Image, alphaThreshold int) *image.NRGBA {
	result := image.NewNRGBA(image.Rect(0, 0, utils.EdgeExpandedTextureSize, utils.EdgeExpandedTextureSize))
	for _, img := range images {
		for y := 0; y < result.Bounds().Dy(); y++ {
			for x := 0; x < result.Bounds().Dx(); x++ {
				pixel := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
				if int(pixel.A) <= alphaThreshold {
					continue
				}
				offset := result.PixOffset(x, y)
				if pixel.A <= result.Pix[offset+3] {
					continue
				}
				result.Pix[offset] = 255
				result.Pix[offset+1] = 255
				result.Pix[offset+2] = 255
				// The union is a geometry mask, not a texture. Keep it binary so
				// excluded background alpha can never leak into GLB geometry.
				result.Pix[offset+3] = 255
			}
		}
	}
	return result
}

func writePNG(path string, img image.Image) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("create prepared PNG %q: %w", path, err)
	}
	encoder := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := encoder.Encode(file, img); err != nil {
		_ = file.Close()
		return fmt.Errorf("encode prepared PNG %q: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close prepared PNG %q: %w", path, err)
	}
	return nil
}

func parallelFor(ctx context.Context, jobs, workers int, run func(int) error) error {
	if jobs == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if workers < 1 {
		workers = 1
	}
	if workers > jobs {
		workers = jobs
	}
	workContext, cancel := context.WithCancel(ctx)
	defer cancel()
	queue := make(chan int, jobs)
	for index := 0; index < jobs; index++ {
		queue <- index
	}
	close(queue)

	var firstErr error
	var errorOnce sync.Once
	var wait sync.WaitGroup
	wait.Add(workers)
	for worker := 0; worker < workers; worker++ {
		go func() {
			defer wait.Done()
			for {
				select {
				case <-workContext.Done():
					return
				case index, ok := <-queue:
					if !ok {
						return
					}
					if err := run(index); err != nil {
						errorOnce.Do(func() {
							firstErr = err
							cancel()
						})
						return
					}
				}
			}
		}()
	}
	wait.Wait()
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}

func writeExportJSON(path string, output PipelineResult) error {
	data, err := json.Marshal(output)
	if err != nil {
		return fmt.Errorf("encode output JSON: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write output JSON %q: %w", path, err)
	}
	return nil
}

// MarshalJSON emits the port's values as a plain, flat JSON object (e.g.
// {"SkyFront": "12345", ...}) rather than the previous zstd-compressed
// buffer envelope, so the output can be read/edited directly and so any
// field - including the sky fields - is visible without decompressing
// anything.
func (output PipelineResult) MarshalJSON() ([]byte, error) {
	return json.Marshal(output.Values)
}

func availableOutputPath(zipPath string) string {
	directory := filepath.Dir(zipPath)
	stem := strings.TrimSuffix(filepath.Base(zipPath), filepath.Ext(zipPath))
	base := filepath.Join(directory, stem+"_cone.json")
	if _, err := os.Stat(base); os.IsNotExist(err) {
		return base
	}
	for number := 2; ; number++ {
		candidate := filepath.Join(directory, fmt.Sprintf("%s_cone_%d.json", stem, number))
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
}
