package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// DefaultMaxTexturePackUploadBytes is the default request-body limit a future
// web handler can advertise and enforce before unpacking a texture pack.
const DefaultMaxTexturePackUploadBytes int64 = 128 << 20

// ProgressStage is stable and JSON-friendly for a future web progress stream.
type ProgressStage string

const (
	ProgressReceiving ProgressStage = "receiving"
	ProgressPreparing ProgressStage = "preparing"
	ProgressUploading ProgressStage = "uploading"
	ProgressNotifying ProgressStage = "notifying"
	ProgressComplete  ProgressStage = "complete"
	ProgressFailed    ProgressStage = "failed"
)

// ProgressEvent contains no credentials or temporary filesystem paths, so it
// is safe to forward directly to a local web client.
type ProgressEvent struct {
	Stage     ProgressStage `json:"stage"`
	Completed int           `json:"completed,omitempty"`
	Total     int           `json:"total,omitempty"`
	Name      string        `json:"name,omitempty"`
	Message   string        `json:"message,omitempty"`
	Error     string        `json:"error,omitempty"`
}

// ProgressFunc receives synchronous lifecycle updates. A web frontend can map
// these events to SSE or WebSocket messages without coupling HTTP to meshing.
type ProgressFunc func(ProgressEvent)

// ProcessorConfig configures the UI-neutral texture-pack processor.
type ProcessorConfig struct {
	Uploader       UploadBatcher
	MaxUploadBytes int64
}

// Processor accepts both filesystem paths (CLI) and streams (multipart web
// uploads), while keeping asset preparation and credential use in one place.
type Processor struct {
	uploader       UploadBatcher
	maxUploadBytes int64
}

func NewProcessor(config ProcessorConfig) (*Processor, error) {
	if config.Uploader == nil {
		return nil, errors.New("processor upload client is nil")
	}
	limit := config.MaxUploadBytes
	if limit == 0 {
		limit = DefaultMaxTexturePackUploadBytes
	}
	if limit < 0 {
		return nil, errors.New("processor upload limit must not be negative")
	}
	return &Processor{uploader: config.Uploader, maxUploadBytes: limit}, nil
}

// Validate checks backend permissions once at CLI or web-server startup when
// the configured uploader supports validation.
func (p *Processor) Validate(ctx context.Context) error {
	if p == nil {
		return errors.New("processor is nil")
	}
	if ctx == nil {
		return errors.New("processor context is nil")
	}
	validator, ok := p.uploader.(interface {
		ValidateOpenUsePermission(context.Context) error
	})
	if !ok {
		return nil
	}
	return validator.ValidateOpenUsePermission(ctx)
}

// ProcessPath processes an existing ZIP and returns the JSON-ready result.
// It does not choose an output path or write a response, keeping it reusable by
// the CLI and a future HTTP handler.
func (p *Processor) ProcessPath(ctx context.Context, zipPath string, progress ProgressFunc) (PipelineResult, error) {
	if p == nil {
		return PipelineResult{}, errors.New("processor is nil")
	}
	if ctx == nil {
		return PipelineResult{}, errors.New("processor context is nil")
	}
	validatedPath, err := validateTexturePackPath(zipPath)
	if err != nil {
		emitProgress(progress, ProgressEvent{Stage: ProgressFailed, Error: err.Error()})
		return PipelineResult{}, err
	}
	if err := ctx.Err(); err != nil {
		emitProgress(progress, ProgressEvent{Stage: ProgressFailed, Error: err.Error()})
		return PipelineResult{}, err
	}
	packID, err := texturePackID(validatedPath)
	if err != nil {
		emitProgress(progress, ProgressEvent{Stage: ProgressFailed, Error: err.Error()})
		return PipelineResult{}, err
	}
	emitProgress(progress, ProgressEvent{Stage: ProgressPreparing, Message: "Opening texture-pack ZIP"})
	lastTotal := 0
	result, err := runPipeline(ctx, validatedPath, p.uploader, func(done, total int, name string, uploadErr error) {
		lastTotal = total
		event := ProgressEvent{
			Stage: ProgressUploading, Completed: done, Total: total, Name: name,
			Message: fmt.Sprintf("Uploaded %s", name),
		}
		if uploadErr != nil {
			event.Error = uploadErr.Error()
			event.Message = fmt.Sprintf("Skipped %s; using default asset", name)
		}
		emitProgress(progress, event)
	}, func(message string) {
		emitProgress(progress, ProgressEvent{Stage: ProgressPreparing, Message: message})
	})
	if err != nil {
		emitProgress(progress, ProgressEvent{Stage: ProgressFailed, Completed: lastTotal, Total: lastTotal, Error: err.Error()})
		return PipelineResult{}, err
	}
	result.PackID = packID
	result.PackName = sanitizePackName(filepath.Base(validatedPath))
	emitProgress(progress, ProgressEvent{
		Stage: ProgressComplete, Completed: lastTotal, Total: lastTotal,
		Message: "Generated JSON mapping",
	})
	return result, nil
}

// ProcessUpload safely stages a streamed ZIP (such as multipart.File), applies
// a hard compressed-size limit, and removes it immediately after processing.
func (p *Processor) ProcessUpload(ctx context.Context, filename string, upload io.Reader, progress ProgressFunc) (PipelineResult, error) {
	fail := func(err error) (PipelineResult, error) {
		emitProgress(progress, ProgressEvent{Stage: ProgressFailed, Error: err.Error()})
		return PipelineResult{}, err
	}
	if p == nil {
		return fail(errors.New("processor is nil"))
	}
	if ctx == nil {
		return fail(errors.New("processor context is nil"))
	}
	if upload == nil {
		return fail(errors.New("texture-pack upload is nil"))
	}
	if !strings.EqualFold(filepath.Ext(strings.TrimSpace(filename)), ".zip") {
		return fail(errors.New("texture-pack upload must have a .zip extension"))
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	file, err := os.CreateTemp("", "autopack-upload-*.zip")
	if err != nil {
		return fail(fmt.Errorf("stage texture-pack upload: %w", err))
	}
	path := file.Name()
	defer os.Remove(path)
	reader := &io.LimitedReader{R: upload, N: p.maxUploadBytes + 1}
	written, copyErr := io.Copy(file, reader)
	closeErr := file.Close()
	if copyErr != nil {
		return fail(fmt.Errorf("read texture-pack upload: %w", copyErr))
	}
	if closeErr != nil {
		return fail(fmt.Errorf("close staged texture-pack upload: %w", closeErr))
	}
	if written == 0 {
		return fail(errors.New("texture-pack upload is empty"))
	}
	if written > p.maxUploadBytes {
		return fail(fmt.Errorf("texture-pack upload exceeds %d-byte limit", p.maxUploadBytes))
	}

	emitProgress(progress, ProgressEvent{
		Stage:   ProgressReceiving,
		Message: "Received " + filepath.Base(filename),
	})
	result, err := p.ProcessPath(ctx, path, progress)
	if err == nil {
		result.PackName = sanitizePackName(filepath.Base(filename))
	}
	return result, err
}

// sanitizePackName turns a user-supplied file name into a plain, display-safe
// pack name: the extension is dropped, Minecraft "§"-style formatting codes
// and any other punctuation/symbols are stripped, and only letters, digits,
// and single spaces survive (so "! §cDragonfruit §532x.zip" becomes
// "Dragonfruit 532x"). If stripping leaves nothing usable, "Texture pack" is
// used instead so the library never shows a blank name.
func sanitizePackName(name string) string {
	name = strings.TrimSuffix(name, filepath.Ext(name))

	var builder strings.Builder
	builder.Grow(len(name))
	lastWasSpace := false
	runes := []rune(name)
	for index := 0; index < len(runes); index++ {
		r := runes[index]
		switch {
		case r == '§':
			// Minecraft formatting code: the section sign plus the single
			// code character right after it (e.g. "§c", "§5") both go.
			if index+1 < len(runes) {
				index++
			}
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
			builder.WriteRune(r)
			lastWasSpace = false
		case r == ' ' || r == '_' || r == '-':
			if !lastWasSpace && builder.Len() > 0 {
				builder.WriteByte(' ')
				lastWasSpace = true
			}
		default:
			// Drop everything else: punctuation, emoji, and any other symbol.
		}
	}

	cleaned := strings.TrimSpace(builder.String())
	if cleaned == "" {
		return "Texture pack"
	}
	return cleaned
}

func texturePackID(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open texture pack for ID: %w", err)
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if copyErr != nil {
		return "", fmt.Errorf("hash texture pack: %w", copyErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("close texture pack after hashing: %w", closeErr)
	}
	return fmt.Sprintf("%x", hash.Sum(nil)[:8]), nil
}

func emitProgress(progress ProgressFunc, event ProgressEvent) {
	if progress != nil {
		progress(event)
	}
}
