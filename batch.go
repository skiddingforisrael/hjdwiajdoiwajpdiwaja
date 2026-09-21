package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/qaustria/AutoPack-Go/utils"
)

const (
	defaultBatchEndpoint        = "http://127.0.0.1:8080/api/convert"
	defaultBatchStatePath       = "data/batch-queue.json"
	defaultBatchOutputDir       = "batch-output"
	maxBatchSheetBytes    int64 = 10 << 20
	maxBatchLandingBytes        = 4 << 20
	maxBatchQueueEntries        = 2000
)

var batchURLPattern = regexp.MustCompile(`https?://[^\s"'<>]+`)

type batchQueueEntry struct {
	Row    int
	Name   string
	Source string
}

type batchQueueState struct {
	Version   int                            `json:"version"`
	Completed map[string]batchCompletedEntry `json:"completed"`
}

type batchCompletedEntry struct {
	Name       string    `json:"name"`
	PackID     string    `json:"packId,omitempty"`
	OutputPath string    `json:"outputPath"`
	FinishedAt time.Time `json:"finishedAt"`
}

type batchAPIEvent struct {
	Type     string          `json:"type"`
	Progress *ProgressEvent  `json:"progress,omitempty"`
	Result   json.RawMessage `json:"result,omitempty"`
	PackID   string          `json:"packId,omitempty"`
	Message  string          `json:"message,omitempty"`
}

// runBatch queues every pack URL in a CSV or public Google Sheet and submits
// one pack at a time to Cone's existing local web API. The server remains the
// single source of truth for processing, history, and Discord notifications.
func runBatch(ctx context.Context, args []string) error {
	if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
		return errors.New("usage: cone batch <public Google Sheet URL or CSV path>")
	}
	credentials, err := loadRobloxCredentials()
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 30 * time.Minute}
	queue, err := loadBatchQueue(ctx, args[0], client)
	if err != nil {
		return err
	}
	if len(queue) == 0 {
		return errors.New("batch source contains no HTTP texture-pack links")
	}

	statePath := strings.TrimSpace(os.Getenv("CONE_BATCH_STATE_PATH"))
	if statePath == "" {
		statePath = defaultBatchStatePath
	}
	outputDir := strings.TrimSpace(os.Getenv("CONE_BATCH_OUTPUT_DIR"))
	if outputDir == "" {
		outputDir = defaultBatchOutputDir
	}
	endpoint := strings.TrimSpace(os.Getenv("CONE_BATCH_ENDPOINT"))
	if endpoint == "" {
		endpoint = defaultBatchEndpoint
	}
	if err := validateBatchEndpoint(endpoint); err != nil {
		return err
	}
	batchToken := strings.TrimSpace(os.Getenv("CONE_BATCH_TOKEN"))
	if len(batchToken) < 32 || len(batchToken) > 256 {
		return errors.New("CONE_BATCH_TOKEN must contain between 32 and 256 characters")
	}
	if err := os.MkdirAll(outputDir, 0o750); err != nil {
		return fmt.Errorf("create batch output directory: %w", err)
	}
	state, err := loadBatchState(statePath)
	if err != nil {
		return err
	}

	completed := 0
	failed := 0
	skipped := 0
	fmt.Printf("Cone batch: %d queued packs\n", len(queue))
	for index, entry := range queue {
		if _, done := state.Completed[entry.Source]; done {
			skipped++
			fmt.Printf("[%d/%d] SKIP %s (already completed)\n", index+1, len(queue), entry.Name)
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		fmt.Printf("[%d/%d] DOWNLOAD %s\n", index+1, len(queue), entry.Name)
		zipPath, filename, err := downloadBatchPack(ctx, client, entry)
		if err != nil {
			failed++
			fmt.Printf("[%d/%d] FAILED %s: %v\n", index+1, len(queue), entry.Name, err)
			continue
		}
		expectedPackID, err := texturePackID(zipPath)
		if err != nil {
			_ = os.Remove(zipPath)
			failed++
			fmt.Printf("[%d/%d] FAILED %s: %v\n", index+1, len(queue), entry.Name, err)
			continue
		}
		result, err := submitBatchPackWhenAvailable(ctx, client, endpoint, credentials, batchToken, zipPath, filename, index+1, len(queue), func(progress ProgressEvent) {
			message := progress.Message
			if message == "" {
				message = progress.Name
			}
			if progress.Error != "" {
				if message == "" {
					message = "Skipped asset"
				}
				message += ": " + progress.Error
			}
			if message != "" {
				fmt.Printf("[%d/%d] %s: %s\n", index+1, len(queue), progress.Stage, message)
			}
		})
		_ = os.Remove(zipPath)
		if err != nil {
			if batchAuthenticationFailed(err) {
				return fmt.Errorf(
					"batch paused at [%d/%d] %s because Roblox rejected the credentials: %w; replace the API key or use an unmoderated owner, then rerun the same command to resume",
					index+1, len(queue), entry.Name, err,
				)
			}
			failed++
			fmt.Printf("[%d/%d] FAILED %s: %v\n", index+1, len(queue), entry.Name, err)
			continue
		}
		if result.PackID == "" {
			// Cone 1.4.5 and earlier did not include the Pack ID in the final
			// stream event. It is the first eight bytes of the ZIP SHA-256, so
			// the batch client can recover it without changing the result.
			result.PackID = expectedPackID
		} else if result.PackID != expectedPackID {
			return fmt.Errorf("Cone returned Pack ID %s for ZIP %s", result.PackID, expectedPackID)
		}
		outputPath := filepath.Join(outputDir, batchOutputFilename(filename, result.PackID))
		if err := os.WriteFile(outputPath, append(result.Result, '\n'), 0o640); err != nil {
			return fmt.Errorf("write batch output for %q: %w", entry.Name, err)
		}
		state.Completed[entry.Source] = batchCompletedEntry{
			Name: entry.Name, PackID: result.PackID, OutputPath: outputPath, FinishedAt: time.Now().UTC(),
		}
		if err := saveBatchState(statePath, state); err != nil {
			return err
		}
		completed++
		fmt.Printf("[%d/%d] DONE %s (Pack ID %s)\n", index+1, len(queue), entry.Name, result.PackID)
	}
	fmt.Printf("Cone batch finished: %d completed, %d skipped, %d failed\n", completed, skipped, failed)
	if failed != 0 {
		return fmt.Errorf("%d batch packs failed; rerun the same command to retry only unfinished rows", failed)
	}
	return nil
}

type batchSubmitResult struct {
	PackID string
	Result json.RawMessage
}

type batchServerError struct {
	StatusCode int
	Status     string
	Message    string
	RetryAfter time.Duration
}

func (err *batchServerError) Error() string {
	if err.Message == "" {
		return "Cone returned " + err.Status
	}
	return "Cone returned " + err.Status + ": " + err.Message
}

// Authentication failures affect every remaining pack, so continuing would
// only hammer Roblox and hide the real problem behind hundreds of failures.
// The completed checkpoint stays intact and the same command can resume once
// working credentials are supplied.
func batchAuthenticationFailed(err error) bool {
	var serverError *batchServerError
	if !errors.As(err, &serverError) {
		return false
	}
	return serverError.StatusCode == http.StatusUnauthorized || serverError.StatusCode == http.StatusForbidden
}

func submitBatchPackWhenAvailable(ctx context.Context, client *http.Client, endpoint string, credentials robloxCredentials, batchToken, zipPath, filename string, batchIndex, batchTotal int, progress ProgressFunc) (batchSubmitResult, error) {
	deadline := time.Now().Add(30 * time.Minute)
	for {
		result, err := submitBatchPack(ctx, client, endpoint, credentials, batchToken, zipPath, filename, batchIndex, batchTotal, progress)
		var serverError *batchServerError
		if !errors.As(err, &serverError) || serverError.StatusCode != http.StatusTooManyRequests {
			return result, err
		}
		if time.Now().After(deadline) {
			return batchSubmitResult{}, fmt.Errorf("wait for Cone queue slot: %w", err)
		}
		delay := serverError.RetryAfter
		if delay <= 0 || delay > time.Minute {
			delay = 5 * time.Second
		}
		fmt.Printf("Cone is busy; retrying this pack in %s\n", delay)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return batchSubmitResult{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func submitBatchPack(ctx context.Context, client *http.Client, endpoint string, credentials robloxCredentials, batchToken, zipPath, filename string, batchIndex, batchTotal int, progress ProgressFunc) (batchSubmitResult, error) {
	file, err := os.Open(zipPath)
	if err != nil {
		return batchSubmitResult{}, fmt.Errorf("open queued pack: %w", err)
	}
	defer file.Close()

	requestContext, cancel := context.WithCancel(ctx)
	defer cancel()
	reader, writer := io.Pipe()
	defer reader.Close()
	multipartWriter := multipart.NewWriter(writer)
	writeDone := make(chan error, 1)
	go func() {
		part, createErr := multipartWriter.CreateFormFile("pack", filename)
		if createErr == nil {
			_, createErr = io.Copy(part, file)
		}
		if closeErr := multipartWriter.Close(); createErr == nil {
			createErr = closeErr
		}
		_ = writer.CloseWithError(createErr)
		writeDone <- createErr
	}()

	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, endpoint, reader)
	if err != nil {
		_ = reader.CloseWithError(err)
		return batchSubmitResult{}, fmt.Errorf("create queued port request: %w", err)
	}
	request.Header.Set("Content-Type", multipartWriter.FormDataContentType())
	request.Header.Set(robloxAPIKeyHeader, credentials.APIKey)
	if credentials.UserID == "" {
		return batchSubmitResult{}, errors.New("batch mode currently requires a Roblox user ID")
	}
	request.Header.Set(robloxUserIDHeader, credentials.UserID)
	request.Header.Set(batchIndexHeader, strconv.Itoa(batchIndex))
	request.Header.Set(batchTotalHeader, strconv.Itoa(batchTotal))
	request.Header.Set(batchTokenHeader, batchToken)
	response, err := client.Do(request)
	if err != nil {
		_ = reader.CloseWithError(err)
		return batchSubmitResult{}, fmt.Errorf("submit queued pack: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		_ = reader.CloseWithError(errors.New("Cone rejected queued pack"))
		return batchSubmitResult{}, &batchServerError{
			StatusCode: response.StatusCode,
			Status:     response.Status,
			Message:    strings.TrimSpace(string(body)),
			RetryAfter: parseBatchRetryAfter(response.Header.Get("Retry-After")),
		}
	}

	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 64<<10), 2<<20)
	var result batchSubmitResult
	for scanner.Scan() {
		var event batchAPIEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			_ = reader.CloseWithError(err)
			return batchSubmitResult{}, fmt.Errorf("decode Cone batch event: %w", err)
		}
		if event.Progress != nil && progress != nil {
			progress(*event.Progress)
		}
		switch event.Type {
		case "error":
			_ = reader.CloseWithError(errors.New(event.Message))
			return batchSubmitResult{}, errors.New(event.Message)
		case "result":
			if !json.Valid(event.Result) {
				return batchSubmitResult{}, errors.New("Cone returned invalid result JSON")
			}
			result = batchSubmitResult{PackID: event.PackID, Result: append(json.RawMessage(nil), event.Result...)}
		}
	}
	if err := scanner.Err(); err != nil {
		_ = reader.CloseWithError(err)
		return batchSubmitResult{}, fmt.Errorf("read Cone batch events: %w", err)
	}
	if err := <-writeDone; err != nil {
		return batchSubmitResult{}, fmt.Errorf("stream queued pack: %w", err)
	}
	if len(result.Result) == 0 {
		return batchSubmitResult{}, errors.New("Cone finished without a batch result")
	}
	return result, nil
}

func parseBatchRetryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if timestamp, err := http.ParseTime(value); err == nil {
		if delay := time.Until(timestamp); delay > 0 {
			return delay
		}
	}
	return 0
}

func loadBatchQueue(ctx context.Context, source string, client *http.Client) ([]batchQueueEntry, error) {
	reader, closeReader, err := openBatchCSV(ctx, strings.TrimSpace(source), client)
	if err != nil {
		return nil, err
	}
	defer closeReader()
	data, err := io.ReadAll(io.LimitReader(reader, maxBatchSheetBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read batch CSV: %w", err)
	}
	if int64(len(data)) > maxBatchSheetBytes {
		return nil, fmt.Errorf("batch CSV exceeds the %d-byte limit", maxBatchSheetBytes)
	}
	records, err := csv.NewReader(bytes.NewReader(data)).ReadAll()
	if err != nil {
		return nil, fmt.Errorf("read batch CSV: %w", err)
	}
	return expandBatchQueue(ctx, client, parseBatchRecords(records)), nil
}

func openBatchCSV(ctx context.Context, source string, client *http.Client) (io.Reader, func(), error) {
	if sheetURL, ok := googleSheetCSVURL(source); ok {
		response, err := batchGET(ctx, client, sheetURL)
		if err != nil {
			if strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), "403") {
				return nil, func() {}, fmt.Errorf("read Google Sheet: %w; share it as Anyone with the link → Viewer", err)
			}
			return nil, func() {}, fmt.Errorf("read Google Sheet: %w", err)
		}
		return io.LimitReader(response.Body, maxBatchSheetBytes+1), func() { _ = response.Body.Close() }, nil
	}
	if parsed, err := url.Parse(source); err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") {
		response, err := batchGET(ctx, client, source)
		if err != nil {
			return nil, func() {}, fmt.Errorf("download batch CSV: %w", err)
		}
		return io.LimitReader(response.Body, maxBatchSheetBytes+1), func() { _ = response.Body.Close() }, nil
	}
	file, err := os.Open(source)
	if err != nil {
		return nil, func() {}, fmt.Errorf("open batch CSV %q: %w", source, err)
	}
	return io.LimitReader(file, maxBatchSheetBytes+1), func() { _ = file.Close() }, nil
}

func googleSheetCSVURL(source string) (string, bool) {
	parsed, err := url.Parse(source)
	if err != nil || !strings.EqualFold(parsed.Hostname(), "docs.google.com") {
		return "", false
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) < 3 || parts[0] != "spreadsheets" || parts[1] != "d" || parts[2] == "" {
		return "", false
	}
	gid := parsed.Query().Get("gid")
	if gid == "" {
		if fragment, err := url.ParseQuery(parsed.Fragment); err == nil {
			gid = fragment.Get("gid")
		}
	}
	if gid == "" {
		gid = "0"
	}
	return "https://docs.google.com/spreadsheets/d/" + url.PathEscape(parts[2]) + "/gviz/tq?tqx=out:csv&gid=" + url.QueryEscape(gid), true
}

func expandBatchQueue(ctx context.Context, client *http.Client, queue []batchQueueEntry) []batchQueueEntry {
	expanded := make([]batchQueueEntry, 0, len(queue))
	for _, entry := range queue {
		folderKey, isFolder := mediaFireFolderKey(entry.Source)
		if !isFolder {
			expanded = append(expanded, entry)
			continue
		}
		fmt.Printf("EXPAND %s (MediaFire folder)\n", entry.Name)
		files, err := listMediaFireFolderFiles(ctx, client, folderKey, entry.Row)
		if err != nil {
			fmt.Printf("SKIP %s: %v\n", entry.Name, err)
			continue
		}
		fmt.Printf("EXPANDED %s: %d ZIP files\n", entry.Name, len(files))
		expanded = append(expanded, files...)
		if len(expanded) >= maxBatchQueueEntries {
			fmt.Printf("Queue reached its %d-pack safety limit; remaining links were ignored\n", maxBatchQueueEntries)
			break
		}
	}

	seen := make(map[string]struct{}, len(expanded))
	deduplicated := expanded[:0]
	for _, entry := range expanded {
		identity := batchSourceIdentity(entry.Source)
		if _, exists := seen[identity]; exists {
			continue
		}
		seen[identity] = struct{}{}
		deduplicated = append(deduplicated, entry)
		if len(deduplicated) == maxBatchQueueEntries {
			break
		}
	}
	return deduplicated
}

type mediaFireContentResponse struct {
	Response struct {
		Result        string `json:"result"`
		Message       string `json:"message"`
		FolderContent struct {
			MoreChunks string `json:"more_chunks"`
			Files      []struct {
				Filename string `json:"filename"`
				MIMEType string `json:"mimetype"`
				Links    struct {
					NormalDownload string `json:"normal_download"`
				} `json:"links"`
			} `json:"files"`
			Folders []struct {
				FolderKey string `json:"folderkey"`
			} `json:"folders"`
		} `json:"folder_content"`
	} `json:"response"`
}

func listMediaFireFolderFiles(ctx context.Context, client *http.Client, rootKey string, row int) ([]batchQueueEntry, error) {
	folders := []string{rootKey}
	seenFolders := make(map[string]struct{})
	var files []batchQueueEntry
	for len(folders) != 0 {
		folderKey := folders[0]
		folders = folders[1:]
		if _, exists := seenFolders[folderKey]; exists {
			continue
		}
		seenFolders[folderKey] = struct{}{}
		if len(seenFolders) > 500 {
			return nil, errors.New("MediaFire folder tree exceeds the 500-folder safety limit")
		}

		for chunk := 1; ; chunk++ {
			content, err := fetchMediaFireFolderContent(ctx, client, folderKey, "files", chunk)
			if err != nil {
				return nil, err
			}
			for _, file := range content.Response.FolderContent.Files {
				if !strings.EqualFold(filepath.Ext(file.Filename), ".zip") || file.Links.NormalDownload == "" {
					continue
				}
				files = append(files, batchQueueEntry{Row: row, Name: file.Filename, Source: file.Links.NormalDownload})
				if len(files) >= maxBatchQueueEntries {
					return files, nil
				}
			}
			if !strings.EqualFold(content.Response.FolderContent.MoreChunks, "yes") {
				break
			}
		}

		for chunk := 1; ; chunk++ {
			content, err := fetchMediaFireFolderContent(ctx, client, folderKey, "folders", chunk)
			if err != nil {
				return nil, err
			}
			for _, folder := range content.Response.FolderContent.Folders {
				if folder.FolderKey != "" {
					folders = append(folders, folder.FolderKey)
				}
			}
			if !strings.EqualFold(content.Response.FolderContent.MoreChunks, "yes") {
				break
			}
		}
	}
	return files, nil
}

func fetchMediaFireFolderContent(ctx context.Context, client *http.Client, folderKey, contentType string, chunk int) (mediaFireContentResponse, error) {
	query := url.Values{
		"folder_key":      {folderKey},
		"content_type":    {contentType},
		"response_format": {"json"},
		"chunk":           {strconv.Itoa(chunk)},
	}
	target := "https://www.mediafire.com/api/1.5/folder/get_content.php?" + query.Encode()
	response, err := batchGET(ctx, client, target)
	if err != nil {
		return mediaFireContentResponse{}, fmt.Errorf("list MediaFire folder %s: %w", folderKey, err)
	}
	defer response.Body.Close()
	var content mediaFireContentResponse
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxBatchSheetBytes+1))
	if err := decoder.Decode(&content); err != nil {
		return content, fmt.Errorf("decode MediaFire folder %s: %w", folderKey, err)
	}
	if !strings.EqualFold(content.Response.Result, "Success") {
		message := strings.TrimSpace(content.Response.Message)
		if message == "" {
			message = "unknown MediaFire API error"
		}
		return content, fmt.Errorf("list MediaFire folder %s: %s", folderKey, message)
	}
	return content, nil
}

func mediaFireFolderKey(source string) (string, bool) {
	parsed, err := url.Parse(source)
	if err != nil || (!strings.EqualFold(parsed.Hostname(), "www.mediafire.com") && !strings.EqualFold(parsed.Hostname(), "mediafire.com")) {
		return "", false
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) < 2 || parts[0] != "folder" || parts[1] == "" {
		return "", false
	}
	return parts[1], true
}

func batchSourceIdentity(source string) string {
	parsed, err := url.Parse(source)
	if err != nil {
		return source
	}
	host := strings.ToLower(parsed.Hostname())
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if (host == "www.mediafire.com" || host == "mediafire.com") && len(parts) >= 3 && parts[0] == "file" {
		return "mediafire:file:" + parts[1]
	}
	parsed.Fragment = ""
	return parsed.String()
}

func parseBatchRecords(records [][]string) []batchQueueEntry {
	if len(records) == 0 {
		return nil
	}
	nameColumn := -1
	for column, value := range records[0] {
		header := strings.ToLower(strings.TrimSpace(value))
		if header == "name" || header == "pack name" || header == "texture pack" || header == "texturepack" {
			nameColumn = column
			break
		}
	}
	seen := make(map[string]struct{})
	var queue []batchQueueEntry
	for rowIndex, row := range records {
		name := ""
		if nameColumn >= 0 && nameColumn < len(row) {
			name = strings.TrimSpace(row[nameColumn])
		}
		for _, cell := range row {
			for _, match := range batchURLPattern.FindAllString(cell, -1) {
				match = strings.TrimRight(html.UnescapeString(match), ").,;]}")
				parsed, err := url.Parse(match)
				if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
					continue
				}
				parsed.Fragment = ""
				match = parsed.String()
				if _, exists := seen[match]; exists {
					continue
				}
				seen[match] = struct{}{}
				entryName := name
				if entryName == "" {
					entryName = batchNameFromURL(parsed, rowIndex+1)
				}
				queue = append(queue, batchQueueEntry{Row: rowIndex + 1, Name: entryName, Source: match})
			}
		}
	}
	return queue
}

func downloadBatchPack(ctx context.Context, client *http.Client, entry batchQueueEntry) (string, string, error) {
	source := normalizeBatchDownloadURL(entry.Source)
	response, err := openBatchDownload(ctx, client, source)
	if err != nil {
		return "", "", err
	}
	defer response.Body.Close()
	filename := batchDownloadFilename(response, entry)
	file, err := os.CreateTemp("", "cone-batch-*.zip")
	if err != nil {
		return "", "", fmt.Errorf("create queued ZIP: %w", err)
	}
	path := file.Name()
	reader := &io.LimitedReader{R: response.Body, N: DefaultMaxTexturePackUploadBytes + 1}
	written, copyErr := io.Copy(file, reader)
	closeErr := file.Close()
	if copyErr != nil {
		_ = os.Remove(path)
		return "", "", fmt.Errorf("download queued ZIP: %w", copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(path)
		return "", "", fmt.Errorf("close queued ZIP: %w", closeErr)
	}
	if written == 0 || written > DefaultMaxTexturePackUploadBytes {
		_ = os.Remove(path)
		return "", "", fmt.Errorf("queued ZIP size %d is outside the 1-%d byte limit", written, DefaultMaxTexturePackUploadBytes)
	}
	archive, err := os.Open(path)
	if err != nil {
		_ = os.Remove(path)
		return "", "", err
	}
	var signature [4]byte
	_, signatureErr := io.ReadFull(archive, signature[:])
	_ = archive.Close()
	if signatureErr != nil || (string(signature[:]) != "PK\x03\x04" && string(signature[:]) != "PK\x05\x06" && string(signature[:]) != "PK\x07\x08") {
		_ = os.Remove(path)
		return "", "", errors.New("downloaded file is not a ZIP; the sheet may contain a landing-page link")
	}
	return path, filename, nil
}

func openBatchDownload(ctx context.Context, client *http.Client, source string) (*http.Response, error) {
	response, err := batchGET(ctx, client, source)
	if err != nil {
		return nil, err
	}
	contentType := strings.ToLower(response.Header.Get("Content-Type"))
	parsed, _ := url.Parse(source)
	isMediaFire := parsed != nil && strings.HasSuffix(strings.ToLower(parsed.Hostname()), "mediafire.com")
	if !isMediaFire || !strings.Contains(contentType, "text/html") {
		return response, nil
	}
	page, readErr := io.ReadAll(io.LimitReader(response.Body, maxBatchLandingBytes+1))
	_ = response.Body.Close()
	if readErr != nil {
		return nil, fmt.Errorf("read MediaFire download page: %w", readErr)
	}
	if len(page) > maxBatchLandingBytes {
		return nil, errors.New("MediaFire download page exceeds the safety limit")
	}
	directURL := mediaFireDirectDownloadURL(page)
	if directURL == "" {
		return nil, errors.New("MediaFire page does not contain a working direct-download link")
	}
	return batchGET(ctx, client, directURL)
}

func mediaFireDirectDownloadURL(page []byte) string {
	for _, match := range batchURLPattern.FindAllString(string(page), -1) {
		match = html.UnescapeString(strings.TrimRight(match, "\\).,;]}"))
		parsed, err := url.Parse(match)
		if err != nil {
			continue
		}
		host := strings.ToLower(parsed.Hostname())
		if strings.HasPrefix(host, "download") && strings.HasSuffix(host, ".mediafire.com") {
			return parsed.String()
		}
	}
	return ""
}

func normalizeBatchDownloadURL(source string) string {
	parsed, err := url.Parse(source)
	if err != nil {
		return source
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "www.dropbox.com" || host == "dropbox.com" {
		query := parsed.Query()
		query.Set("dl", "1")
		parsed.RawQuery = query.Encode()
	}
	if host == "drive.google.com" {
		parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
		fileID := parsed.Query().Get("id")
		if len(parts) >= 3 && parts[0] == "file" && parts[1] == "d" {
			fileID = parts[2]
		}
		if fileID != "" {
			return "https://drive.usercontent.google.com/download?id=" + url.QueryEscape(fileID) + "&export=download&confirm=t"
		}
	}
	return parsed.String()
}

func batchGET(ctx context.Context, client *http.Client, target string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", "Cone/"+utils.Version+" batch porter")
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_ = response.Body.Close()
		return nil, fmt.Errorf("%s returned %s", request.URL.Hostname(), response.Status)
	}
	return response, nil
}

func validateBatchEndpoint(endpoint string) error {
	parsed, err := url.Parse(endpoint)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return errors.New("CONE_BATCH_ENDPOINT must be an HTTP or HTTPS URL")
	}
	return nil
}

func loadBatchState(path string) (batchQueueState, error) {
	state := batchQueueState{Version: 1, Completed: make(map[string]batchCompletedEntry)}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return state, nil
	}
	if err != nil {
		return state, fmt.Errorf("read batch state: %w", err)
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, fmt.Errorf("decode batch state: %w", err)
	}
	if state.Completed == nil {
		state.Completed = make(map[string]batchCompletedEntry)
	}
	return state, nil
}

func saveBatchState(path string, state batchQueueState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create batch state directory: %w", err)
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode batch state: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".cone-batch-state-*")
	if err != nil {
		return fmt.Errorf("create batch state: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(append(data, '\n')); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write batch state: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close batch state: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace batch state: %w", err)
	}
	return nil
}

func batchNameFromURL(parsed *url.URL, row int) string {
	name := filepath.Base(strings.TrimSuffix(parsed.Path, "/"))
	if decoded, err := url.PathUnescape(name); err == nil {
		name = decoded
	}
	if name == "" || name == "." || name == "/" {
		name = fmt.Sprintf("pack-%03d.zip", row)
	}
	return name
}

func batchDownloadFilename(response *http.Response, entry batchQueueEntry) string {
	filename := ""
	if disposition := response.Header.Get("Content-Disposition"); disposition != "" {
		if _, parameters, err := mime.ParseMediaType(disposition); err == nil {
			filename = parameters["filename"]
		}
	}
	if filename == "" {
		filename = entry.Name
	}
	filename = filepath.Base(strings.ReplaceAll(filename, "\\", "/"))
	if !strings.EqualFold(filepath.Ext(filename), ".zip") {
		filename += ".zip"
	}
	return filename
}

func batchOutputFilename(filename, packID string) string {
	stem := strings.TrimSuffix(filepath.Base(filename), filepath.Ext(filename))
	stem = strings.Map(func(character rune) rune {
		if character == ' ' || character == '-' || character == '_' ||
			character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' {
			return character
		}
		return '_'
	}, stem)
	stem = strings.Trim(strings.Join(strings.Fields(stem), "_"), "._-")
	if stem == "" {
		stem = "pack"
	}
	if packID == "" {
		hash := sha256.Sum256([]byte(filename))
		packID = hex.EncodeToString(hash[:8])
	}
	return stem + "_" + packID + "_cone.json"
}
