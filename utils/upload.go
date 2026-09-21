package utils

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/robloxapi/rbxfile"
	"github.com/robloxapi/rbxfile/rbxl"
)

const (
	robloxAssetsBaseURL              = "https://apis.roblox.com"
	maxAssetUploadSize               = 20 << 20 // Roblox's documented 20 MB content limit.
	defaultRobloxRequestTimeout      = 2 * time.Minute
	defaultRobloxTLSHandshakeTimeout = 30 * time.Second
	maxIntrospectionRetries          = 3
)

// AssetType is a Roblox Open Cloud asset type supported by this pipeline.
type AssetType string

const (
	AssetTypeMesh  AssetType = "Mesh"
	AssetTypeModel AssetType = "Model"
	AssetTypeImage AssetType = "Image"
	AssetTypeDecal AssetType = "Decal"
)

// AssetPrivacy controls who can load supported Roblox assets. Open Use is
// required when the generated JSON is consumed by a game owned by a different
// creator. Roblox does not allow Open Use to be reverted after creation.
type AssetPrivacy string

const (
	AssetPrivacyDefault    AssetPrivacy = "default"
	AssetPrivacyRestricted AssetPrivacy = "restricted"
	AssetPrivacyOpenUse    AssetPrivacy = "openUse"
)

// AssetCreator identifies the user or group that will own uploaded assets.
// Exactly one field must be set. IDs stay strings to avoid numeric precision
// loss when upload results are later encoded to JSON or consumed by JavaScript.
type AssetCreator struct {
	UserID  string `json:"userId,omitempty"`
	GroupID string `json:"groupId,omitempty"`
}

// UploaderConfig configures a reusable Roblox Open Cloud uploader.
type UploaderConfig struct {
	APIKey       string
	Creator      AssetCreator
	Description  string
	AssetPrivacy AssetPrivacy

	// HTTPClient, BaseURL, and timing fields are optional. BaseURL mainly exists
	// for local tests; production callers should leave it empty.
	HTTPClient    *http.Client
	BaseURL       string
	PollInterval  time.Duration
	PollTimeout   time.Duration
	MaxConcurrent int

	// Geometry imports are substantially heavier than image uploads and Roblox
	// accepts fewer simultaneous create requests. This only limits the brief
	// POST phase; accepted imports continue processing and polling concurrently.
	// Zero selects two, which matches the observed Roblox create capacity.
	MaxConcurrentModels int

	// A zero MaxRetries selects the default. Set it to -1 to disable retries.
	// Upload retries handle HTTP 429 responses. API-key validation also uses up
	// to three retries for transient transport and upstream server failures.
	MaxRetries     int
	RetryBaseDelay time.Duration
	RetryMaxDelay  time.Duration
}

// UploadRequest describes one local file to upload. Description falls back to
// UploaderConfig.Description when empty. DisplayName falls back to the filename
// without its extension when empty.
type UploadRequest struct {
	FilePath      string    `json:"filePath"`
	DisplayName   string    `json:"displayName"`
	Description   string    `json:"description,omitempty"`
	AssetType     AssetType `json:"assetType"`
	ResolveMeshID bool      `json:"resolveMeshId,omitempty"`
}

// ModerationResult is the moderation state returned by Roblox.
type ModerationResult struct {
	State string `json:"moderationState"`
}

// UploadedAsset contains the useful fields returned when an upload operation
// finishes. OperationPath records the asynchronous operation used to create it.
type UploadedAsset struct {
	Path               string           `json:"path"`
	AssetID            string           `json:"assetId"`
	RevisionID         string           `json:"revisionId,omitempty"`
	RevisionCreateTime string           `json:"revisionCreateTime,omitempty"`
	DisplayName        string           `json:"displayName,omitempty"`
	Description        string           `json:"description,omitempty"`
	AssetType          string           `json:"assetType,omitempty"`
	ModerationResult   ModerationResult `json:"moderationResult,omitempty"`
	OperationPath      string           `json:"operationPath"`
}

// UploadResult is JSON-friendly and retains input ordering in UploadMany.
type UploadResult struct {
	Request UploadRequest  `json:"request"`
	Asset   *UploadedAsset `json:"asset,omitempty"`
	Error   string         `json:"error,omitempty"`
}

// AssetUploader uploads meshes, GLB models, and PNG textures through Roblox Open Cloud.
// It is safe for concurrent use.
type AssetUploader struct {
	apiKey        string
	creator       AssetCreator
	description   string
	assetPrivacy  AssetPrivacy
	client        *http.Client
	baseURL       string
	pollInterval  time.Duration
	pollTimeout   time.Duration
	maxConcurrent int
	createSlots   chan struct{}
	modelSlots    chan struct{}
	maxRetries    int
	retryBase     time.Duration
	retryMax      time.Duration

	modelRate rateWindow
	decalRate rateWindow
}

type rateWindow struct {
	mu             sync.Mutex
	blockedUntil   time.Time
	nextCreate     time.Time
	requestSpacing time.Duration
}

// APIError describes a non-success response from Roblox without exposing the
// API key or request authorization headers.
type APIError struct {
	StatusCode int
	Status     string
	Body       string
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("Roblox Assets API returned %s", e.Status)
	}
	return fmt.Sprintf("Roblox Assets API returned %s: %s", e.Status, e.Body)
}

type createAssetPayload struct {
	AssetType       AssetType       `json:"assetType"`
	DisplayName     string          `json:"displayName"`
	Description     string          `json:"description"`
	CreationContext creationContext `json:"creationContext"`
}

type creationContext struct {
	Creator      AssetCreator `json:"creator"`
	AssetPrivacy AssetPrivacy `json:"assetPrivacy,omitempty"`
}

type assetOperation struct {
	Path     string          `json:"path"`
	Done     bool            `json:"done"`
	Response *UploadedAsset  `json:"response,omitempty"`
	Error    *operationError `json:"error,omitempty"`
}

type operationError struct {
	Code    int               `json:"code"`
	Message string            `json:"message"`
	Details []json.RawMessage `json:"details,omitempty"`
}

type apiKeyIntrospection struct {
	Enabled bool `json:"enabled"`
	Expired bool `json:"expired"`
	Scopes  []struct {
		Name       string   `json:"name"`
		Operations []string `json:"operations"`
	} `json:"scopes"`
}

type assetPermissionRequest struct {
	SubjectType string                 `json:"subjectType"`
	SubjectID   *string                `json:"subjectId"`
	Action      string                 `json:"action"`
	Requests    []assetPermissionGrant `json:"requests"`
}

type assetPermissionGrant struct {
	AssetID             int64 `json:"assetId"`
	GrantToDependencies bool  `json:"grantToDependencies"`
}

type assetPermissionResponse struct {
	SuccessAssetIDs []int64 `json:"successAssetIds"`
	Errors          []struct {
		AssetID int64  `json:"assetId"`
		Code    string `json:"code"`
	} `json:"errors"`
}

var uploadCopyBufferPool = sync.Pool{
	New: func() any { return make([]byte, 128<<10) },
}

// NewAssetUploader validates config and creates a reusable uploader.
func NewAssetUploader(config UploaderConfig) (*AssetUploader, error) {
	if strings.TrimSpace(config.APIKey) == "" {
		return nil, errors.New("Roblox API key is required")
	}
	if err := validateCreator(config.Creator); err != nil {
		return nil, err
	}
	assetPrivacy := config.AssetPrivacy
	if assetPrivacy == "" {
		assetPrivacy = AssetPrivacyDefault
	}
	switch assetPrivacy {
	case AssetPrivacyDefault, AssetPrivacyRestricted, AssetPrivacyOpenUse:
	default:
		return nil, fmt.Errorf("invalid Roblox asset privacy %q", assetPrivacy)
	}
	baseURL := strings.TrimRight(config.BaseURL, "/")
	if baseURL == "" {
		baseURL = robloxAssetsBaseURL
	}
	parsedBaseURL, err := url.Parse(baseURL)
	if err != nil || (parsedBaseURL.Scheme != "http" && parsedBaseURL.Scheme != "https") || parsedBaseURL.Host == "" {
		return nil, fmt.Errorf("invalid Roblox Assets API base URL %q", baseURL)
	}
	if parsedBaseURL.RawQuery != "" || parsedBaseURL.Fragment != "" {
		return nil, errors.New("Roblox Assets API base URL must not contain a query or fragment")
	}
	client := config.HTTPClient
	if client == nil {
		transport := &http.Transport{
			Proxy:             http.ProxyFromEnvironment,
			ForceAttemptHTTP2: true,
		}
		if defaultTransport, ok := http.DefaultTransport.(*http.Transport); ok {
			transport = defaultTransport.Clone()
		}
		transport.TLSHandshakeTimeout = defaultRobloxTLSHandshakeTimeout
		client = &http.Client{
			Transport: transport,
			Timeout:   defaultRobloxRequestTimeout,
		}
	}
	pollInterval := config.PollInterval
	if pollInterval == 0 {
		pollInterval = 200 * time.Millisecond
	}
	if pollInterval < 0 {
		return nil, errors.New("upload poll interval must not be negative")
	}
	pollTimeout := config.PollTimeout
	if pollTimeout == 0 {
		pollTimeout = 5 * time.Minute
	}
	if pollTimeout < 0 {
		return nil, errors.New("upload poll timeout must not be negative")
	}
	maxConcurrent := config.MaxConcurrent
	if maxConcurrent == 0 {
		maxConcurrent = runtime.GOMAXPROCS(0)
		if maxConcurrent < 8 {
			maxConcurrent = 8
		}
		if maxConcurrent > 16 {
			maxConcurrent = 16
		}
	}
	if maxConcurrent < 0 {
		return nil, errors.New("maximum concurrent uploads must not be negative")
	}
	maxConcurrentModels := config.MaxConcurrentModels
	if maxConcurrentModels == 0 {
		maxConcurrentModels = 2
	}
	if maxConcurrentModels < 0 {
		return nil, errors.New("maximum concurrent model uploads must not be negative")
	}
	if maxConcurrentModels > maxConcurrent {
		maxConcurrentModels = maxConcurrent
	}
	maxRetries := config.MaxRetries
	if maxRetries == 0 {
		maxRetries = 8
	}
	if maxRetries < -1 {
		return nil, errors.New("maximum upload retries must be -1 or greater")
	}
	if maxRetries == -1 {
		maxRetries = 0
	}
	retryBase := config.RetryBaseDelay
	if retryBase == 0 {
		retryBase = 250 * time.Millisecond
	}
	if retryBase < 0 {
		return nil, errors.New("upload retry base delay must not be negative")
	}
	retryMax := config.RetryMaxDelay
	if retryMax == 0 {
		retryMax = 8 * time.Second
	}
	if retryMax < retryBase {
		return nil, errors.New("upload retry maximum delay must not be less than its base delay")
	}
	return &AssetUploader{
		apiKey:        config.APIKey,
		creator:       config.Creator,
		description:   config.Description,
		assetPrivacy:  assetPrivacy,
		client:        client,
		baseURL:       baseURL,
		pollInterval:  pollInterval,
		pollTimeout:   pollTimeout,
		maxConcurrent: maxConcurrent,
		createSlots:   make(chan struct{}, maxConcurrent),
		modelSlots:    make(chan struct{}, maxConcurrentModels),
		maxRetries:    maxRetries,
		retryBase:     retryBase,
		retryMax:      retryMax,
	}, nil
}

func validateCreator(creator AssetCreator) error {
	hasUser := strings.TrimSpace(creator.UserID) != ""
	hasGroup := strings.TrimSpace(creator.GroupID) != ""
	if hasUser == hasGroup {
		return errors.New("exactly one Roblox creator user ID or group ID is required")
	}
	return nil
}

// ValidateOpenUsePermission verifies every permission Cone needs before any
// expensive asset preparation begins. Native mesh creation also uses Roblox's
// legacy asset permission in addition to the current Assets API scopes.
func (u *AssetUploader) ValidateOpenUsePermission(ctx context.Context) error {
	if u == nil {
		return errors.New("asset uploader is nil")
	}
	if ctx == nil {
		return errors.New("permission validation context is nil")
	}
	body, err := json.Marshal(map[string]string{"apiKey": u.apiKey})
	if err != nil {
		return fmt.Errorf("encode Roblox API-key introspection: %w", err)
	}
	result, err := u.inspectAPIKey(ctx, body)
	if err != nil {
		return err
	}
	if !result.Enabled || result.Expired {
		return errors.New("Roblox API key is disabled or expired")
	}
	required := []struct {
		name      string
		operation string
		label     string
	}{
		{name: "asset", operation: "read", label: "Assets → Read"},
		{name: "asset", operation: "write", label: "Assets → Write"},
		{name: "asset-permissions", operation: "write", label: "Asset Permissions → Write"},
		{name: "legacy-asset", operation: "manage", label: "Legacy Assets → Manage"},
	}
	var missing []string
	for _, permission := range required {
		if !hasAPIKeyPermission(result, permission.name, permission.operation) {
			missing = append(missing, permission.label)
		}
	}
	if len(missing) != 0 {
		return fmt.Errorf("Roblox API key is missing required permissions: %s", strings.Join(missing, ", "))
	}
	return nil
}

func (u *AssetUploader) inspectAPIKey(ctx context.Context, body []byte) (apiKeyIntrospection, error) {
	retries := u.maxRetries
	if retries > maxIntrospectionRetries {
		retries = maxIntrospectionRetries
	}
	for attempt := 0; ; attempt++ {
		result, retryable, err := u.inspectAPIKeyOnce(ctx, body)
		if err == nil {
			return result, nil
		}
		if ctx.Err() != nil {
			return apiKeyIntrospection{}, fmt.Errorf("inspect Roblox API key: %w", ctx.Err())
		}
		if !retryable || attempt >= retries {
			if retryable && attempt > 0 {
				return apiKeyIntrospection{}, fmt.Errorf("inspect Roblox API key after %d attempts: %w", attempt+1, err)
			}
			return apiKeyIntrospection{}, fmt.Errorf("inspect Roblox API key: %w", err)
		}
		delay := u.retryBackoff(attempt)
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.RetryAfter > delay {
			delay = apiErr.RetryAfter
		}
		if err := sleepContext(ctx, delay); err != nil {
			return apiKeyIntrospection{}, fmt.Errorf("inspect Roblox API key: %w", err)
		}
	}
}

func (u *AssetUploader) inspectAPIKeyOnce(ctx context.Context, body []byte) (apiKeyIntrospection, bool, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, u.baseURL+"/api-keys/v1/introspect", bytes.NewReader(body))
	if err != nil {
		return apiKeyIntrospection{}, false, fmt.Errorf("create Roblox API-key introspection request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := u.client.Do(request)
	if err != nil {
		return apiKeyIntrospection{}, true, err
	}
	var result apiKeyIntrospection
	if err := decodeAPIResponse(response, &result); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			return apiKeyIntrospection{}, retryableIntrospectionStatus(apiErr.StatusCode), err
		}
		// A proxy or upstream edge can occasionally return a truncated/non-JSON
		// success body. Replaying this read-only validation request is safe.
		return apiKeyIntrospection{}, true, err
	}
	return result, false, nil
}

func retryableIntrospectionStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout,
		http.StatusTooEarly,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func hasAPIKeyPermission(result apiKeyIntrospection, wantedName, wantedOperation string) bool {
	for _, scope := range result.Scopes {
		name := strings.ToLower(strings.TrimSpace(scope.Name))
		if name == wantedName+":"+wantedOperation {
			return true
		}
		if name != wantedName && name != wantedName+"s" {
			continue
		}
		for _, operation := range scope.Operations {
			if strings.EqualFold(strings.TrimSpace(operation), wantedOperation) {
				return true
			}
		}
	}
	return false
}

// UploadModel uploads a GLB (or another supported Roblox model file).
func (u *AssetUploader) UploadModel(ctx context.Context, filePath, displayName string) (UploadedAsset, error) {
	return u.Upload(ctx, UploadRequest{
		FilePath: filePath, DisplayName: displayName, AssetType: AssetTypeModel,
	})
}

// UploadMesh uploads a Roblox native mesh stream. Unlike a GLB Model import,
// the returned asset ID is valid for MeshPart.MeshId.
func (u *AssetUploader) UploadMesh(ctx context.Context, filePath, displayName string) (UploadedAsset, error) {
	return u.Upload(ctx, UploadRequest{
		FilePath: filePath, DisplayName: displayName, AssetType: AssetTypeMesh,
	})
}

// UploadTexture uploads a PNG (or another supported image) as a Roblox Image.
// A Decal is a wrapper container and its ID is not interchangeable with the
// Image ID expected by TextureID and image-content fields.
func (u *AssetUploader) UploadTexture(ctx context.Context, filePath, displayName string) (UploadedAsset, error) {
	return u.Upload(ctx, UploadRequest{
		FilePath: filePath, DisplayName: displayName, AssetType: AssetTypeImage,
	})
}

// Upload sends one asset and waits for Roblox's asynchronous operation to
// finish or for the context/poll timeout to expire.
func (u *AssetUploader) Upload(ctx context.Context, request UploadRequest) (UploadedAsset, error) {
	if u == nil {
		return UploadedAsset{}, errors.New("asset uploader is nil")
	}
	if ctx == nil {
		return UploadedAsset{}, errors.New("upload context is nil")
	}
	request, contentType, err := u.validateRequest(request)
	if err != nil {
		return UploadedAsset{}, err
	}
	payload := createAssetPayload{
		AssetType:       request.AssetType,
		DisplayName:     request.DisplayName,
		Description:     request.Description,
		CreationContext: creationContext{Creator: u.creator, AssetPrivacy: u.assetPrivacy},
	}
	for attempt := 0; ; attempt++ {
		operation, err := u.createAssetLimited(ctx, request, contentType, payload)
		if err == nil {
			asset, waitErr := u.waitForOperation(ctx, operation)
			if waitErr != nil {
				return UploadedAsset{}, waitErr
			}
			if !matchesAssetType(request.AssetType, asset.AssetType) {
				return UploadedAsset{}, fmt.Errorf(
					"Roblox returned asset type %q for requested %s upload",
					asset.AssetType, request.AssetType,
				)
			}
			if request.ResolveMeshID {
				meshID, resolveErr := u.resolveImportedModelMeshID(ctx, asset.AssetID)
				if resolveErr != nil {
					return UploadedAsset{}, fmt.Errorf("resolve imported model %s to its MeshPart mesh: %w", asset.AssetID, resolveErr)
				}
				asset.AssetID = meshID
				asset.AssetType = string(AssetTypeMesh)
			}
			return asset, nil
		}
		delay, retry := u.retryDelay(err, attempt)
		if !retry {
			return UploadedAsset{}, err
		}
		u.noteRateLimit(request.AssetType, delay, attempt)
	}
}

// resolveImportedModelMeshID follows the supported custom-mesh upload flow:
// upload GLB/FBX as a Model, download the resulting package, then return the
// MeshPart's internal MeshId. Open Cloud's Mesh upload endpoint explicitly
// accepts only mesh bytes previously downloaded from Asset Delivery; it is not
// a custom geometry importer.
func (u *AssetUploader) resolveImportedModelMeshID(ctx context.Context, modelID string) (string, error) {
	endpoint := u.baseURL + "/asset-delivery-api/v1/assetId/" + url.PathEscape(modelID)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("x-api-key", u.apiKey)
	request.Header.Set("Accept", "application/json")
	response, err := u.client.Do(request)
	if err != nil {
		return "", fmt.Errorf("request imported model download location: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		defer response.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		return "", &APIError{StatusCode: response.StatusCode, Status: response.Status, Body: strings.TrimSpace(string(body))}
	}
	var delivery struct {
		Location string `json:"location"`
	}
	decodeErr := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&delivery)
	closeErr := response.Body.Close()
	if decodeErr != nil {
		return "", fmt.Errorf("decode imported model download location: %w", decodeErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("close imported model location response: %w", closeErr)
	}
	delivery.Location = strings.TrimSpace(delivery.Location)
	if delivery.Location == "" {
		return "", errors.New("Roblox asset delivery response has no download location")
	}
	deliveryURL, err := url.Parse(delivery.Location)
	if err != nil || !deliveryURL.IsAbs() || (deliveryURL.Scheme != "https" && deliveryURL.Scheme != "http") {
		return "", fmt.Errorf("Roblox asset delivery returned an invalid download location %q", delivery.Location)
	}

	download, err := http.NewRequestWithContext(ctx, http.MethodGet, deliveryURL.String(), nil)
	if err != nil {
		return "", fmt.Errorf("create imported model download request: %w", err)
	}
	download.Header.Set("Accept", "application/octet-stream")
	modelResponse, err := u.client.Do(download)
	if err != nil {
		return "", fmt.Errorf("download imported model content: %w", err)
	}
	defer modelResponse.Body.Close()
	if modelResponse.StatusCode < 200 || modelResponse.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(modelResponse.Body, 64<<10))
		return "", fmt.Errorf("download imported model content: %w", &APIError{
			StatusCode: modelResponse.StatusCode,
			Status:     modelResponse.Status,
			Body:       strings.TrimSpace(string(body)),
		})
	}
	root, _, err := rbxl.Decoder{Mode: rbxl.Model}.Decode(io.LimitReader(modelResponse.Body, maxAssetUploadSize))
	if err != nil {
		return "", fmt.Errorf("decode imported Roblox model: %w", err)
	}
	var meshIDs []string
	seenMeshIDs := make(map[string]struct{})
	var visit func([]*rbxfile.Instance)
	visit = func(instances []*rbxfile.Instance) {
		for _, instance := range instances {
			if instance.ClassName == "MeshPart" {
				for _, name := range []string{"MeshContent", "MeshId", "MeshID"} {
					if value, ok := instance.Properties[name]; ok {
						if id := assetIDFromContent(value.String()); id != "" {
							if _, seen := seenMeshIDs[id]; !seen {
								seenMeshIDs[id] = struct{}{}
								meshIDs = append(meshIDs, id)
							}
						}
					}
				}
			}
			visit(instance.Children)
		}
	}
	visit(root.Instances)
	if len(meshIDs) != 1 {
		return "", fmt.Errorf("imported model contains %d usable MeshPart mesh IDs, want exactly 1", len(meshIDs))
	}
	return meshIDs[0], nil
}

func assetIDFromContent(value string) string {
	value = strings.TrimSpace(value)
	for _, prefix := range []string{"rbxassetid://", "https://www.roblox.com/asset/?id=", "http://www.roblox.com/asset/?id="} {
		value = strings.TrimPrefix(value, prefix)
	}
	if parsed, err := url.Parse(value); err == nil && parsed.Query().Get("id") != "" {
		value = parsed.Query().Get("id")
	}
	value = strings.TrimSpace(value)
	if _, err := strconv.ParseUint(value, 10, 64); err != nil {
		return ""
	}
	return value
}

// GrantOpenUse permanently allows every Roblox creator and game to use the
// listed assets. Roblox does not provide a way to revoke Open Use.
func (u *AssetUploader) GrantOpenUse(ctx context.Context, assetIDs []string) error {
	if u == nil {
		return errors.New("asset uploader is nil")
	}
	if ctx == nil {
		return errors.New("permission grant context is nil")
	}
	grants := make([]assetPermissionGrant, 0, len(assetIDs))
	seen := make(map[int64]struct{}, len(assetIDs))
	for _, rawID := range assetIDs {
		assetID, err := strconv.ParseInt(strings.TrimSpace(rawID), 10, 64)
		if err != nil || assetID <= 0 {
			return fmt.Errorf("invalid Roblox asset ID %q for Open Use", rawID)
		}
		if _, exists := seen[assetID]; exists {
			continue
		}
		seen[assetID] = struct{}{}
		grants = append(grants, assetPermissionGrant{AssetID: assetID})
	}
	if len(grants) == 0 {
		return nil
	}
	// Roblox accepts at most 50 assets in one grant. All/Open Use also rejects
	// dependency propagation, so AutoPack uploads Image assets directly.
	const maxPermissionBatch = 50
	for offset := 0; offset < len(grants); offset += maxPermissionBatch {
		end := offset + maxPermissionBatch
		if end > len(grants) {
			end = len(grants)
		}
		batch := grants[offset:end]
		payload := assetPermissionRequest{
			SubjectType: "All",
			SubjectID:   nil,
			Action:      "Use",
			Requests:    batch,
		}
		body, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("encode Roblox Open Use request: %w", err)
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPatch, u.baseURL+"/asset-permissions-api/v1/assets/permissions", bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("create Roblox Open Use request: %w", err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json")
		request.Header.Set("x-api-key", u.apiKey)
		response, err := u.client.Do(request)
		if err != nil {
			return fmt.Errorf("grant Roblox Open Use: %w", err)
		}
		var result assetPermissionResponse
		if err := decodeAPIResponse(response, &result); err != nil {
			return fmt.Errorf("grant Roblox Open Use: %w", err)
		}
		if len(result.Errors) != 0 {
			failures := make([]string, 0, len(result.Errors))
			for _, failure := range result.Errors {
				failures = append(failures, fmt.Sprintf("%d (%s)", failure.AssetID, failure.Code))
			}
			return fmt.Errorf("Roblox refused Open Use for: %s", strings.Join(failures, ", "))
		}
		if len(result.SuccessAssetIDs) != len(batch) {
			return fmt.Errorf("Roblox granted Open Use to %d of %d assets in batch", len(result.SuccessAssetIDs), len(batch))
		}
	}
	return nil
}

func (u *AssetUploader) createAssetLimited(
	ctx context.Context,
	request UploadRequest,
	contentType string,
	payload createAssetPayload,
) (assetOperation, error) {
	if isGeometryAsset(request.AssetType) {
		select {
		case u.modelSlots <- struct{}{}:
			defer func() { <-u.modelSlots }()
		case <-ctx.Done():
			return assetOperation{}, ctx.Err()
		}
	}
	select {
	case u.createSlots <- struct{}{}:
		defer func() { <-u.createSlots }()
	case <-ctx.Done():
		return assetOperation{}, ctx.Err()
	}
	// Check the adaptive window after acquiring a POST slot. Requests queued
	// behind an in-flight 429 therefore observe its cooldown before they start.
	if err := u.waitForCreateWindow(ctx, request.AssetType); err != nil {
		return assetOperation{}, err
	}
	return u.createAsset(ctx, request.FilePath, contentType, payload)
}

func (u *AssetUploader) validateRequest(request UploadRequest) (UploadRequest, string, error) {
	if request.ResolveMeshID && request.AssetType != AssetTypeModel {
		return request, "", errors.New("resolve-mesh-id requires a Model upload")
	}
	if strings.TrimSpace(request.FilePath) == "" {
		return request, "", errors.New("upload file path is empty")
	}
	info, err := os.Stat(request.FilePath)
	if err != nil {
		return request, "", fmt.Errorf("inspect upload file %q: %w", request.FilePath, err)
	}
	if !info.Mode().IsRegular() {
		return request, "", fmt.Errorf("upload path %q is not a regular file", request.FilePath)
	}
	if info.Size() > maxAssetUploadSize {
		return request, "", fmt.Errorf("upload file %q is %d bytes; Roblox's limit is %d", request.FilePath, info.Size(), maxAssetUploadSize)
	}
	if info.Size() == 0 {
		return request, "", fmt.Errorf("upload file %q is empty", request.FilePath)
	}
	if request.DisplayName == "" {
		base := filepath.Base(request.FilePath)
		request.DisplayName = strings.TrimSuffix(base, filepath.Ext(base))
	}
	if request.Description == "" {
		request.Description = u.description
	}
	contentType, err := uploadContentType(request.AssetType, filepath.Ext(request.FilePath))
	if err != nil {
		return request, "", err
	}
	return request, contentType, nil
}

func uploadContentType(assetType AssetType, extension string) (string, error) {
	extension = strings.ToLower(extension)
	var supported map[string]string
	switch assetType {
	case AssetTypeMesh:
		supported = map[string]string{".mesh": "model/x-file-mesh-data"}
	case AssetTypeModel:
		supported = map[string]string{
			".glb": "model/gltf-binary", ".gltf": "model/gltf+json", ".fbx": "model/fbx",
			".rbxm": "model/x-rbxm", ".rbxmx": "model/x-rbxm",
		}
	case AssetTypeImage, AssetTypeDecal:
		supported = map[string]string{
			".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
			".bmp": "image/bmp", ".tga": "image/tga",
		}
	default:
		return "", fmt.Errorf("unsupported Roblox asset type %q", assetType)
	}
	contentType := supported[extension]
	if contentType == "" {
		return "", fmt.Errorf("file extension %q is not supported for Roblox %s uploads", extension, assetType)
	}
	return contentType, nil
}

func (u *AssetUploader) createAsset(ctx context.Context, filePath, contentType string, payload createAssetPayload) (assetOperation, error) {
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return assetOperation{}, fmt.Errorf("encode Roblox asset request: %w", err)
	}
	file, err := os.Open(filePath)
	if err != nil {
		return assetOperation{}, fmt.Errorf("open upload file %q: %w", filePath, err)
	}
	reader, writer := io.Pipe()
	multipartWriter := multipart.NewWriter(writer)
	multipartContentType := multipartWriter.FormDataContentType()
	writeDone := make(chan error, 1)
	go func() {
		writeErr := writeMultipartUpload(multipartWriter, file, filepath.Base(filePath), contentType, payloadJSON)
		if closeErr := file.Close(); writeErr == nil {
			writeErr = closeErr
		}
		if closeErr := multipartWriter.Close(); writeErr == nil {
			writeErr = closeErr
		}
		_ = writer.CloseWithError(writeErr)
		writeDone <- writeErr
	}()

	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, u.baseURL+"/assets/v1/assets", reader)
	if err != nil {
		_ = reader.CloseWithError(err)
		<-writeDone
		return assetOperation{}, fmt.Errorf("create Roblox upload request: %w", err)
	}
	httpRequest.Header.Set("Content-Type", multipartContentType)
	httpRequest.Header.Set("x-api-key", u.apiKey)
	httpRequest.Header.Set("Accept", "application/json")
	response, requestErr := u.client.Do(httpRequest)
	_ = reader.Close()
	writeErr := <-writeDone
	if requestErr != nil {
		return assetOperation{}, fmt.Errorf("upload asset to Roblox: %w", requestErr)
	}
	if writeErr != nil && response.StatusCode >= 200 && response.StatusCode < 300 {
		_ = response.Body.Close()
		return assetOperation{}, fmt.Errorf("stream upload file %q: %w", filePath, writeErr)
	}
	var operation assetOperation
	if err := decodeAPIResponse(response, &operation); err != nil {
		return assetOperation{}, err
	}
	if operation.Path == "" {
		return assetOperation{}, errors.New("Roblox create-asset response did not include an operation path")
	}
	return operation, nil
}

func writeMultipartUpload(writer *multipart.Writer, file *os.File, filename, contentType string, payload []byte) error {
	requestPart, err := writer.CreateFormField("request")
	if err != nil {
		return err
	}
	if _, err := requestPart.Write(payload); err != nil {
		return err
	}
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{
		"name": "fileContent", "filename": filename,
	}))
	header.Set("Content-Type", contentType)
	filePart, err := writer.CreatePart(header)
	if err != nil {
		return err
	}
	buffer := uploadCopyBufferPool.Get().([]byte)
	defer uploadCopyBufferPool.Put(buffer)
	_, err = io.CopyBuffer(filePart, file, buffer)
	return err
}

func (u *AssetUploader) waitForOperation(ctx context.Context, operation assetOperation) (UploadedAsset, error) {
	if operation.Done {
		return finishOperation(operation)
	}
	waitContext := ctx
	cancel := func() {}
	if u.pollTimeout > 0 {
		waitContext, cancel = context.WithTimeout(ctx, u.pollTimeout)
	}
	defer cancel()
	failedAttempts := 0
	for {
		current, err := u.getOperation(waitContext, operation.Path)
		if err != nil {
			delay, retry := u.retryDelay(err, failedAttempts)
			if !retry {
				return UploadedAsset{}, err
			}
			failedAttempts++
			if err := sleepContext(waitContext, delay); err != nil {
				return UploadedAsset{}, fmt.Errorf("wait for Roblox operation %q: %w", operation.Path, err)
			}
			continue
		}
		failedAttempts = 0
		if current.Done {
			return finishOperation(current)
		}
		timer := time.NewTimer(u.pollInterval)
		select {
		case <-waitContext.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return UploadedAsset{}, fmt.Errorf("wait for Roblox operation %q: %w", operation.Path, waitContext.Err())
		case <-timer.C:
		}
	}
}

func (u *AssetUploader) getOperation(ctx context.Context, operationPath string) (assetOperation, error) {
	const prefix = "operations/"
	if !strings.HasPrefix(operationPath, prefix) {
		return assetOperation{}, fmt.Errorf("invalid Roblox operation path %q", operationPath)
	}
	operationID := strings.TrimPrefix(operationPath, prefix)
	if operationID == "" || strings.Contains(operationID, "/") {
		return assetOperation{}, fmt.Errorf("invalid Roblox operation path %q", operationPath)
	}
	endpoint := u.baseURL + "/assets/v1/operations/" + url.PathEscape(operationID)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return assetOperation{}, fmt.Errorf("create Roblox operation request: %w", err)
	}
	request.Header.Set("x-api-key", u.apiKey)
	request.Header.Set("Accept", "application/json")
	response, err := u.client.Do(request)
	if err != nil {
		return assetOperation{}, fmt.Errorf("get Roblox operation %q: %w", operationPath, err)
	}
	var operation assetOperation
	if err := decodeAPIResponse(response, &operation); err != nil {
		return assetOperation{}, err
	}
	if operation.Path == "" {
		operation.Path = operationPath
	}
	return operation, nil
}

func finishOperation(operation assetOperation) (UploadedAsset, error) {
	if operation.Error != nil {
		message := strings.TrimSpace(operation.Error.Message)
		if message == "" {
			message = "unknown operation failure"
		}
		return UploadedAsset{}, fmt.Errorf("Roblox operation %q failed (code %d): %s", operation.Path, operation.Error.Code, message)
	}
	if operation.Response == nil {
		return UploadedAsset{}, fmt.Errorf("Roblox operation %q completed without an asset response", operation.Path)
	}
	asset := *operation.Response
	asset.OperationPath = operation.Path
	if asset.AssetID == "" {
		return UploadedAsset{}, fmt.Errorf("Roblox operation %q completed without an asset ID", operation.Path)
	}
	return asset, nil
}

func decodeAPIResponse(response *http.Response, destination any) error {
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		return &APIError{
			StatusCode: response.StatusCode,
			Status:     response.Status,
			Body:       strings.TrimSpace(string(body)),
			RetryAfter: responseRetryDelay(response.Header, time.Now()),
		}
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 4<<20))
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("decode Roblox Assets API response: %w", err)
	}
	return nil
}

func (u *AssetUploader) retryDelay(err error, attempt int) (time.Duration, bool) {
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusTooManyRequests || attempt >= u.maxRetries {
		return 0, false
	}
	delay := u.retryBackoff(attempt)
	if apiErr.RetryAfter > delay {
		delay = apiErr.RetryAfter
	}
	return delay, true
}

func (u *AssetUploader) retryBackoff(attempt int) time.Duration {
	delay := u.retryBase
	for step := 0; step < attempt && delay < u.retryMax; step++ {
		if delay > u.retryMax/2 {
			delay = u.retryMax
			break
		}
		delay *= 2
	}
	if delay > u.retryMax {
		delay = u.retryMax
	}
	return delay
}

// noteRateLimit turns burst uploads into a short, shared leaky-bucket stream.
// It only activates after Roblox returns 429, preserving maximum speed while
// quota is available and preventing a synchronized retry burst afterward.
func (u *AssetUploader) noteRateLimit(assetType AssetType, delay time.Duration, attempt int) {
	now := time.Now()
	blockedUntil := now.Add(delay)
	spacing := 50 * time.Millisecond
	for step := 0; step < attempt && spacing < time.Second; step++ {
		spacing *= 2
	}
	if spacing > time.Second {
		spacing = time.Second
	}
	window := u.rateWindow(assetType)
	window.mu.Lock()
	if blockedUntil.After(window.blockedUntil) {
		window.blockedUntil = blockedUntil
	}
	if spacing > window.requestSpacing {
		window.requestSpacing = spacing
	}
	window.mu.Unlock()
}

func (u *AssetUploader) waitForCreateWindow(ctx context.Context, assetType AssetType) error {
	window := u.rateWindow(assetType)
	for {
		window.mu.Lock()
		now := time.Now()
		ready := window.blockedUntil
		if window.nextCreate.After(ready) {
			ready = window.nextCreate
		}
		if !ready.After(now) {
			if window.requestSpacing > 0 {
				window.nextCreate = now.Add(window.requestSpacing)
			}
			window.mu.Unlock()
			return nil
		}
		window.mu.Unlock()
		if err := sleepContext(ctx, time.Until(ready)); err != nil {
			return err
		}
	}
}

func (u *AssetUploader) rateWindow(assetType AssetType) *rateWindow {
	if isGeometryAsset(assetType) {
		return &u.modelRate
	}
	return &u.decalRate
}

func isGeometryAsset(assetType AssetType) bool {
	return assetType == AssetTypeMesh || assetType == AssetTypeModel
}

func matchesAssetType(expected AssetType, actual string) bool {
	actual = strings.TrimSpace(strings.ToUpper(actual))
	if actual == "" {
		// Some test doubles and older successful operation responses omit this
		// redundant field. A present field must always match.
		return true
	}
	actual = strings.TrimPrefix(actual, "ASSET_TYPE_")
	return actual == strings.ToUpper(string(expected))
}

func responseRetryDelay(header http.Header, now time.Time) time.Duration {
	var longest time.Duration
	if value := strings.TrimSpace(header.Get("Retry-After")); value != "" {
		if seconds, err := strconv.ParseFloat(value, 64); err == nil && seconds >= 0 {
			longest = time.Duration(seconds * float64(time.Second))
		} else if retryAt, err := http.ParseTime(value); err == nil && retryAt.After(now) {
			longest = retryAt.Sub(now)
		}
	}
	if value := strings.TrimSpace(header.Get("x-ratelimit-reset")); value != "" {
		// Some endpoints return multiple comma-separated windows. Waiting for the
		// longest advertised reset avoids retrying against another exhausted one.
		for _, part := range strings.Split(value, ",") {
			part = strings.TrimSpace(strings.SplitN(part, ";", 2)[0])
			if seconds, err := strconv.ParseFloat(part, 64); err == nil && seconds >= 0 {
				if delay := time.Duration(seconds * float64(time.Second)); delay > longest {
					longest = delay
				}
			}
		}
	}
	return longest
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// UploadMany uploads all jobs concurrently, preserves input ordering, and
// returns per-file errors without cancelling successful independent uploads.
// POST concurrency is bounded inside Upload; accepted asynchronous operations
// do not consume a POST slot while Roblox processes and the client polls them.
func (u *AssetUploader) UploadMany(ctx context.Context, jobs []UploadRequest) []UploadResult {
	return u.UploadManyWithProgress(ctx, jobs, nil)
}

// UploadManyWithProgress behaves like UploadMany and calls progress once as
// each independent Roblox upload finishes. Callbacks are serialized even
// though the uploads themselves run concurrently.
func (u *AssetUploader) UploadManyWithProgress(ctx context.Context, jobs []UploadRequest, progress func(index int, result UploadResult)) []UploadResult {
	results := make([]UploadResult, len(jobs))
	if len(jobs) == 0 {
		return results
	}
	completed := make(chan int, len(jobs))
	var wait sync.WaitGroup
	wait.Add(len(jobs))
	for index := range jobs {
		index := index
		go func() {
			defer wait.Done()
			defer func() { completed <- index }()
			job := jobs[index]
			results[index].Request = job
			asset, err := u.Upload(ctx, job)
			if err != nil {
				results[index].Error = err.Error()
				return
			}
			results[index].Asset = &asset
		}()
	}
	for range jobs {
		index := <-completed
		if progress != nil {
			progress(index, results[index])
		}
	}
	wait.Wait()
	if u.assetPrivacy == AssetPrivacyOpenUse {
		assetIDs := make([]string, 0, len(results))
		for _, result := range results {
			if result.Error == "" && result.Asset != nil && result.Asset.AssetID != "" &&
				(result.Request.AssetType == AssetTypeMesh || result.Request.AssetType == AssetTypeImage || result.Request.AssetType == AssetTypeDecal || result.Request.ResolveMeshID) {
				assetIDs = append(assetIDs, result.Asset.AssetID)
			}
		}
		if err := u.GrantOpenUse(ctx, assetIDs); err != nil {
			for index := range results {
				if results[index].Error == "" && results[index].Asset != nil &&
					(results[index].Request.AssetType == AssetTypeMesh || results[index].Request.AssetType == AssetTypeImage || results[index].Request.AssetType == AssetTypeDecal || results[index].Request.ResolveMeshID) {
					results[index].Asset = nil
					results[index].Error = err.Error()
				}
			}
		}
	}
	return results
}
