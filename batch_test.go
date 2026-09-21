package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestGoogleSheetCSVURL(t *testing.T) {
	got, ok := googleSheetCSVURL("https://docs.google.com/spreadsheets/d/sheet-id/edit?usp=sharing#gid=123")
	if !ok {
		t.Fatal("Google Sheet URL was not recognized")
	}
	want := "https://docs.google.com/spreadsheets/d/sheet-id/gviz/tq?tqx=out:csv&gid=123"
	if got != want {
		t.Fatalf("CSV URL = %q, want %q", got, want)
	}
}

func TestParseBatchRecordsUsesNamesAndDeduplicatesLinks(t *testing.T) {
	records := [][]string{
		{"Pack Name", "Download"},
		{"First Pack", "https://example.com/first.zip"},
		{"Duplicate", "https://example.com/first.zip"},
		{"Second Pack", "Get it at https://example.com/second.zip."},
	}
	queue := parseBatchRecords(records)
	if len(queue) != 2 {
		t.Fatalf("queue length = %d, want 2: %#v", len(queue), queue)
	}
	if queue[0].Name != "First Pack" || queue[0].Row != 2 || queue[0].Source != "https://example.com/first.zip" {
		t.Fatalf("first queue entry = %#v", queue[0])
	}
	if queue[1].Name != "Second Pack" || queue[1].Source != "https://example.com/second.zip" {
		t.Fatalf("second queue entry = %#v", queue[1])
	}
}

func TestMediaFireDirectDownloadURL(t *testing.T) {
	page := []byte(`<a id="downloadButton" href="https://download123.mediafire.com/token/file/pack.zip?x=1&amp;y=2">Download</a>`)
	got := mediaFireDirectDownloadURL(page)
	want := "https://download123.mediafire.com/token/file/pack.zip?x=1&y=2"
	if got != want {
		t.Fatalf("direct MediaFire URL = %q, want %q", got, want)
	}
}

func TestMediaFireFolderKeyAndSourceIdentity(t *testing.T) {
	key, ok := mediaFireFolderKey("https://www.mediafire.com/folder/abc123/packs#myfiles")
	if !ok || key != "abc123" {
		t.Fatalf("folder key = %q, ok=%v", key, ok)
	}
	first := batchSourceIdentity("https://www.mediafire.com/file/quickkey/one.zip/file")
	second := batchSourceIdentity("https://www.mediafire.com/file/quickkey/two.zip/file")
	if first != second {
		t.Fatalf("same MediaFire quickkey identities differ: %q != %q", first, second)
	}
}

func TestBatchAuthenticationFailed(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "unauthorized", err: &batchServerError{StatusCode: http.StatusUnauthorized}, want: true},
		{name: "forbidden", err: fmt.Errorf("submit pack: %w", &batchServerError{StatusCode: http.StatusForbidden}), want: true},
		{name: "bad pack", err: &batchServerError{StatusCode: http.StatusBadRequest}, want: false},
		{name: "download", err: errors.New("download failed"), want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := batchAuthenticationFailed(test.err); got != test.want {
				t.Fatalf("batchAuthenticationFailed() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestRunBatchQueuesSequentiallyAndResumes(t *testing.T) {
	zipBytes := testBatchZIP(t)
	zipPath := filepath.Join(t.TempDir(), "pack.zip")
	if err := os.WriteFile(zipPath, zipBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	expectedPackID, err := texturePackID(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	var baseURL string
	var requests atomic.Int32
	var active atomic.Int32
	var maximumActive atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/packs.csv":
			response.Header().Set("Content-Type", "text/csv")
			_, _ = fmt.Fprintf(response, "Pack Name,Download\nOne,%s/one.zip\nTwo,%s/two.zip\n", baseURL, baseURL)
		case "/one.zip", "/two.zip":
			response.Header().Set("Content-Type", "application/zip")
			response.Header().Set("Content-Disposition", `attachment; filename="`+strings.TrimPrefix(request.URL.Path, "/")+`"`)
			_, _ = response.Write(zipBytes)
		case "/api/convert":
			if request.Header.Get(robloxAPIKeyHeader) != "test-api-key" || request.Header.Get(robloxUserIDHeader) != "12345" {
				http.Error(response, "bad credentials", http.StatusUnauthorized)
				return
			}
			if request.Header.Get(batchTokenHeader) != "test-batch-token-with-at-least-32-characters" {
				http.Error(response, "bad batch token", http.StatusForbidden)
				return
			}
			current := active.Add(1)
			defer active.Add(-1)
			for {
				previous := maximumActive.Load()
				if current <= previous || maximumActive.CompareAndSwap(previous, current) {
					break
				}
			}
			part, _, err := texturePackPart(request)
			if err != nil {
				http.Error(response, err.Error(), http.StatusBadRequest)
				return
			}
			if _, err := io.Copy(io.Discard, part); err != nil {
				http.Error(response, err.Error(), http.StatusBadRequest)
				return
			}
			requestNumber := requests.Add(1)
			response.Header().Set("Content-Type", "application/x-ndjson")
			_, _ = fmt.Fprintf(response, "{\"type\":\"progress\",\"progress\":{\"message\":\"Porting %d\"}}\n", requestNumber)
			_, _ = fmt.Fprintf(response, "{\"type\":\"result\",\"packId\":%q,\"result\":{\"m\":null,\"t\":\"buffer\",\"zbase64\":\"data-%d\"}}\n", expectedPackID, requestNumber)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	baseURL = server.URL

	t.Setenv("ROBLOX_API_KEY", "test-api-key")
	t.Setenv("ROBLOX_USER_ID", "12345")
	t.Setenv("ROBLOX_GROUP_ID", "")
	t.Setenv("CONE_BATCH_TOKEN", "test-batch-token-with-at-least-32-characters")
	t.Setenv("CONE_BATCH_ENDPOINT", server.URL+"/api/convert")
	temporary := t.TempDir()
	statePath := filepath.Join(temporary, "state", "queue.json")
	outputDir := filepath.Join(temporary, "output")
	t.Setenv("CONE_BATCH_STATE_PATH", statePath)
	t.Setenv("CONE_BATCH_OUTPUT_DIR", outputDir)

	if err := runBatch(context.Background(), []string{server.URL + "/packs.csv"}); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 2 || maximumActive.Load() != 1 {
		t.Fatalf("port requests = %d, maximum active = %d; want 2 and 1", requests.Load(), maximumActive.Load())
	}
	state, err := loadBatchState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Completed) != 2 {
		t.Fatalf("completed state entries = %d, want 2", len(state.Completed))
	}
	outputs, err := filepath.Glob(filepath.Join(outputDir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(outputs) != 2 {
		t.Fatalf("output files = %d, want 2", len(outputs))
	}
	for _, path := range outputs {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !json.Valid(data) || !bytes.Contains(data, []byte(`"zbase64"`)) {
			t.Fatalf("invalid output %s: %s", path, data)
		}
	}

	if err := runBatch(context.Background(), []string{server.URL + "/packs.csv"}); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 2 {
		t.Fatalf("resume submitted completed packs; requests = %d, want 2", requests.Load())
	}
}

func testBatchZIP(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	file, err := writer.Create("assets/minecraft/textures/items/diamond_sword.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("test")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}
