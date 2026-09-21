package tests

import (
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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	. "github.com/qaustria/AutoPack-Go/utils"
	"github.com/robloxapi/rbxfile"
	"github.com/robloxapi/rbxfile/rbxl"
)

const testMaxAssetUploadSize = 20 << 20

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

type testCreateAssetPayload struct {
	AssetType       AssetType `json:"assetType"`
	DisplayName     string    `json:"displayName"`
	Description     string    `json:"description"`
	CreationContext struct {
		Creator      AssetCreator `json:"creator"`
		AssetPrivacy AssetPrivacy `json:"assetPrivacy"`
	} `json:"creationContext"`
}

func TestAssetUploaderStreamsModelAndPollsOperation(t *testing.T) {
	modelPath := filepath.Join(t.TempDir(), "pixel sword.glb")
	modelBytes := []byte("glTF-test-data")
	if err := os.WriteFile(modelPath, modelBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	polls := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("x-api-key") != "secret-key" {
			http.Error(response, "missing key", http.StatusUnauthorized)
			return
		}
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/assets/v1/assets":
			if err := request.ParseMultipartForm(testMaxAssetUploadSize); err != nil {
				t.Errorf("parse multipart form: %v", err)
				http.Error(response, "bad multipart", http.StatusBadRequest)
				return
			}
			var payload testCreateAssetPayload
			if err := json.Unmarshal([]byte(request.FormValue("request")), &payload); err != nil {
				t.Errorf("decode request field: %v", err)
			}
			if payload.AssetType != AssetTypeModel || payload.DisplayName != "pixel sword" ||
				payload.Description != "AutoPack upload" || payload.CreationContext.Creator.UserID != "12345" ||
				payload.CreationContext.AssetPrivacy != AssetPrivacyOpenUse {
				t.Errorf("unexpected create payload: %+v", payload)
			}
			file, header, err := request.FormFile("fileContent")
			if err != nil {
				t.Errorf("read fileContent: %v", err)
				http.Error(response, "missing file", http.StatusBadRequest)
				return
			}
			defer file.Close()
			contents, err := io.ReadAll(file)
			if err != nil {
				t.Errorf("read uploaded file: %v", err)
			}
			if string(contents) != string(modelBytes) {
				t.Errorf("uploaded bytes = %q", contents)
			}
			if got := header.Header.Get("Content-Type"); got != "model/gltf-binary" {
				t.Errorf("file content type = %q", got)
			}
			response.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(response, `{"path":"operations/model-op"}`)

		case request.Method == http.MethodGet && request.URL.Path == "/assets/v1/operations/model-op":
			mu.Lock()
			polls++
			currentPoll := polls
			mu.Unlock()
			response.Header().Set("Content-Type", "application/json")
			if currentPoll == 1 {
				_, _ = io.WriteString(response, `{"path":"operations/model-op","done":false}`)
				return
			}
			_, _ = io.WriteString(response, `{"path":"operations/model-op","done":true,"response":{"path":"assets/9988","assetId":"9988","displayName":"pixel sword","assetType":"ASSET_TYPE_MODEL"}}`)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	uploader, err := NewAssetUploader(UploaderConfig{
		APIKey: "secret-key", Creator: AssetCreator{UserID: "12345"},
		Description: "AutoPack upload", AssetPrivacy: AssetPrivacyOpenUse, BaseURL: server.URL,
		HTTPClient: server.Client(), PollInterval: time.Millisecond, PollTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	asset, err := uploader.UploadModel(context.Background(), modelPath, "")
	if err != nil {
		t.Fatal(err)
	}
	if asset.AssetID != "9988" || asset.OperationPath != "operations/model-op" {
		t.Fatalf("unexpected uploaded asset: %+v", asset)
	}
	if polls != 2 {
		t.Fatalf("operation polls = %d, want 2", polls)
	}
}

func TestAssetUploaderResolvesImportedModelToContainedMeshID(t *testing.T) {
	modelPath := filepath.Join(t.TempDir(), "sword.glb")
	if err := os.WriteFile(modelPath, []byte("glTF-test-data"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := rbxfile.NewRoot()
	meshPart := rbxfile.NewInstance("MeshPart")
	meshPart.Properties["MeshId"] = rbxfile.ValueContent("rbxassetid://556677")
	root.Instances = append(root.Instances, meshPart)
	var importedModel bytes.Buffer
	if _, err := (rbxl.Encoder{Mode: rbxl.Model}).Encode(&importedModel, root); err != nil {
		t.Fatal(err)
	}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/assets/v1/assets":
			response.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(response, `{"path":"operations/import-op"}`)
		case request.Method == http.MethodGet && request.URL.Path == "/assets/v1/operations/import-op":
			response.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(response, `{"path":"operations/import-op","done":true,"response":{"assetId":"9988","assetType":"Model"}}`)
		case request.Method == http.MethodGet && request.URL.Path == "/asset-delivery-api/v1/assetId/9988":
			if request.Header.Get("x-api-key") != "key" {
				t.Errorf("asset delivery request did not include the API key")
			}
			response.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(response, `{"location":%q}`, server.URL+"/cdn/imported-model.rbxm")
		case request.Method == http.MethodGet && request.URL.Path == "/cdn/imported-model.rbxm":
			if request.Header.Get("x-api-key") != "" {
				t.Errorf("temporary CDN request leaked the Roblox API key")
			}
			response.Header().Set("Content-Type", "application/octet-stream")
			_, _ = response.Write(importedModel.Bytes())
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	uploader, err := NewAssetUploader(UploaderConfig{
		APIKey: "key", Creator: AssetCreator{UserID: "1"}, BaseURL: server.URL,
		HTTPClient: server.Client(), PollInterval: time.Millisecond, PollTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	asset, err := uploader.Upload(context.Background(), UploadRequest{
		FilePath: modelPath, DisplayName: "Cone sword mesh",
		AssetType: AssetTypeModel, ResolveMeshID: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if asset.AssetID != "556677" || asset.AssetType != "Mesh" {
		t.Fatalf("resolved asset = %+v, want contained mesh 556677", asset)
	}
}

func TestAssetUploaderUploadsNativeRawMesh(t *testing.T) {
	meshPath := filepath.Join(t.TempDir(), "pixel-sword.mesh")
	if err := os.WriteFile(meshPath, []byte("version 2.00\nmesh-test-data"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.Method {
		case http.MethodPost:
			if err := request.ParseMultipartForm(testMaxAssetUploadSize); err != nil {
				t.Errorf("parse multipart: %v", err)
				return
			}
			var payload testCreateAssetPayload
			if err := json.Unmarshal([]byte(request.FormValue("request")), &payload); err != nil {
				t.Errorf("decode payload: %v", err)
			}
			if payload.AssetType != AssetTypeMesh {
				t.Errorf("asset type = %q, want Mesh", payload.AssetType)
			}
			file, header, err := request.FormFile("fileContent")
			if err != nil {
				t.Errorf("read fileContent: %v", err)
				return
			}
			_ = file.Close()
			if got := header.Header.Get("Content-Type"); got != "model/x-file-mesh-data" {
				t.Errorf("file content type = %q, want model/x-file-mesh-data", got)
			}
			_, _ = io.WriteString(response, `{"path":"operations/mesh-op"}`)
		case http.MethodGet:
			_, _ = io.WriteString(response, `{"path":"operations/mesh-op","done":true,"response":{"assetId":"mesh-id","assetType":"Mesh"}}`)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	uploader, err := NewAssetUploader(UploaderConfig{
		APIKey: "key", Creator: AssetCreator{UserID: "1"}, BaseURL: server.URL,
		HTTPClient: server.Client(), PollInterval: time.Millisecond, PollTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	asset, err := uploader.UploadMesh(context.Background(), meshPath, "pixel sword")
	if err != nil {
		t.Fatal(err)
	}
	if asset.AssetID != "mesh-id" || asset.AssetType != "Mesh" {
		t.Fatalf("uploaded asset = %+v", asset)
	}
}

func TestAssetUploaderRejectsMismatchedReturnedAssetType(t *testing.T) {
	meshPath := filepath.Join(t.TempDir(), "mesh.mesh")
	if err := os.WriteFile(meshPath, []byte("mesh"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		if request.Method == http.MethodPost {
			_, _ = io.WriteString(response, `{"path":"operations/wrong-type"}`)
			return
		}
		_, _ = io.WriteString(response, `{"path":"operations/wrong-type","done":true,"response":{"assetId":"bad-id","assetType":"Model"}}`)
	}))
	defer server.Close()
	uploader, err := NewAssetUploader(UploaderConfig{
		APIKey: "key", Creator: AssetCreator{UserID: "1"}, BaseURL: server.URL,
		HTTPClient: server.Client(), PollInterval: time.Millisecond, PollTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = uploader.UploadMesh(context.Background(), meshPath, "mesh")
	if err == nil || !strings.Contains(err.Error(), "returned asset type") {
		t.Fatalf("upload error = %v, want asset-type mismatch", err)
	}
}

func TestAssetUploaderValidatesAndGrantsOpenUse(t *testing.T) {
	var gotGrant assetPermissionRequestForTest
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api-keys/v1/introspect":
			if request.Method != http.MethodPost {
				t.Errorf("introspection method = %s", request.Method)
			}
			var body map[string]string
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Errorf("decode introspection: %v", err)
			}
			if body["apiKey"] != "key" {
				t.Errorf("introspection API key was not sent in body")
			}
			_, _ = io.WriteString(response, `{"enabled":true,"expired":false,"scopes":[{"name":"asset","operations":["read","write"]},{"name":"asset-permissions","operations":["write"]},{"name":"legacy-asset","operations":["manage"]}]}`)
		case "/asset-permissions-api/v1/assets/permissions":
			if request.Method != http.MethodPatch {
				t.Errorf("grant method = %s", request.Method)
			}
			if request.Header.Get("x-api-key") != "key" {
				t.Errorf("grant is missing API key")
			}
			if err := json.NewDecoder(request.Body).Decode(&gotGrant); err != nil {
				t.Errorf("decode grant: %v", err)
			}
			_, _ = io.WriteString(response, `{"successAssetIds":[123,456],"errors":[]}`)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	uploader, err := NewAssetUploader(UploaderConfig{
		APIKey: "key", Creator: AssetCreator{UserID: "1"}, BaseURL: server.URL,
		HTTPClient: server.Client(), AssetPrivacy: AssetPrivacyOpenUse,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := uploader.ValidateOpenUsePermission(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := uploader.GrantOpenUse(context.Background(), []string{"123", "456", "123"}); err != nil {
		t.Fatal(err)
	}
	if gotGrant.SubjectType != "All" || gotGrant.SubjectID != nil || gotGrant.Action != "Use" || len(gotGrant.Requests) != 2 {
		t.Fatalf("Open Use payload = %+v", gotGrant)
	}
	for _, grant := range gotGrant.Requests {
		if grant.GrantToDependencies {
			t.Fatalf("Open Use grant included unsupported dependency propagation: %+v", grant)
		}
	}
}

func TestAssetUploaderRetriesTransientIntrospectionTransportFailures(t *testing.T) {
	attempts := 0
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		attempts++
		if attempts < 3 {
			return nil, errors.New("simulated TLS handshake timeout")
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(
				`{"enabled":true,"expired":false,"scopes":[{"name":"asset","operations":["read","write"]},{"name":"asset-permissions","operations":["write"]},{"name":"legacy-asset","operations":["manage"]}]}`,
			)),
			Request: request,
		}, nil
	})}
	uploader, err := NewAssetUploader(UploaderConfig{
		APIKey: "key", Creator: AssetCreator{UserID: "1"}, HTTPClient: client,
		RetryBaseDelay: time.Millisecond, RetryMaxDelay: 2 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := uploader.ValidateOpenUsePermission(context.Background()); err != nil {
		t.Fatal(err)
	}
	if attempts != 3 {
		t.Fatalf("introspection attempts = %d, want 3", attempts)
	}
}

func TestAssetUploaderDoesNotRetryRejectedAPIKey(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		attempts++
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(response, `{"code":16,"message":"Invalid API Key secret."}`)
	}))
	defer server.Close()
	uploader, err := NewAssetUploader(UploaderConfig{
		APIKey: "bad-key", Creator: AssetCreator{UserID: "1"}, BaseURL: server.URL,
		HTTPClient: server.Client(), RetryBaseDelay: time.Millisecond, RetryMaxDelay: 2 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	err = uploader.ValidateOpenUsePermission(context.Background())
	if err == nil || !strings.Contains(err.Error(), "401 Unauthorized") {
		t.Fatalf("validation error = %v, want 401", err)
	}
	if attempts != 1 {
		t.Fatalf("introspection attempts = %d, want 1", attempts)
	}
}

func TestAssetUploaderBatchesOpenUseAtFiftyAssets(t *testing.T) {
	var mu sync.Mutex
	var batchSizes []int
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		var grant assetPermissionRequestForTest
		if err := json.NewDecoder(request.Body).Decode(&grant); err != nil {
			t.Errorf("decode grant: %v", err)
			return
		}
		ids := make([]int64, len(grant.Requests))
		for index, item := range grant.Requests {
			ids[index] = item.AssetID
		}
		mu.Lock()
		batchSizes = append(batchSizes, len(ids))
		mu.Unlock()
		_ = json.NewEncoder(response).Encode(map[string]any{"successAssetIds": ids, "errors": []any{}})
	}))
	defer server.Close()
	uploader, err := NewAssetUploader(UploaderConfig{
		APIKey: "key", Creator: AssetCreator{UserID: "1"}, BaseURL: server.URL,
		HTTPClient: server.Client(), AssetPrivacy: AssetPrivacyOpenUse,
	})
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 69)
	for index := range ids {
		ids[index] = strconv.Itoa(index + 1)
	}
	if err := uploader.GrantOpenUse(context.Background(), ids); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(batchSizes) != "[50 19]" {
		t.Fatalf("Open Use batch sizes = %v, want [50 19]", batchSizes)
	}
}

func TestAssetUploaderRejectsMissingOpenUseScope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(response, `{"enabled":true,"expired":false,"scopes":[{"name":"asset","operations":["read","write"]}]}`)
	}))
	defer server.Close()
	uploader, err := NewAssetUploader(UploaderConfig{
		APIKey: "key", Creator: AssetCreator{UserID: "1"}, BaseURL: server.URL,
		HTTPClient: server.Client(), AssetPrivacy: AssetPrivacyOpenUse,
	})
	if err != nil {
		t.Fatal(err)
	}
	err = uploader.ValidateOpenUsePermission(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Asset Permissions → Write") || !strings.Contains(err.Error(), "Legacy Assets → Manage") {
		t.Fatalf("scope error = %v", err)
	}
}

type assetPermissionRequestForTest struct {
	SubjectType string  `json:"subjectType"`
	SubjectID   *string `json:"subjectId"`
	Action      string  `json:"action"`
	Requests    []struct {
		AssetID             int64 `json:"assetId"`
		GrantToDependencies bool  `json:"grantToDependencies"`
	} `json:"requests"`
}

func TestAssetUploaderUploadManyPreservesOrderErrorsAndStreamsProgress(t *testing.T) {
	directory := t.TempDir()
	modelPath := filepath.Join(directory, "mesh.glb")
	texturePath := filepath.Join(directory, "texture.png")
	if err := os.WriteFile(modelPath, []byte("model"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(texturePath, []byte("png"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		if request.Method == http.MethodPost {
			if err := request.ParseMultipartForm(testMaxAssetUploadSize); err != nil {
				t.Errorf("parse multipart: %v", err)
				return
			}
			var payload testCreateAssetPayload
			if err := json.Unmarshal([]byte(request.FormValue("request")), &payload); err != nil {
				t.Errorf("decode payload: %v", err)
				return
			}
			_, _ = fmt.Fprintf(response, `{"path":"operations/%s"}`, payload.DisplayName)
			return
		}
		name := strings.TrimPrefix(request.URL.Path, "/assets/v1/operations/")
		if name == "broken" {
			_, _ = io.WriteString(response, `{"path":"operations/broken","done":true,"error":{"code":3,"message":"rejected"}}`)
			return
		}
		_, _ = fmt.Fprintf(response, `{"path":"operations/%s","done":true,"response":{"assetId":"id-%s"}}`, name, name)
	}))
	defer server.Close()
	uploader, err := NewAssetUploader(UploaderConfig{
		APIKey: "key", Creator: AssetCreator{GroupID: "777"}, BaseURL: server.URL,
		HTTPClient: server.Client(), PollInterval: time.Millisecond, PollTimeout: time.Second,
		MaxConcurrent: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	jobs := []UploadRequest{
		{FilePath: modelPath, DisplayName: "mesh", AssetType: AssetTypeModel},
		{FilePath: texturePath, DisplayName: "texture", AssetType: AssetTypeDecal},
		{FilePath: modelPath, DisplayName: "broken", AssetType: AssetTypeModel},
	}
	var progressMu sync.Mutex
	var progressIndexes []int
	results := uploader.UploadManyWithProgress(context.Background(), jobs, func(index int, result UploadResult) {
		progressMu.Lock()
		defer progressMu.Unlock()
		progressIndexes = append(progressIndexes, index)
		if result.Request.DisplayName != jobs[index].DisplayName {
			t.Errorf("progress result %d has request %q", index, result.Request.DisplayName)
		}
	})
	if len(results) != 3 {
		t.Fatalf("result count = %d", len(results))
	}
	if results[0].Asset == nil || results[0].Asset.AssetID != "id-mesh" {
		t.Fatalf("first result = %+v", results[0])
	}
	if results[1].Asset == nil || results[1].Asset.AssetID != "id-texture" {
		t.Fatalf("second result = %+v", results[1])
	}
	if results[2].Asset != nil || !strings.Contains(results[2].Error, "rejected") {
		t.Fatalf("third result = %+v", results[2])
	}
	if len(progressIndexes) != len(jobs) {
		t.Fatalf("progress callback count = %d, want %d", len(progressIndexes), len(jobs))
	}
	seen := make(map[int]bool, len(progressIndexes))
	for _, index := range progressIndexes {
		seen[index] = true
	}
	for index := range jobs {
		if !seen[index] {
			t.Fatalf("progress never reported job %d: %v", index, progressIndexes)
		}
	}
}

func TestAssetUploaderValidationAndAPIError(t *testing.T) {
	if _, err := NewAssetUploader(UploaderConfig{Creator: AssetCreator{UserID: "1"}}); err == nil {
		t.Fatal("expected missing API key to fail")
	}
	if _, err := NewAssetUploader(UploaderConfig{APIKey: "key"}); err == nil {
		t.Fatal("expected missing creator to fail")
	}
	if _, err := NewAssetUploader(UploaderConfig{
		APIKey: "key", Creator: AssetCreator{UserID: "1", GroupID: "2"},
	}); err == nil {
		t.Fatal("expected ambiguous creator to fail")
	}
	if _, err := NewAssetUploader(UploaderConfig{
		APIKey: "key", Creator: AssetCreator{UserID: "1"}, AssetPrivacy: "public-ish",
	}); err == nil {
		t.Fatal("expected invalid asset privacy to fail")
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		http.Error(response, "rate limited", http.StatusTooManyRequests)
	}))
	defer server.Close()
	uploader, err := NewAssetUploader(UploaderConfig{
		APIKey: "key", Creator: AssetCreator{UserID: "1"}, BaseURL: server.URL,
		HTTPClient: server.Client(), PollTimeout: time.Second, MaxRetries: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	modelPath := filepath.Join(t.TempDir(), "model.glb")
	if err := os.WriteFile(modelPath, []byte("model"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = uploader.UploadModel(context.Background(), modelPath, "model")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("upload error = %v, want APIError 429", err)
	}
}

func TestAssetUploaderRetriesRateLimits(t *testing.T) {
	modelPath := filepath.Join(t.TempDir(), "model.glb")
	if err := os.WriteFile(modelPath, []byte("model"), 0o600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.Method {
		case http.MethodPost:
			mu.Lock()
			posts++
			attempt := posts
			mu.Unlock()
			if attempt == 1 {
				response.Header().Set("Retry-After", "0.001")
				http.Error(response, "limited", http.StatusTooManyRequests)
				return
			}
			_, _ = io.WriteString(response, `{"path":"operations/retried"}`)
		case http.MethodGet:
			_, _ = io.WriteString(response, `{"path":"operations/retried","done":true,"response":{"assetId":"retry-id"}}`)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	uploader, err := NewAssetUploader(UploaderConfig{
		APIKey: "key", Creator: AssetCreator{UserID: "1"}, BaseURL: server.URL,
		HTTPClient: server.Client(), PollInterval: time.Millisecond, PollTimeout: time.Second,
		MaxRetries: 2, RetryBaseDelay: time.Millisecond, RetryMaxDelay: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	asset, err := uploader.UploadModel(context.Background(), modelPath, "model")
	if err != nil {
		t.Fatal(err)
	}
	if asset.AssetID != "retry-id" || posts != 2 {
		t.Fatalf("asset = %+v, POST attempts = %d", asset, posts)
	}
}

func TestAssetUploaderLimitsConcurrentModels(t *testing.T) {
	modelPath := filepath.Join(t.TempDir(), "model.glb")
	if err := os.WriteFile(modelPath, []byte("model"), 0o600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	active := 0
	maximum := 0
	activePolls := 0
	maximumPolls := 0
	sequence := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		if request.Method == http.MethodPost {
			mu.Lock()
			active++
			if active > maximum {
				maximum = active
			}
			sequence++
			operation := sequence
			mu.Unlock()
			time.Sleep(10 * time.Millisecond)
			mu.Lock()
			active--
			mu.Unlock()
			_, _ = fmt.Fprintf(response, `{"path":"operations/%d"}`, operation)
			return
		}
		operation := strings.TrimPrefix(request.URL.Path, "/assets/v1/operations/")
		mu.Lock()
		activePolls++
		if activePolls > maximumPolls {
			maximumPolls = activePolls
		}
		mu.Unlock()
		time.Sleep(25 * time.Millisecond)
		mu.Lock()
		activePolls--
		mu.Unlock()
		_, _ = fmt.Fprintf(response, `{"path":"operations/%s","done":true,"response":{"assetId":"%s"}}`, operation, operation)
	}))
	defer server.Close()
	uploader, err := NewAssetUploader(UploaderConfig{
		APIKey: "key", Creator: AssetCreator{UserID: "1"}, BaseURL: server.URL,
		HTTPClient: server.Client(), PollInterval: time.Millisecond, PollTimeout: time.Second,
		MaxConcurrent: 8, MaxConcurrentModels: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	requests := make([]UploadRequest, 8)
	for index := range requests {
		requests[index] = UploadRequest{FilePath: modelPath, DisplayName: fmt.Sprintf("model-%d", index), AssetType: AssetTypeModel}
	}
	results := uploader.UploadMany(context.Background(), requests)
	for index, result := range results {
		if result.Error != "" || result.Asset == nil {
			t.Fatalf("result %d = %+v", index, result)
		}
	}
	if maximum != 2 {
		t.Fatalf("maximum concurrent model uploads = %d, want 2", maximum)
	}
	if maximumPolls <= 2 {
		t.Fatalf("maximum concurrent model operation polls = %d, want more than 2", maximumPolls)
	}
}
