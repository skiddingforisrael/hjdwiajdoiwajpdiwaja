package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDiscordNotifierSendsMessageAndHotbarPreview(t *testing.T) {
	var gotContent string
	var gotPreview []byte
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			t.Errorf("webhook method = %s", request.Method)
		}
		if request.URL.Query().Get("with_components") != "true" {
			t.Errorf("webhook did not enable components: %s", request.URL.RawQuery)
		}
		if err := request.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse webhook multipart: %v", err)
			http.Error(response, "bad multipart", http.StatusBadRequest)
			return
		}
		var payload struct {
			Flags           int `json:"flags"`
			AllowedMentions struct {
				Parse []string `json:"parse"`
			} `json:"allowed_mentions"`
			Attachments []struct {
				ID       int    `json:"id"`
				Filename string `json:"filename"`
			} `json:"attachments"`
			Components []struct {
				Type        int `json:"type"`
				AccentColor int `json:"accent_color"`
				Components  []struct {
					Type    int    `json:"type"`
					Content string `json:"content"`
					Items   []struct {
						Media struct {
							URL string `json:"url"`
						} `json:"media"`
					} `json:"items"`
				} `json:"components"`
			} `json:"components"`
		}
		if err := json.Unmarshal([]byte(request.FormValue("payload_json")), &payload); err != nil {
			t.Errorf("decode webhook payload: %v", err)
		}
		if payload.Flags != discordComponentsV2Flag || len(payload.Components) != 1 || payload.Components[0].Type != 17 || payload.Components[0].AccentColor != discordAccentOrange {
			t.Errorf("unexpected Components V2 payload: %#v", payload)
		}
		if len(payload.Attachments) != 1 || payload.Attachments[0].ID != 0 {
			t.Errorf("unexpected attachment metadata: %#v", payload.Attachments)
		}
		children := payload.Components[0].Components
		wantTypes := []int{10, 14, 10, 14, 10, 12}
		if len(children) != len(wantTypes) {
			t.Errorf("container child count = %d, want %d", len(children), len(wantTypes))
		}
		for index, component := range children {
			if index < len(wantTypes) && component.Type != wantTypes[index] {
				t.Errorf("container child %d type = %d, want %d", index, component.Type, wantTypes[index])
			}
			gotContent += component.Content
			if component.Type == 12 && (len(component.Items) != 1 || component.Items[0].Media.URL != "attachment://"+payload.Attachments[0].Filename) {
				t.Errorf("media gallery does not reference its attachment: %#v", component.Items)
			}
		}
		if len(payload.AllowedMentions.Parse) != 0 {
			t.Errorf("webhook allows mentions: %#v", payload.AllowedMentions.Parse)
		}
		file, _, err := request.FormFile("files[0]")
		if err != nil {
			t.Errorf("read preview attachment: %v", err)
			return
		}
		defer file.Close()
		gotPreview, err = io.ReadAll(file)
		if err != nil {
			t.Errorf("read preview bytes: %v", err)
		}
		response.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	notifier, err := NewDiscordNotifier(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	notifier.client = server.Client()
	output := []byte(`{"SwordMesh":"123","ClayBlue":"456"}`)
	var previewBuffer bytes.Buffer
	previewImage := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	previewImage.SetNRGBA(0, 0, color.NRGBA{R: 255, A: 255})
	if err := png.Encode(&previewBuffer, previewImage); err != nil {
		t.Fatal(err)
	}
	preview := previewBuffer.Bytes()
	if err := notifier.Notify(context.Background(), PortNotification{
		PackID: "0123456789abcdef", OutputJSON: output, PreviewPNG: preview,
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotContent, "Cone Website Logs") || !strings.Contains(gotContent, "0123456789abcdef") {
		t.Fatalf("webhook components omit required content: %q", gotContent)
	}
	// The embed compresses the plain OutputJSON just for Discord's size
	// limit; the stored/served OutputJSON itself stays plain (checked
	// elsewhere), so here just confirm the embedded blob decompresses back
	// to the same values that were passed in.
	const marker = "**Output JSON**: ```"
	start := strings.Index(gotContent, marker)
	if start == -1 {
		t.Fatalf("webhook content missing Output JSON block: %q", gotContent)
	}
	start += len(marker)
	end := strings.Index(gotContent[start:], "```")
	if end == -1 {
		t.Fatalf("webhook content missing closing code fence: %q", gotContent)
	}
	compressed := gotContent[start : start+end]
	decoded, err := decodeCompressedJSON([]byte(compressed))
	if err != nil {
		t.Fatalf("embedded Discord JSON does not decompress: %v", err)
	}
	if decoded["SwordMesh"] != "123" || decoded["ClayBlue"] != "456" {
		t.Fatalf("decompressed Discord embed = %#v, want the original plain values", decoded)
	}
	labeledPreview, err := png.Decode(bytes.NewReader(gotPreview))
	if err != nil {
		t.Fatalf("decode labeled preview: %v", err)
	}
	if labeledPreview.Bounds().Dx() != 4 || labeledPreview.Bounds().Dy() != 4+previewIDFooter {
		t.Fatalf("labeled preview size = %v", labeledPreview.Bounds().Size())
	}
}

func TestDiscordNotifierRejectsOversizedMessage(t *testing.T) {
	notifier, err := NewDiscordNotifier("https://discord.com/api/webhooks/test/test")
	if err != nil {
		t.Fatal(err)
	}
	// The embed is compressed before the size check, so highly repetitive
	// data (like a run of one character) would shrink to almost nothing
	// and never trigger the limit. Use real random bytes instead, which
	// zstd can't meaningfully compress, so the encoded size stays large.
	randomBytes := make([]byte, discordComponentTextLimit*2)
	if _, err := rand.Read(randomBytes); err != nil {
		t.Fatal(err)
	}
	oversized, err := json.Marshal(map[string]string{"Filler": hex.EncodeToString(randomBytes)})
	if err != nil {
		t.Fatal(err)
	}
	err = notifier.Notify(context.Background(), PortNotification{
		PackID: "pack", OutputJSON: oversized, PreviewPNG: []byte("png"),
	})
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("oversized message error = %v", err)
	}
}

func TestDiscordNotifierLabelsBatchPosition(t *testing.T) {
	var content string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if err := request.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
			return
		}
		content = request.FormValue("payload_json")
		response.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	notifier, err := NewDiscordNotifier(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	notifier.client = server.Client()
	var preview bytes.Buffer
	if err := png.Encode(&preview, image.NewNRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	if err := notifier.Notify(context.Background(), PortNotification{
		PackID: "batch-pack", OutputJSON: []byte(`{"t":"buffer"}`), PreviewPNG: preview.Bytes(),
		BatchIndex: 10, BatchTotal: 100,
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, "Cone Batch Logs") || !strings.Contains(content, "BATCHING [10/100]") || strings.Contains(content, "Cone Website Logs") {
		t.Fatalf("batch webhook heading = %s", content)
	}
}
