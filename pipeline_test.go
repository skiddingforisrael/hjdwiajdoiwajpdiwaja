package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/qaustria/AutoPack-Go/utils"
)

type fakeUploader struct {
	requests []utils.UploadRequest
}

func (u *fakeUploader) UploadMany(_ context.Context, requests []utils.UploadRequest) []utils.UploadResult {
	u.requests = append([]utils.UploadRequest(nil), requests...)
	results := make([]utils.UploadResult, len(requests))
	for index, request := range requests {
		results[index] = utils.UploadResult{
			Request: request,
			Asset:   &utils.UploadedAsset{AssetID: fmt.Sprintf("%d", 1000+index)},
		}
	}
	return results
}

type selectiveFailureUploader struct {
	requests []utils.UploadRequest
	failures map[string]string
}

type artifactCaptureUploader struct {
	files map[string][]byte
}

func (u *artifactCaptureUploader) UploadMany(_ context.Context, requests []utils.UploadRequest) []utils.UploadResult {
	u.files = make(map[string][]byte, len(requests))
	results := make([]utils.UploadResult, len(requests))
	for index, request := range requests {
		data, err := os.ReadFile(request.FilePath)
		if err != nil {
			results[index] = utils.UploadResult{Request: request, Error: err.Error()}
			continue
		}
		u.files[request.DisplayName] = data
		results[index] = utils.UploadResult{Request: request, Asset: &utils.UploadedAsset{AssetID: fmt.Sprintf("%d", index+1)}}
	}
	return results
}

func (u *selectiveFailureUploader) UploadMany(_ context.Context, requests []utils.UploadRequest) []utils.UploadResult {
	u.requests = append([]utils.UploadRequest(nil), requests...)
	results := make([]utils.UploadResult, len(requests))
	for index, request := range requests {
		results[index].Request = request
		if message, failed := u.failures[request.DisplayName]; failed {
			results[index].Error = message
			continue
		}
		results[index].Asset = &utils.UploadedAsset{AssetID: fmt.Sprintf("uploaded-%d", index)}
	}
	return results
}

func TestRunPipelineBuildsSchemaAndDeduplicatesSharedMeshes(t *testing.T) {
	zipPath := filepath.Join(t.TempDir(), "pack.zip")
	writeCompletePack(t, zipPath)
	uploader := &fakeUploader{}
	output, err := RunPipeline(context.Background(), zipPath, uploader, nil)
	if err != nil {
		t.Fatal(err)
	}
	if output.Values["BlocksVPImage"] != "0" {
		t.Fatalf("BlocksVPImage = %q", output.Values["BlocksVPImage"])
	}
	if rotation, ok := output.Values["GoldAppleRotation"].([]int); !ok || !reflect.DeepEqual(rotation, []int{0, -90, 0}) {
		t.Fatalf("GoldAppleRotation = %#v, want [0 -90 0]", output.Values["GoldAppleRotation"])
	}
	if rotation := output.Values["DiamondRotation"]; !reflect.DeepEqual(rotation, []any{float64(0), float64(0), float64(35)}) {
		t.Fatalf("DiamondRotation = %#v, want default [0 0 35] and never -90", rotation)
	}
	if rotation, ok := output.Values["ShearsRotation"].([]int); !ok || !reflect.DeepEqual(rotation, []int{0, 0, -35}) {
		t.Fatalf("ShearsRotation = %#v, want [0 0 -35]", output.Values["ShearsRotation"])
	}
	for _, forbidden := range []string{"Moon", "Sun", "SkyBoxRotationSpeed"} {
		if _, exists := output.Values[forbidden]; exists {
			t.Fatalf("unimplemented field %q should not be exported", forbidden)
		}
	}
	for field, expected := range map[string][]any{
		"SwordScale":       {float64(2), float64(2), float64(2)},
		"PickaxeScale":     {float64(2), float64(2), float64(2)},
		"DiamondScale":     {float64(1.75), float64(1.75), float64(1.75)},
		"DefaultBowScale":  {float64(2.25), float64(2.25), float64(2.25)},
		"JumpPotionScale":  {float64(2), float64(2), float64(2)},
		"MeteorStaffScale": {float64(2), float64(2), float64(2)},
	} {
		if !reflect.DeepEqual(output.Values[field], expected) {
			t.Fatalf("%s = %#v, want default %#v", field, output.Values[field], expected)
		}
	}
	for _, field := range []string{"AmethystSwordTexture", "MeteorStaffMesh", "RegenStaffMesh", "SpeedStaffMesh", "WindStaffMesh"} {
		if output.Values[field] == "" {
			t.Fatalf("default item %s is missing", field)
		}
	}
	if output.Values["SwordMesh"] == "" || output.Values["PickaxeMesh"] == "" || output.Values["AxeMesh"] == "" {
		t.Fatalf("shared mesh fields missing: %#v", output.Values)
	}
	if output.Values["JumpPotionMesh"] != output.Values["SpeedPotionMesh"] {
		t.Fatalf("potion meshes are not shared: %q != %q", output.Values["JumpPotionMesh"], output.Values["SpeedPotionMesh"])
	}
	preview, err := png.Decode(bytes.NewReader(output.PreviewPNG))
	if err != nil {
		t.Fatalf("decode hotbar preview: %v", err)
	}
	if preview.Bounds().Dx() != 440 || preview.Bounds().Dy() != 128 {
		t.Fatalf("hotbar preview size = %v, want 440x128", preview.Bounds().Size())
	}
	if output.Values["GoldSwordTexture"] == "" || output.Values["GoldSwordVPImage"] == "" {
		t.Fatal("iron sword did not populate GoldSword fields")
	}
	if output.Values["PickaxeTexture"] == "88288870749393" || output.Values["AxeTexture"] == "109633191239964" {
		t.Fatal("stone tool textures retained template IDs instead of the uploaded pack textures")
	}
	if output.Values["ClayBlue"] == "" || output.Values["ClayRed"] == "" || output.Values["ClayWhite"] == "" {
		t.Fatal("wool did not populate Clay fields")
	}
	meshes := make(map[string]int)
	for _, request := range uploader.requests {
		if request.ResolveMeshID {
			meshes[request.DisplayName]++
			if request.AssetType != utils.AssetTypeModel || filepath.Ext(request.FilePath) != ".glb" {
				t.Fatalf("mesh import %q uses type %s at %q, want Model .glb", request.DisplayName, request.AssetType, request.FilePath)
			}
		}
	}
	for _, name := range []string{"Cone sword mesh", "Cone pickaxe mesh", "Cone axe mesh", "Cone potion mesh"} {
		if meshes[name] != 1 {
			t.Fatalf("%q upload count = %d, want one", name, meshes[name])
		}
	}
	// Discord's notifier now compresses the pack JSON on the fly just for
	// its own embed (see discord.go), independently of the plain JSON this
	// pipeline returns everywhere else - so check that compressed form,
	// not the plain output, against Discord's component limit.
	compactJSON, err := encodeCompressedJSON(output.Values)
	if err != nil {
		t.Fatal(err)
	}
	if len(compactJSON) > discordComponentTextLimit {
		t.Fatalf("compressed full template does not fit in a Discord component: %d characters", len(compactJSON))
	}
	// Every prepared upload lives under the extracted temporary directory and
	// must be gone when RunPipeline returns.
	for _, request := range uploader.requests {
		if _, err := os.Stat(request.FilePath); !os.IsNotExist(err) {
			t.Fatalf("temporary upload still exists: %s (%v)", request.FilePath, err)
		}
	}
}

func TestRunPipelineSkipsFailedUploadsAndKeepsDefaults(t *testing.T) {
	zipPath := filepath.Join(t.TempDir(), "pack.zip")
	writeCompletePack(t, zipPath)
	uploader := &selectiveFailureUploader{failures: map[string]string{
		"Cone shears mesh":   "Roblox returned 500 for shears",
		"Cone bow_1 mesh":    "Roblox returned 429 for bow 1",
		"Cone bow_2 mesh":    "Roblox returned 429 for bow 2",
		"Cone fireball mesh": "Roblox returned 400 for fireball",
	}}
	defaults, err := defaultPipelineValues()
	if err != nil {
		t.Fatal(err)
	}
	var logs []string
	output, err := runPipeline(context.Background(), zipPath, uploader, nil, func(message string) {
		logs = append(logs, message)
	})
	if err != nil {
		t.Fatalf("pipeline treated independent upload failures as fatal: %v", err)
	}
	for _, field := range []string{"ShearsMesh", "Bow1Mesh", "Bow2Mesh", "FireballMesh"} {
		if !reflect.DeepEqual(output.Values[field], defaults[field]) {
			t.Fatalf("%s = %#v, want retained default %#v", field, output.Values[field], defaults[field])
		}
	}
	if reflect.DeepEqual(output.Values["SwordMesh"], defaults["SwordMesh"]) {
		t.Fatal("successful SwordMesh upload did not replace the default")
	}
	logText := strings.Join(logs, "\n")
	for _, expected := range []string{
		"Skipped Cone shears mesh; using default for ShearsMesh. Roblox error: Roblox returned 500 for shears",
		"Skipped Cone bow_1 mesh; using default for Bow1Mesh. Roblox error: Roblox returned 429 for bow 1",
		"Roblox accepted 67 assets; skipped 4 and kept their default asset IDs",
	} {
		if !strings.Contains(logText, expected) {
			t.Fatalf("pipeline log does not contain %q:\n%s", expected, logText)
		}
	}
}

func TestRunPipelineRefusesBrokenCoreToolMeshes(t *testing.T) {
	zipPath := filepath.Join(t.TempDir(), "pack.zip")
	writeCompletePack(t, zipPath)
	uploader := &selectiveFailureUploader{failures: map[string]string{
		"Cone sword mesh": "Roblox rejected sword geometry",
	}}
	var logs []string
	_, err := runPipeline(context.Background(), zipPath, uploader, nil, func(message string) {
		logs = append(logs, message)
	})
	if err == nil || !strings.Contains(err.Error(), "refused to return a broken pack") {
		t.Fatalf("core mesh failure = %v, want broken-pack rejection", err)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "Required Cone sword mesh failed") {
		t.Fatalf("core mesh failure was not logged clearly:\n%s", strings.Join(logs, "\n"))
	}
}

func TestRunPipelineRemovesCanvasWideAlphaNoiseFromUploadArtifacts(t *testing.T) {
	zipPath := filepath.Join(t.TempDir(), "noisy-pack.zip")
	files := completePackImages()
	for _, name := range []string{"stone_sword", "diamond_sword", "iron_sword", "wood_sword"} {
		files["Wrapper/assets/minecraft/textures/items/"+name+".png"] = noisyTestTexture(2)
	}
	writePackImages(t, zipPath, files)
	uploader := &artifactCaptureUploader{}
	if _, err := RunPipeline(context.Background(), zipPath, uploader, nil); err != nil {
		t.Fatal(err)
	}

	vp, err := png.Decode(bytes.NewReader(uploader.files["Cone diamond_sword VPImage"]))
	if err != nil {
		t.Fatal(err)
	}
	lowAlpha := 0
	transparent := 0
	for y := vp.Bounds().Min.Y; y < vp.Bounds().Max.Y; y++ {
		for x := vp.Bounds().Min.X; x < vp.Bounds().Max.X; x++ {
			_, _, _, alpha16 := vp.At(x, y).RGBA()
			alpha := int(alpha16 >> 8)
			if alpha == 0 {
				transparent++
			} else if alpha <= 8 {
				lowAlpha++
			}
		}
	}
	if lowAlpha != 0 || transparent == 0 {
		t.Fatalf("uploaded sword VP background: transparent=%d low-alpha=%d", transparent, lowAlpha)
	}
	glb := uploader.files["Cone sword mesh"]
	if len(glb) < 28 || binary.LittleEndian.Uint32(glb[:4]) != 0x46546c67 {
		t.Fatalf("sword GLB is invalid or too short: %d bytes", len(glb))
	}
	jsonLength := int(binary.LittleEndian.Uint32(glb[12:16]))
	var document struct {
		BufferViews []struct {
			ByteOffset int `json:"byteOffset"`
		} `json:"bufferViews"`
		Accessors []struct {
			BufferView int `json:"bufferView"`
			Count      int `json:"count"`
		} `json:"accessors"`
	}
	if err := json.Unmarshal(glb[20:20+jsonLength], &document); err != nil {
		t.Fatal(err)
	}
	uvAccessor := document.Accessors[2]
	uvStart := 20 + jsonLength + 8 + document.BufferViews[uvAccessor.BufferView].ByteOffset
	minU, maxU := float32(1), float32(0)
	for vertex := 0; vertex < uvAccessor.Count; vertex++ {
		offset := uvStart + vertex*8
		u := math.Float32frombits(binary.LittleEndian.Uint32(glb[offset : offset+4]))
		if u < minU {
			minU = u
		}
		if u > maxU {
			maxU = u
		}
	}
	if minU <= 0 || maxU >= 1 {
		t.Fatalf("sword mesh spans the full noisy canvas: U=%g..%g", minU, maxU)
	}
}

func TestPipelineMeshesKeepOriginalOrientationWithAxeHalfTurn(t *testing.T) {
	bindings := pipelineMeshes()
	wantZ := map[string]float64{
		"sword": 0, "pickaxe": 0, "bow_0": 0,
		"gold_apple": 0, "axe": 180, "shears": 0,
	}
	for _, binding := range bindings {
		if expected, exists := wantZ[binding.Name]; exists && binding.Config.RotateZ != expected {
			t.Fatalf("%s RotateZ = %v, want %v", binding.Name, binding.Config.RotateZ, expected)
		}
	}
	var sword, axe, shears utils.Config
	for _, binding := range bindings {
		switch binding.Name {
		case "sword":
			sword = binding.Config
		case "axe":
			axe = binding.Config
		case "shears":
			shears = binding.Config
		}
	}
	if axe.RotateX != sword.RotateX || axe.RotateY != sword.RotateY || axe.RotateZ != sword.RotateZ+180 {
		t.Fatalf("axe rotation = (%v, %v, %v), want sword tilt with 180-degree yaw (%v, %v, %v)",
			axe.RotateX, axe.RotateY, axe.RotateZ, sword.RotateX, sword.RotateY, sword.RotateZ+180)
	}
	if shears != sword {
		t.Fatalf("shears mesh config = %+v, want unmodified standard config %+v", shears, sword)
	}
}

func TestGoldenAppleMeshHasNoCustomRotationOffset(t *testing.T) {
	for _, binding := range pipelineMeshes() {
		if binding.Name != "gold_apple" {
			continue
		}
		if binding.Config.RotateX != 90 || binding.Config.RotateY != 0 || binding.Config.RotateZ != 0 {
			t.Fatalf("gold apple mesh rotation = (%v, %v, %v), want unchanged flat config (90, 0, 0)",
				binding.Config.RotateX, binding.Config.RotateY, binding.Config.RotateZ)
		}
		return
	}
	t.Fatal("gold apple mesh binding is missing")
}

func TestPotionMeshUsesUprightFlatOrientation(t *testing.T) {
	found := 0
	for _, binding := range pipelineMeshes() {
		if binding.Name != "potion" && binding.Name != "fireball" {
			continue
		}
		found++
		if binding.Config.RotateX != 90 || binding.Config.RotateY != 0 || binding.Config.RotateZ != 0 {
			t.Fatalf("%s mesh rotation = (%v, %v, %v), want upright flat config (90, 0, 0)",
				binding.Name, binding.Config.RotateX, binding.Config.RotateY, binding.Config.RotateZ)
		}
	}
	if found != 2 {
		t.Fatalf("found %d upright flat bindings, want potion and fireball", found)
	}
}

func TestWriteExportJSONIsPlainFlatJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "result.json")
	output := PipelineResult{Values: map[string]any{
		"BlocksVPImage": "0", "SwordMesh": "123", "ClayBlue": "456",
		"GoldAppleRotation": []int{0, -90, 0},
	}}
	if err := writeExportJSON(path, output); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("output is not strict flat JSON: %v", err)
	}
	if decoded["SwordMesh"] != "123" || decoded["ClayBlue"] != "456" || decoded["BlocksVPImage"] != "0" {
		t.Fatalf("expected plain, directly-readable field values, got %#v", decoded)
	}
	if !reflect.DeepEqual(decoded["GoldAppleRotation"], []any{float64(0), float64(-90), float64(0)}) {
		t.Fatalf("GoldAppleRotation = %#v, want [0 -90 0]", decoded["GoldAppleRotation"])
	}
}

func TestRunPipelinePortsPartialPackWithoutError(t *testing.T) {
	zipPath := filepath.Join(t.TempDir(), "partial.zip")
	writePackImages(t, zipPath, map[string]image.Image{
		"assets/minecraft/textures/items/diamond_sword.png": testTexture(color.NRGBA{B: 255, A: 255}),
	})
	uploader := &fakeUploader{}
	_, err := RunPipeline(context.Background(), zipPath, uploader, nil)
	if err != nil {
		t.Fatalf("pipeline should port whatever textures are present, got error: %v", err)
	}
	if len(uploader.requests) == 0 {
		t.Fatal("pipeline uploaded nothing despite a valid diamond_sword texture being present")
	}
	for _, request := range uploader.requests {
		name := strings.ToLower(request.DisplayName)
		if strings.Contains(name, "emerald") || strings.Contains(name, "iron") || strings.Contains(name, "bow") {
			t.Fatalf("pipeline uploaded an asset for a texture that was never provided: %s", request.DisplayName)
		}
	}
}

func TestRunPipelineZeroesFieldsForMissingOptionalTextures(t *testing.T) {
	zipPath := filepath.Join(t.TempDir(), "optional-missing.zip")
	files := completePackImages()
	for _, name := range []string{"emerald", "fireball", "shears"} {
		delete(files, "Wrapper/assets/minecraft/textures/items/"+name+".png")
	}
	writePackImages(t, zipPath, files)
	uploader := &fakeUploader{}
	var logs []string
	output, err := runPipeline(context.Background(), zipPath, uploader, nil, func(message string) {
		logs = append(logs, message)
	})
	if err != nil {
		t.Fatalf("missing optional textures failed the pack: %v", err)
	}
	for _, field := range []string{"EmeraldTexture", "EmeraldVPImage", "EmeraldMesh", "FireballTexture", "FireballVPImage", "FireballMesh", "ShearsTexture", "ShearsVPImage", "ShearsMesh"} {
		if output.Values[field] != "0" {
			t.Fatalf("%s = %#v, want \"0\" for a texture absent from the pack", field, output.Values[field])
		}
	}
	for _, request := range uploader.requests {
		name := strings.ToLower(request.DisplayName)
		if strings.Contains(name, "emerald") || strings.Contains(name, "fireball") || strings.Contains(name, "shears") {
			t.Fatalf("optional missing texture was uploaded: %s", request.DisplayName)
		}
	}
	logText := strings.Join(logs, "\n")
	if !strings.Contains(logText, "No texture provided, using 0 (no asset) for:") ||
		!strings.Contains(logText, "emerald") || !strings.Contains(logText, "fireball") || !strings.Contains(logText, "shears") {
		t.Fatalf("missing texture defaults were not logged:\n%s", logText)
	}
}

func completePackImages() map[string]image.Image {
	files := make(map[string]image.Image)
	items := []string{
		"stone_sword", "diamond_sword", "iron_sword", "wood_sword",
		"stone_pickaxe", "diamond_pickaxe", "iron_pickaxe", "wood_pickaxe",
		"stone_axe", "diamond_axe", "iron_axe", "wood_axe",
		"bow_standby", "bow_pulling_0", "bow_pulling_1", "bow_pulling_2",
		"apple_golden", "iron_ingot", "diamond", "emerald", "ender_pearl", "shears", "fireball",
	}
	for index, name := range items {
		files["Wrapper/assets/minecraft/textures/items/"+name+".png"] = testTexture(color.NRGBA{
			R: uint8(20 + index), G: 100, B: 180, A: 255,
		})
	}
	files["Wrapper/assets/minecraft/textures/items/potion_overlay.png"] = testTexture(color.NRGBA{R: 255, G: 255, B: 255, A: 255})
	files["Wrapper/assets/minecraft/textures/items/potion_bottle_drinkable.png"] = image.NewNRGBA(image.Rect(0, 0, 16, 16))
	for _, name := range []string{"blue", "cyan", "green", "gray", "orange", "purple", "red", "white", "yellow"} {
		files["Wrapper/assets/minecraft/textures/blocks/wool_colored_"+name+".png"] = testTexture(color.NRGBA{R: 160, G: 80, B: 40, A: 255})
	}
	return files
}

func writeCompletePack(t *testing.T, path string) {
	t.Helper()
	writePackImages(t, path, completePackImages())
}

func writePackImages(t *testing.T, path string, files map[string]image.Image) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(file)
	for name, img := range files {
		entry, err := archive.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := png.Encode(entry, img); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func testTexture(pixel color.NRGBA) image.Image {
	img := image.NewNRGBA(image.Rect(0, 0, 16, 16))
	for y := 3; y < 13; y++ {
		for x := 5; x < 11; x++ {
			img.SetNRGBA(x, y, pixel)
		}
	}
	return img
}

func noisyTestTexture(backgroundAlpha uint8) image.Image {
	img := image.NewNRGBA(image.Rect(0, 0, 16, 16))
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 240, G: 240, B: 240, A: backgroundAlpha})
		}
	}
	for y := 3; y < 13; y++ {
		for x := 5; x < 11; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 80, G: 20, B: 140, A: 255})
		}
	}
	return img
}
