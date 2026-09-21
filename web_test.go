package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qaustria/AutoPack-Go/packstore"
)

type stubWebProcessor struct {
	filename string
	payload  string
}

type capturedCredentials struct {
	apiKey string
	userID string
}

type stubPortNotifier struct {
	notification PortNotification
}

type gatedWebProcessor struct {
	started chan<- struct{}
	release <-chan struct{}
}

func (processor *gatedWebProcessor) ProcessUpload(_ context.Context, _ string, _ io.Reader, _ ProgressFunc) (PipelineResult, error) {
	processor.started <- struct{}{}
	<-processor.release
	return PipelineResult{
		Values: map[string]any{"SwordMesh": "123"}, PackID: "pack", PackName: "pack.zip",
	}, nil
}

func (notifier *stubPortNotifier) Notify(_ context.Context, notification PortNotification) error {
	notifier.notification = notification
	return nil
}

func (processor *stubWebProcessor) ProcessUpload(_ context.Context, filename string, upload io.Reader, progress ProgressFunc) (PipelineResult, error) {
	data, err := io.ReadAll(upload)
	if err != nil {
		return PipelineResult{}, err
	}
	processor.filename = filename
	processor.payload = string(data)
	progress(ProgressEvent{Stage: ProgressReceiving, Message: "Receiving test.zip"})
	progress(ProgressEvent{Stage: ProgressUploading, Completed: 1, Total: 1, Name: "Cone test mesh", Message: "Uploaded Cone test mesh"})
	progress(ProgressEvent{Stage: ProgressComplete, Completed: 1, Total: 1, Message: "Generated JSON mapping"})
	return PipelineResult{
		Values: map[string]any{
			"SwordMesh": "123", "ShearsRotation": []int{0, 0, -35},
		},
		PackID: "0123456789abcdef", PackName: filename, PreviewPNG: []byte("preview"),
	}, nil
}

func TestWebHandlerServesFrontendWithSecurityHeaders(t *testing.T) {
	handler, err := NewWebHandler(&stubWebProcessor{})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("GET / status = %d", response.Code)
	}
	if !strings.Contains(response.Body.String(), "Drop your texture pack here") {
		t.Fatalf("frontend body is unexpected: %s", response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `id="activity-log"`) {
		t.Fatal("frontend is missing its live porting log")
	}
	if !strings.Contains(response.Body.String(), `id="result-modal"`) || !strings.Contains(response.Body.String(), `id="copy-button"`) || !strings.Contains(response.Body.String(), `id="close-result-button"`) {
		t.Fatal("frontend is missing its centered JSON result modal or actions")
	}
	if !strings.Contains(response.Body.String(), `id="api-key-input"`) || !strings.Contains(response.Body.String(), `id="user-id-input"`) || !strings.Contains(response.Body.String(), "Assets: Read + Write") {
		t.Fatal("frontend is missing Roblox credential fields or API-key instructions")
	}
	if !strings.Contains(response.Body.String(), `id="secret-toggle"`) {
		t.Fatal("frontend is missing its API-key visibility control")
	}
	if !strings.Contains(response.Body.String(), "Minecraft 1.8.9") || !strings.Contains(response.Body.String(), "BridgeDuel") || strings.Contains(response.Body.String(), "Cone texture pack porter") {
		t.Fatal("frontend branding does not identify Cone and the BridgeDuel conversion")
	}
	if !strings.Contains(response.Body.String(), "Leave IP restriction off.") || !strings.Contains(response.Body.String(), `id="remember-credentials"`) {
		t.Fatal("frontend is missing its IP instruction or browser credential-memory control")
	}
	if !strings.Contains(response.Body.String(), `class="open-source-note"`) {
		t.Fatal("frontend is missing its open-source note")
	}
	if strings.Contains(response.Body.String(), "Download JSON") {
		t.Fatal("frontend still offers a JSON download instead of showing the result")
	}
	if !strings.Contains(response.Header().Get("Content-Security-Policy"), "default-src 'self'") {
		t.Fatal("frontend is missing its content security policy")
	}

	iconRequest := httptest.NewRequest(http.MethodGet, "/cone.png", nil)
	iconResponse := httptest.NewRecorder()
	handler.ServeHTTP(iconResponse, iconRequest)
	if iconResponse.Code != http.StatusOK || !strings.HasPrefix(iconResponse.Header().Get("Content-Type"), "image/png") {
		t.Fatalf("GET /cone.png = %d %q", iconResponse.Code, iconResponse.Header().Get("Content-Type"))
	}
	for _, path := range []string{"/cone_accepted.png", "/cone_error.png"} {
		stateRequest := httptest.NewRequest(http.MethodGet, path, nil)
		stateResponse := httptest.NewRecorder()
		handler.ServeHTTP(stateResponse, stateRequest)
		if stateResponse.Code != http.StatusOK || !strings.HasPrefix(stateResponse.Header().Get("Content-Type"), "image/png") {
			t.Fatalf("GET %s = %d %q", path, stateResponse.Code, stateResponse.Header().Get("Content-Type"))
		}
	}
	fontRequest := httptest.NewRequest(http.MethodGet, "/fonts/inter-var.woff2", nil)
	fontResponse := httptest.NewRecorder()
	handler.ServeHTTP(fontResponse, fontRequest)
	if fontResponse.Code != http.StatusOK || !strings.Contains(fontResponse.Header().Get("Content-Type"), "font/woff2") {
		t.Fatalf("GET font = %d %q", fontResponse.Code, fontResponse.Header().Get("Content-Type"))
	}
	for _, path := range []string{"/fonts/FredokaOne-Regular.ttf", "/fonts/LuckiestGuy-Regular.ttf"} {
		fontRequest := httptest.NewRequest(http.MethodGet, path, nil)
		fontResponse := httptest.NewRecorder()
		handler.ServeHTTP(fontResponse, fontRequest)
		if fontResponse.Code != http.StatusOK || !strings.Contains(fontResponse.Header().Get("Content-Type"), "font/ttf") {
			t.Fatalf("GET %s = %d %q", path, fontResponse.Code, fontResponse.Header().Get("Content-Type"))
		}
	}
	for path, marker := range map[string]string{
		"/styles.css": ".result-modal",
		"/app.js":     `classList.add("modal-open")`,
	} {
		assetRequest := httptest.NewRequest(http.MethodGet, path, nil)
		assetResponse := httptest.NewRecorder()
		handler.ServeHTTP(assetResponse, assetRequest)
		if assetResponse.Code != http.StatusOK || !strings.Contains(assetResponse.Body.String(), marker) {
			t.Fatalf("GET %s omits completed-layout marker %q", path, marker)
		}
	}
}

func TestWebHandlerStreamsProgressAndJSONResult(t *testing.T) {
	processor := &stubWebProcessor{}
	handler, err := NewWebHandler(processor)
	if err != nil {
		t.Fatal(err)
	}
	body := new(bytes.Buffer)
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("pack", "Fractal 512x.zip")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(part, "test ZIP bytes"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/convert", body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("POST /api/convert status = %d: %s", response.Code, response.Body.String())
	}
	if processor.filename != "Fractal 512x.zip" || processor.payload != "test ZIP bytes" {
		t.Fatalf("processor received filename=%q payload=%q", processor.filename, processor.payload)
	}
	lines := strings.Split(strings.TrimSpace(response.Body.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("streamed event count = %d: %s", len(lines), response.Body.String())
	}
	var last struct {
		Type     string         `json:"type"`
		Filename string         `json:"filename"`
		PackID   string         `json:"packId"`
		Result   map[string]any `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil {
		t.Fatal(err)
	}
	if last.Type != "result" || last.Filename != "Fractal 512x_cone.json" || last.PackID != "0123456789abcdef" {
		t.Fatalf("unexpected result event: %#v", last)
	}
	if _, isEnvelope := last.Result["zbase64"]; isEnvelope {
		t.Fatalf("result should be plain flat JSON, not a compressed envelope: %#v", last.Result)
	}
	if last.Result["SwordMesh"] != "123" {
		t.Fatalf("plain web result = %#v", last.Result)
	}
}

func TestCredentialWebHandlerUsesOnlyRequestCredentials(t *testing.T) {
	processor := &stubWebProcessor{}
	captured := &capturedCredentials{}
	notifier := &stubPortNotifier{}
	store, err := packstore.Open(filepath.Join(t.TempDir(), "cone.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const batchToken = "test-batch-token-with-at-least-32-characters"
	handler, err := NewCredentialWebHandlerWithOptions(func(_ context.Context, apiKey, userID string) (uploadProcessor, error) {
		captured.apiKey = apiKey
		captured.userID = userID
		return processor, nil
	}, WebHandlerOptions{Notifier: notifier, Store: store, MaxConcurrentPorts: 2, BatchToken: batchToken})
	if err != nil {
		t.Fatal(err)
	}
	body := new(bytes.Buffer)
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("pack", "public.zip")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(part, "zip"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/convert", body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set(robloxAPIKeyHeader, "request-secret")
	request.Header.Set(robloxUserIDHeader, "123456")
	request.Header.Set(batchIndexHeader, "10")
	request.Header.Set(batchTotalHeader, "100")
	request.Header.Set(batchTokenHeader, batchToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("credential conversion status = %d: %s", response.Code, response.Body.String())
	}
	if captured.apiKey != "request-secret" || captured.userID != "123456" {
		t.Fatalf("factory got API key %q and user ID %q", captured.apiKey, captured.userID)
	}
	if notifier.notification.PackID != "0123456789abcdef" || string(notifier.notification.PreviewPNG) != "preview" {
		t.Fatalf("webhook notification = %#v", notifier.notification)
	}
	if notifier.notification.BatchIndex != 10 || notifier.notification.BatchTotal != 100 {
		t.Fatalf("webhook batch position = %d/%d", notifier.notification.BatchIndex, notifier.notification.BatchTotal)
	}
	var decoded map[string]any
	if err := json.Unmarshal(notifier.notification.OutputJSON, &decoded); err != nil {
		t.Fatalf("webhook output is not plain JSON: %v", err)
	}
	if decoded["SwordMesh"] != "123" {
		t.Fatalf("webhook output JSON = %#v", decoded)
	}
	record, found, err := store.Latest(context.Background(), "0123456789abcdef")
	if err != nil || !found || record.PackName != "public.zip" || string(record.OutputJSON) != string(notifier.notification.OutputJSON) {
		t.Fatalf("stored port = %#v, found=%v, err=%v", record, found, err)
	}
}

func TestCredentialWebHandlerRejectsForgedBatchHeaders(t *testing.T) {
	const batchToken = "test-batch-token-with-at-least-32-characters"
	handler, err := NewCredentialWebHandlerWithOptions(func(_ context.Context, _, _ string) (uploadProcessor, error) {
		t.Fatal("processor factory ran with forged batch headers")
		return nil, nil
	}, WebHandlerOptions{MaxConcurrentPorts: 1, BatchToken: batchToken})
	if err != nil {
		t.Fatal(err)
	}
	request := credentialConvertRequest(t, "request-key")
	request.Header.Set(batchIndexHeader, "1")
	request.Header.Set(batchTotalHeader, "10")
	request.Header.Set(batchTokenHeader, "wrong-batch-token-with-at-least-32-chars")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("forged batch response = %d %q, want 403", response.Code, response.Body.String())
	}
}

func TestCredentialWebHandlerRejectsMissingCredentials(t *testing.T) {
	handler, err := NewCredentialWebHandler(func(_ context.Context, _, _ string) (uploadProcessor, error) {
		t.Fatal("processor factory ran without credentials")
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/convert", strings.NewReader("unused"))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("missing-credential status = %d, want %d", response.Code, http.StatusBadRequest)
	}
}

func TestCredentialWebHandlerLimitsJobsPerAPIKey(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	handler, err := NewCredentialWebHandler(func(_ context.Context, _, _ string) (uploadProcessor, error) {
		return &gatedWebProcessor{started: started, release: release}, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	firstResponse := httptest.NewRecorder()
	firstRequest := credentialConvertRequest(t, "same-key")
	firstDone := make(chan struct{})
	go func() {
		handler.ServeHTTP(firstResponse, firstRequest)
		close(firstDone)
	}()
	waitForJobStart(t, started)

	secondResponse := httptest.NewRecorder()
	handler.ServeHTTP(secondResponse, credentialConvertRequest(t, "same-key"))
	if secondResponse.Code != http.StatusTooManyRequests || !strings.Contains(secondResponse.Body.String(), "with this Roblox API key") {
		t.Fatalf("same-key concurrent response = %d %q", secondResponse.Code, secondResponse.Body.String())
	}
	close(release)
	<-firstDone
	if firstResponse.Code != http.StatusOK {
		t.Fatalf("first conversion response = %d %q", firstResponse.Code, firstResponse.Body.String())
	}
}

func TestCredentialWebHandlerAllowsDifferentAPIKeysConcurrently(t *testing.T) {
	started := map[string]chan struct{}{
		"key-one": make(chan struct{}, 1),
		"key-two": make(chan struct{}, 1),
	}
	release := make(chan struct{})
	handler, err := NewCredentialWebHandler(func(_ context.Context, apiKey, _ string) (uploadProcessor, error) {
		return &gatedWebProcessor{started: started[apiKey], release: release}, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	type responseResult struct {
		key      string
		response *httptest.ResponseRecorder
	}
	results := make(chan responseResult, 2)
	for _, key := range []string{"key-one", "key-two"} {
		key := key
		request := credentialConvertRequest(t, key)
		go func() {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			results <- responseResult{key: key, response: response}
		}()
	}
	waitForJobStart(t, started["key-one"])
	waitForJobStart(t, started["key-two"])
	close(release)
	for range 2 {
		result := <-results
		if result.response.Code != http.StatusOK {
			t.Fatalf("%s conversion response = %d %q", result.key, result.response.Code, result.response.Body.String())
		}
	}
}

func TestCredentialWebHandlerEnforcesGlobalPortLimit(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	handler, err := NewCredentialWebHandlerWithOptions(func(_ context.Context, _, _ string) (uploadProcessor, error) {
		return &gatedWebProcessor{started: started, release: release}, nil
	}, WebHandlerOptions{MaxConcurrentPorts: 1})
	if err != nil {
		t.Fatal(err)
	}

	firstResponse := httptest.NewRecorder()
	firstDone := make(chan struct{})
	go func() {
		handler.ServeHTTP(firstResponse, credentialConvertRequest(t, "key-one"))
		close(firstDone)
	}()
	waitForJobStart(t, started)

	secondResponse := httptest.NewRecorder()
	handler.ServeHTTP(secondResponse, credentialConvertRequest(t, "key-two"))
	if secondResponse.Code != http.StatusTooManyRequests || !strings.Contains(secondResponse.Body.String(), "safe processing limit") {
		t.Fatalf("global-limit response = %d %q", secondResponse.Code, secondResponse.Body.String())
	}
	close(release)
	<-firstDone
}

func TestCredentialWebHandlerRejectsInvalidGlobalPortLimit(t *testing.T) {
	_, err := NewCredentialWebHandlerWithOptions(func(_ context.Context, _, _ string) (uploadProcessor, error) {
		return &stubWebProcessor{}, nil
	}, WebHandlerOptions{})
	if err == nil || !strings.Contains(err.Error(), "maximum concurrent ports") {
		t.Fatalf("invalid limit error = %v", err)
	}
}

func credentialConvertRequest(t *testing.T, apiKey string) *http.Request {
	t.Helper()
	body := new(bytes.Buffer)
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("pack", "pack.zip")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(part, "zip"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/convert", body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set(robloxAPIKeyHeader, apiKey)
	request.Header.Set(robloxUserIDHeader, "123456")
	return request
}

func waitForJobStart(t *testing.T, started <-chan struct{}) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("conversion did not start concurrently")
	}
}

func TestWebHandlerLibraryListsAndServesStoredPacks(t *testing.T) {
	processor := &stubWebProcessor{}
	store, err := packstore.Open(filepath.Join(t.TempDir(), "cone.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	handler, err := NewCredentialWebHandlerWithServices(func(context.Context, string, string) (uploadProcessor, error) {
		return processor, nil
	}, nil, store)
	if err != nil {
		t.Fatal(err)
	}

	body := new(bytes.Buffer)
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("pack", "library-test.zip")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(part, "zip"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/convert", body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set(robloxAPIKeyHeader, "key")
	request.Header.Set(robloxUserIDHeader, "1")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("convert status = %d: %s", response.Code, response.Body.String())
	}

	listResponse := httptest.NewRecorder()
	handler.ServeHTTP(listResponse, httptest.NewRequest(http.MethodGet, "/api/library", nil))
	if listResponse.Code != http.StatusOK {
		t.Fatalf("GET /api/library status = %d: %s", listResponse.Code, listResponse.Body.String())
	}
	var listed struct {
		Packs []struct {
			Sequence   uint64 `json:"sequence"`
			PackID     string `json:"packId"`
			PackName   string `json:"packName"`
			SizeBytes  int    `json:"sizeBytes"`
			HasPreview bool   `json:"hasPreview"`
		} `json:"packs"`
	}
	if err := json.Unmarshal(listResponse.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Packs) != 1 {
		t.Fatalf("expected 1 library entry, got %d: %s", len(listed.Packs), listResponse.Body.String())
	}
	entry := listed.Packs[0]
	if entry.PackName != "library-test.zip" || entry.PackID != "0123456789abcdef" || entry.SizeBytes == 0 || !entry.HasPreview {
		t.Fatalf("unexpected library entry: %#v", entry)
	}

	codeResponse := httptest.NewRecorder()
	handler.ServeHTTP(codeResponse, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/library/%d/code", entry.Sequence), nil))
	if codeResponse.Code != http.StatusOK || codeResponse.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("GET .../code status=%d content-type=%q body=%s", codeResponse.Code, codeResponse.Header().Get("Content-Type"), codeResponse.Body.String())
	}
	var codeValues map[string]any
	if err := json.Unmarshal(codeResponse.Body.Bytes(), &codeValues); err != nil {
		t.Fatal(err)
	}
	if codeValues["SwordMesh"] != "123" {
		t.Fatalf("copied code = %#v", codeValues)
	}

	previewResponse := httptest.NewRecorder()
	handler.ServeHTTP(previewResponse, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/library/%d/preview.png", entry.Sequence), nil))
	if previewResponse.Code != http.StatusOK || previewResponse.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("GET .../preview.png status=%d content-type=%q", previewResponse.Code, previewResponse.Header().Get("Content-Type"))
	}
	if previewResponse.Body.String() != "preview" {
		t.Fatalf("preview bytes = %q, want %q", previewResponse.Body.String(), "preview")
	}

	notFound := httptest.NewRecorder()
	handler.ServeHTTP(notFound, httptest.NewRequest(http.MethodGet, "/api/library/999/code", nil))
	if notFound.Code != http.StatusNotFound {
		t.Fatalf("GET missing sequence status = %d", notFound.Code)
	}
}

func TestWebHandlerLibraryWithoutStoreReturnsEmptyList(t *testing.T) {
	handler, err := NewWebHandler(&stubWebProcessor{})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/library", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	var listed struct {
		Packs []any `json:"packs"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Packs) != 0 {
		t.Fatalf("expected empty library without a store, got %d entries", len(listed.Packs))
	}
}

func TestDownloadFilenameIsSafeAndPortable(t *testing.T) {
	for input, expected := range map[string]string{
		"pack.zip":                   "pack_cone.json",
		`C:\\packs\\orange.zip`:      "orange_cone.json",
		"../../uploaded texture.ZIP": "uploaded texture_cone.json",
	} {
		if actual := downloadFilename(input); actual != expected {
			t.Fatalf("downloadFilename(%q) = %q, want %q", input, actual, expected)
		}
	}
}
