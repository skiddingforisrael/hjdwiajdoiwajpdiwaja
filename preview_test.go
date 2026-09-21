package main

import "testing"

func TestChoosePreviewKeysSkipsBowPullFrames(t *testing.T) {
	textures := map[string]string{
		"bow_pulling_0": "a", "bow_pulling_1": "b", "bow_pulling_2": "c",
		"bow_standby": "d", "sky_cloud_top": "e",
	}
	keys := choosePreviewKeys(textures)
	for _, key := range keys {
		if key == "bow_pulling_0" || key == "bow_pulling_1" || key == "bow_pulling_2" {
			t.Fatalf("choosePreviewKeys chose a mid-pull bow frame: %v", keys)
		}
		if key == "sky_cloud_top" {
			t.Fatalf("choosePreviewKeys chose a sky face: %v", keys)
		}
	}
	found := false
	for _, key := range keys {
		if key == "bow_standby" {
			found = true
		}
	}
	if !found {
		t.Fatalf("choosePreviewKeys should have picked the resting bow_standby texture, got %v", keys)
	}
}
