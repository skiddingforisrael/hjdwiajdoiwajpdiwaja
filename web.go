package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/qaustria/AutoPack-Go/packstore"
	"github.com/qaustria/AutoPack-Go/utils"
)

//go:embed web/*
var embeddedWebFiles embed.FS

type uploadProcessor interface {
	ProcessUpload(context.Context, string, io.Reader, ProgressFunc) (PipelineResult, error)
}

type requestProcessorFactory func(context.Context, string, string) (uploadProcessor, error)

const (
	robloxAPIKeyHeader        = "X-Cone-Roblox-Api-Key"
	robloxUserIDHeader        = "X-Cone-Roblox-User-Id"
	batchIndexHeader          = "X-Cone-Batch-Index"
	batchTotalHeader          = "X-Cone-Batch-Total"
	batchTokenHeader          = "X-Cone-Batch-Token"
	defaultMaxConcurrentPorts = 2
)

var errBatchAuthorization = errors.New("administrative batch credentials are invalid")

type WebHandlerOptions struct {
	Notifier           PortNotifier
	Store              *packstore.Store
	MaxConcurrentPorts int
	BatchToken         string
}

type webHandler struct {
	processor        uploadProcessor
	processorFactory requestProcessorFactory
	notifier         PortNotifier
	packStore        *packstore.Store
	static           http.Handler
	activeJobsMu     sync.Mutex
	activeJobs       map[[sha256.Size]byte]struct{}
	jobSlots         chan struct{}
	batchToken       string
}

type webStreamEvent struct {
	Type     string          `json:"type"`
	Progress *ProgressEvent  `json:"progress,omitempty"`
	Result   *PipelineResult `json:"result,omitempty"`
	Filename string          `json:"filename,omitempty"`
	PackID   string          `json:"packId,omitempty"`
	Message  string          `json:"message,omitempty"`
}

func NewWebHandler(processor uploadProcessor) (http.Handler, error) {
	if processor == nil {
		return nil, errors.New("web upload processor is nil")
	}
	assets, err := fs.Sub(embeddedWebFiles, "web")
	if err != nil {
		return nil, fmt.Errorf("load embedded web files: %w", err)
	}
	handler := newWebHandler(assets, defaultMaxConcurrentPorts, "")
	handler.processor = processor
	return handler.routes(), nil
}

// NewCredentialWebHandler creates the public-site handler. Each conversion
// gets a fresh processor made from credentials supplied with that request;
// keys are never retained on the handler or written to disk.
func NewCredentialWebHandler(factory requestProcessorFactory) (http.Handler, error) {
	return NewCredentialWebHandlerWithNotifier(factory, nil)
}

func NewCredentialWebHandlerWithNotifier(factory requestProcessorFactory, notifier PortNotifier) (http.Handler, error) {
	return NewCredentialWebHandlerWithServices(factory, notifier, nil)
}

// NewCredentialWebHandlerWithServices creates a public handler with optional
// Discord notification and durable successful-port history.
func NewCredentialWebHandlerWithServices(factory requestProcessorFactory, notifier PortNotifier, store *packstore.Store) (http.Handler, error) {
	return NewCredentialWebHandlerWithOptions(factory, WebHandlerOptions{
		Notifier: notifier, Store: store, MaxConcurrentPorts: defaultMaxConcurrentPorts,
	})
}

// NewCredentialWebHandlerWithOptions creates a public handler with explicit
// service dependencies and a process-wide conversion ceiling.
func NewCredentialWebHandlerWithOptions(factory requestProcessorFactory, options WebHandlerOptions) (http.Handler, error) {
	if factory == nil {
		return nil, errors.New("web request processor factory is nil")
	}
	if options.MaxConcurrentPorts < 1 || options.MaxConcurrentPorts > 32 {
		return nil, errors.New("maximum concurrent ports must be between 1 and 32")
	}
	options.BatchToken = strings.TrimSpace(options.BatchToken)
	if options.BatchToken != "" && (len(options.BatchToken) < 32 || len(options.BatchToken) > 256) {
		return nil, errors.New("administrative batch token must be between 32 and 256 characters")
	}
	assets, err := fs.Sub(embeddedWebFiles, "web")
	if err != nil {
		return nil, fmt.Errorf("load embedded web files: %w", err)
	}
	handler := newWebHandler(assets, options.MaxConcurrentPorts, options.BatchToken)
	handler.processorFactory = factory
	handler.notifier = options.Notifier
	handler.packStore = options.Store
	return handler.routes(), nil
}

func newWebHandler(assets fs.FS, maxConcurrentPorts int, batchToken string) *webHandler {
	return &webHandler{
		static:     http.FileServer(http.FS(assets)),
		activeJobs: make(map[[sha256.Size]byte]struct{}),
		jobSlots:   make(chan struct{}, maxConcurrentPorts),
		batchToken: batchToken,
	}
}

func (h *webHandler) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/convert", h.convert)
	mux.HandleFunc("/api/skies", h.skies)
	mux.HandleFunc("/api/library", h.library)
	mux.HandleFunc("/api/library/", h.libraryItem)
	mux.HandleFunc("/healthz", h.health)
	mux.Handle("/", h.static)
	return securityHeaders(mux)
}

func (h *webHandler) health(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		response.Header().Set("Allow", http.MethodGet)
		http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	response.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(response).Encode(map[string]string{"status": "ok", "version": utils.Version})
}

// libraryEntry is the lightweight, list-friendly view of a packstore.Record:
// enough to render a Library row (name, ID, timestamp, size, whether a
// preview image exists) without shipping every stored port's full JSON
// output and PNG bytes in one response.
type libraryEntry struct {
	Sequence   uint64    `json:"sequence"`
	PackID     string    `json:"packId"`
	PackName   string    `json:"packName"`
	CreatedAt  time.Time `json:"createdAt"`
	SizeBytes  int       `json:"sizeBytes"`
	HasPreview bool      `json:"hasPreview"`
}

// library serves the Library page's port history list: every pack this Cone
// instance has ported, newest first.
func (h *webHandler) library(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		response.Header().Set("Allow", http.MethodGet)
		http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	response.Header().Set("Content-Type", "application/json")
	response.Header().Set("Cache-Control", "no-store")
	if h.packStore == nil {
		_ = json.NewEncoder(response).Encode(map[string]any{"packs": []libraryEntry{}})
		return
	}
	records, err := h.packStore.List(request.Context(), 0)
	if err != nil {
		http.Error(response, "read port history: "+err.Error(), http.StatusInternalServerError)
		return
	}
	entries := make([]libraryEntry, len(records))
	for index, record := range records {
		entries[index] = libraryEntry{
			Sequence:   record.Sequence,
			PackID:     record.PackID,
			PackName:   record.PackName,
			CreatedAt:  record.CreatedAt,
			SizeBytes:  len(record.OutputJSON),
			HasPreview: len(record.PreviewPNG) > 0,
		}
	}
	_ = json.NewEncoder(response).Encode(map[string]any{"packs": entries})
}

// libraryItem serves per-pack Library resources:
//
//	GET /api/library/{sequence}/code          -> the stored port's JSON output
//	GET /api/library/{sequence}/preview.png   -> the stored hotbar preview PNG
func (h *webHandler) libraryItem(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		response.Header().Set("Allow", http.MethodGet)
		http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.packStore == nil {
		http.NotFound(response, request)
		return
	}
	rest := strings.TrimPrefix(request.URL.Path, "/api/library/")
	sequenceString, action, ok := strings.Cut(rest, "/")
	if !ok || sequenceString == "" || action == "" {
		http.NotFound(response, request)
		return
	}
	sequence, err := strconv.ParseUint(sequenceString, 10, 64)
	if err != nil {
		http.Error(response, "invalid pack sequence", http.StatusBadRequest)
		return
	}
	record, found, err := h.packStore.Get(request.Context(), sequence)
	if err != nil {
		http.Error(response, "read pack record: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !found {
		http.NotFound(response, request)
		return
	}
	switch action {
	case "code":
		response.Header().Set("Content-Type", "application/json")
		response.Header().Set("Cache-Control", "no-store")
		_, _ = response.Write(record.OutputJSON)
	case "preview.png":
		if len(record.PreviewPNG) == 0 {
			http.NotFound(response, request)
			return
		}
		response.Header().Set("Content-Type", "image/png")
		// Sequence numbers repeat across a fresh database, so a long-lived
		// immutable cache header would keep serving a stale image for that
		// URL forever. Always fetch fresh; these PNGs are only a few KB.
		response.Header().Set("Cache-Control", "no-store")
		_, _ = response.Write(record.PreviewPNG)
	default:
		http.NotFound(response, request)
	}
}

// skies scans an uploaded ZIP for every custom-sky sheet it ships
// (cloud1.png, cloud2.png, ...) without running a full port, so the web UI
// can ask which sky to use before committing to one. Packs with at most one
// sky sheet - the normal case - get back an empty or single-item list,
// telling the client not to bother asking.
func (h *webHandler) skies(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		response.Header().Set("Allow", http.MethodPost)
		http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, DefaultMaxTexturePackUploadBytes+(8<<20))
	part, _, err := texturePackPart(request)
	if err != nil {
		http.Error(response, err.Error(), http.StatusBadRequest)
		return
	}
	defer part.Close()
	stagedPath, cleanup, err := stageUploadedZip(part)
	if err != nil {
		http.Error(response, err.Error(), http.StatusBadRequest)
		return
	}
	defer cleanup()
	options, err := utils.DetectSkyOptions(stagedPath)
	if err != nil {
		http.Error(response, "scan texture pack for skies: "+err.Error(), http.StatusBadRequest)
		return
	}
	type skyOptionView struct {
		ID           string `json:"id"`
		Label        string `json:"label"`
		ThumbnailPNG string `json:"thumbnailPng"`
	}
	views := make([]skyOptionView, len(options))
	for index, option := range options {
		views[index] = skyOptionView{
			ID:           option.ID,
			Label:        option.Label,
			ThumbnailPNG: "data:image/png;base64," + base64.StdEncoding.EncodeToString(option.ThumbnailPNG),
		}
	}
	response.Header().Set("Content-Type", "application/json")
	response.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(response).Encode(map[string]any{"skies": views})
}

// stageUploadedZip copies a multipart upload to a private temporary file so
// it can be inspected or re-read more than once (a raw multipart.Part can
// only be read forward, once). The caller must call the returned cleanup
// func once done with the path.
func stageUploadedZip(part io.Reader) (string, func(), error) {
	file, err := os.CreateTemp("", "autopack-web-upload-*.zip")
	if err != nil {
		return "", func() {}, fmt.Errorf("stage texture-pack upload: %w", err)
	}
	path := file.Name()
	cleanup := func() { _ = os.Remove(path) }
	limited := &io.LimitedReader{R: part, N: DefaultMaxTexturePackUploadBytes + 1}
	written, copyErr := io.Copy(file, limited)
	closeErr := file.Close()
	if copyErr != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("read texture-pack upload: %w", copyErr)
	}
	if closeErr != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("close staged texture-pack upload: %w", closeErr)
	}
	if written == 0 {
		cleanup()
		return "", func() {}, errors.New("texture-pack upload is empty")
	}
	if written > DefaultMaxTexturePackUploadBytes {
		cleanup()
		return "", func() {}, fmt.Errorf("texture-pack upload exceeds %d-byte limit", DefaultMaxTexturePackUploadBytes)
	}
	return path, cleanup, nil
}

func (h *webHandler) convert(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		response.Header().Set("Allow", http.MethodPost)
		http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	processor := h.processor
	batchIndex, batchTotal, err := batchPositionFromHeaders(request.Header, h.batchToken)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errBatchAuthorization) {
			status = http.StatusForbidden
		}
		http.Error(response, err.Error(), status)
		return
	}
	jobKey := sha256.Sum256([]byte("cone-shared-web-processor"))
	var apiKey, userID string
	if h.processorFactory != nil {
		apiKey = strings.TrimSpace(request.Header.Get(robloxAPIKeyHeader))
		userID = strings.TrimSpace(request.Header.Get(robloxUserIDHeader))
		if apiKey == "" || userID == "" {
			http.Error(response, "Roblox API key and user ID are required", http.StatusBadRequest)
			return
		}
		if len(apiKey) > 8192 || len(userID) > 32 {
			http.Error(response, "Roblox credentials are too long", http.StatusBadRequest)
			return
		}
		jobKey = sha256.Sum256([]byte(apiKey))
	}
	if err := h.reserveJob(jobKey); err != nil {
		http.Error(response, err.Error(), http.StatusTooManyRequests)
		return
	}
	defer h.releaseJob(jobKey)
	if h.processorFactory != nil {
		var err error
		processor, err = h.processorFactory(request.Context(), apiKey, userID)
		if err != nil {
			http.Error(response, err.Error(), http.StatusUnauthorized)
			return
		}
	}
	request.Body = http.MaxBytesReader(response, request.Body, DefaultMaxTexturePackUploadBytes+(8<<20))
	part, formFields, err := texturePackPart(request)
	if err != nil {
		http.Error(response, err.Error(), http.StatusBadRequest)
		return
	}
	defer part.Close()
	filename := filepath.Base(strings.ReplaceAll(part.FileName(), "\\", "/"))
	if filename == "." || filename == "" {
		filename = "texture-pack.zip"
	}

	stagedPath, stageCleanup, err := stageUploadedZip(part)
	if err != nil {
		http.Error(response, err.Error(), http.StatusBadRequest)
		return
	}
	defer stageCleanup()

	mainZipPath := stagedPath
	if primarySky := formFields["skyPrimary"]; primarySky != "" {
		rewrittenPath, rewriteCleanup, rewriteErr := utils.RewritePrimarySky(stagedPath, primarySky)
		if rewriteErr != nil {
			http.Error(response, rewriteErr.Error(), http.StatusBadRequest)
			return
		}
		defer rewriteCleanup()
		mainZipPath = rewrittenPath
	}
	var extraSkyIDs []string
	if rawExtras := formFields["skyExtras"]; rawExtras != "" {
		if jsonErr := json.Unmarshal([]byte(rawExtras), &extraSkyIDs); jsonErr != nil {
			http.Error(response, "invalid skyExtras field", http.StatusBadRequest)
			return
		}
	}

	response.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	response.Header().Set("Cache-Control", "no-store")
	response.Header().Set("X-Accel-Buffering", "no")
	encoder := json.NewEncoder(response)
	flusher, _ := response.(http.Flusher)
	writeEvent := func(event webStreamEvent) {
		if err := encoder.Encode(event); err == nil && flusher != nil {
			flusher.Flush()
		}
	}
	onProgress := func(progress ProgressEvent) {
		writeEvent(webStreamEvent{Type: "progress", Progress: &progress})
	}
	mainFile, err := os.Open(mainZipPath)
	if err != nil {
		writeEvent(webStreamEvent{Type: "error", Message: err.Error()})
		return
	}
	result, err := processor.ProcessUpload(request.Context(), filename, mainFile, onProgress)
	_ = mainFile.Close()
	if err != nil {
		writeEvent(webStreamEvent{Type: "error", Message: err.Error()})
		return
	}
	outputJSON, err := json.Marshal(result)
	if err != nil {
		writeEvent(webStreamEvent{Type: "error", Message: err.Error()})
		return
	}
	if h.packStore != nil {
		// Once Roblox has completed a port, retain its record even if the browser
		// closes before the response stream finishes.
		storeContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, storeErr := h.packStore.Save(storeContext, packstore.Record{
			PackID: result.PackID, PackName: result.PackName, OutputJSON: outputJSON,
			PreviewPNG: result.PreviewPNG,
		})
		cancel()
		if storeErr != nil {
			writeEvent(webStreamEvent{Type: "error", Message: "save port history: " + storeErr.Error()})
			return
		}
	}
	if h.notifier != nil {
		notifyContext, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		_ = h.notifier.Notify(notifyContext, PortNotification{
			PackID: result.PackID, PackName: result.PackName,
			OutputJSON: outputJSON, PreviewPNG: result.PreviewPNG,
			BatchIndex: batchIndex, BatchTotal: batchTotal,
		})
		cancel()
	}
	writeEvent(webStreamEvent{
		Type: "result", Result: &result, Filename: downloadFilename(filename), PackID: result.PackID,
	})

	// Any additional skies the person chose to keep as their own separate
	// "sky-only" packs get ported one at a time after the main pack, each
	// logged as an ordinary progress line rather than a second result
	// screen, and saved straight to the library.
	for _, skyID := range extraSkyIDs {
		extraZipPath, extraCleanup, buildErr := utils.BuildSingleSkyZip(stagedPath, skyID)
		if buildErr != nil {
			writeEvent(webStreamEvent{Type: "progress", Progress: &ProgressEvent{
				Stage: ProgressFailed, Error: buildErr.Error(),
				Message: "Skipped an extra sky pack",
			}})
			continue
		}
		extraLabel := utils.SkyLabelForEntry(skyID)
		extraFile, openErr := os.Open(extraZipPath)
		if openErr != nil {
			writeEvent(webStreamEvent{Type: "progress", Progress: &ProgressEvent{
				Stage: ProgressFailed, Error: openErr.Error(),
				Message: "Skipped extra sky pack: " + extraLabel,
			}})
			extraCleanup()
			continue
		}
		extraResult, extraErr := processor.ProcessUpload(request.Context(), filename, extraFile, onProgress)
		_ = extraFile.Close()
		extraCleanup()
		if extraErr != nil {
			writeEvent(webStreamEvent{Type: "progress", Progress: &ProgressEvent{
				Stage: ProgressFailed, Error: extraErr.Error(),
				Message: "Skipped extra sky pack: " + extraLabel,
			}})
			continue
		}
		extraResult.PackName = strings.TrimSpace(result.PackName + " " + strings.ReplaceAll(extraLabel, "·", ""))
		extraOutputJSON, marshalErr := json.Marshal(extraResult)
		if marshalErr != nil {
			writeEvent(webStreamEvent{Type: "progress", Progress: &ProgressEvent{
				Stage: ProgressFailed, Error: marshalErr.Error(),
				Message: "Skipped extra sky pack: " + extraLabel,
			}})
			continue
		}
		if h.packStore == nil {
			writeEvent(webStreamEvent{Type: "progress", Progress: &ProgressEvent{
				Stage: ProgressComplete, Message: "Ported extra sky pack: " + extraLabel,
			}})
			continue
		}
		storeContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, storeErr := h.packStore.Save(storeContext, packstore.Record{
			PackID: extraResult.PackID, PackName: extraResult.PackName, OutputJSON: extraOutputJSON,
			PreviewPNG: extraResult.PreviewPNG,
		})
		cancel()
		if storeErr != nil {
			writeEvent(webStreamEvent{Type: "progress", Progress: &ProgressEvent{
				Stage: ProgressFailed, Error: storeErr.Error(),
				Message: "Ported but couldn't save extra sky pack: " + extraLabel,
			}})
			continue
		}
		writeEvent(webStreamEvent{Type: "progress", Progress: &ProgressEvent{
			Stage:   ProgressComplete,
			Message: "Ported extra sky pack: " + extraLabel,
		}})
	}
}

func batchPositionFromHeaders(headers http.Header, configuredToken string) (int, int, error) {
	indexValue := strings.TrimSpace(headers.Get(batchIndexHeader))
	totalValue := strings.TrimSpace(headers.Get(batchTotalHeader))
	providedToken := strings.TrimSpace(headers.Get(batchTokenHeader))
	if indexValue == "" && totalValue == "" {
		if providedToken != "" {
			return 0, 0, errors.New("administrative batch token requires batch position headers")
		}
		return 0, 0, nil
	}
	if indexValue == "" || totalValue == "" {
		return 0, 0, errors.New("batch index and total headers must be provided together")
	}
	if configuredToken == "" || providedToken == "" || len(configuredToken) != len(providedToken) ||
		subtle.ConstantTimeCompare([]byte(configuredToken), []byte(providedToken)) != 1 {
		return 0, 0, errBatchAuthorization
	}
	index, indexErr := strconv.Atoi(indexValue)
	total, totalErr := strconv.Atoi(totalValue)
	if indexErr != nil || totalErr != nil || index < 1 || total < index || total > maxBatchQueueEntries {
		return 0, 0, fmt.Errorf("batch position must satisfy 1 <= index <= total <= %d", maxBatchQueueEntries)
	}
	return index, total, nil
}

// reserveJob prevents duplicate work per Roblox credential and caps total
// conversion memory across all credentials. Only the SHA-256 key digest is
// retained for the duration of a job.
func (h *webHandler) reserveJob(key [sha256.Size]byte) error {
	h.activeJobsMu.Lock()
	defer h.activeJobsMu.Unlock()
	if _, active := h.activeJobs[key]; active {
		return errors.New("another texture pack is already being processed with this Roblox API key")
	}
	select {
	case h.jobSlots <- struct{}{}:
	default:
		return errors.New("Cone is at its safe processing limit; try again after a current port finishes")
	}
	h.activeJobs[key] = struct{}{}
	return nil
}

func (h *webHandler) releaseJob(key [sha256.Size]byte) {
	h.activeJobsMu.Lock()
	delete(h.activeJobs, key)
	<-h.jobSlots
	h.activeJobsMu.Unlock()
}

// texturePackPart scans the multipart request for the uploaded ZIP's "pack"
// field. Any small text fields that arrive before it in the request body
// (such as the sky-state checkboxes) are collected into formFields, so
// callers can act on both without buffering the ZIP itself into memory. The
// web UI must send those fields before the "pack" file field for them to be
// seen, since the underlying reader is forward-only.
func texturePackPart(request *http.Request) (*multipart.Part, map[string]string, error) {
	reader, err := request.MultipartReader()
	if err != nil {
		return nil, nil, errors.New("request must be multipart form data")
	}
	formFields := make(map[string]string)
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			return nil, formFields, errors.New("missing texture-pack ZIP field named pack")
		}
		if err != nil {
			return nil, formFields, fmt.Errorf("read multipart upload: %w", err)
		}
		if part.FormName() == "pack" && part.FileName() != "" {
			return part, formFields, nil
		}
		if part.FileName() == "" {
			value, readErr := io.ReadAll(io.LimitReader(part, 256))
			closeErr := part.Close()
			if readErr != nil {
				return nil, formFields, fmt.Errorf("read multipart field %q: %w", part.FormName(), readErr)
			}
			if closeErr != nil {
				return nil, formFields, fmt.Errorf("close multipart field %q: %w", part.FormName(), closeErr)
			}
			formFields[part.FormName()] = strings.TrimSpace(string(value))
			continue
		}
		_ = part.Close()
	}
}

func downloadFilename(uploadName string) string {
	name := filepath.Base(strings.ReplaceAll(uploadName, "\\", "/"))
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	if strings.TrimSpace(stem) == "" {
		stem = "texture-pack"
	}
	return stem + "_cone.json"
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		response.Header().Set("Referrer-Policy", "no-referrer")
		response.Header().Set("X-Content-Type-Options", "nosniff")
		response.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(response, request)
	})
}
