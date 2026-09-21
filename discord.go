package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	discordComponentTextLimit = 4000
	discordComponentsV2Flag   = 1 << 15
	discordAccentOrange       = 16742144
)

type PortNotification struct {
	PackID     string
	PackName   string
	OutputJSON []byte
	PreviewPNG []byte
	BatchIndex int
	BatchTotal int
}

type PortNotifier interface {
	Notify(context.Context, PortNotification) error
}

type DiscordNotifier struct {
	webhookURL string
	client     *http.Client
}

func NewDiscordNotifier(webhookURL string) (*DiscordNotifier, error) {
	webhookURL = strings.TrimSpace(webhookURL)
	if webhookURL == "" {
		return nil, errors.New("Discord webhook URL is empty")
	}
	parsed, err := url.Parse(webhookURL)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
		return nil, errors.New("Discord webhook URL is invalid")
	}
	query := parsed.Query()
	query.Set("with_components", "true")
	parsed.RawQuery = query.Encode()
	return &DiscordNotifier{
		webhookURL: parsed.String(),
		client:     &http.Client{Timeout: 15 * time.Second},
	}, nil
}

func (notifier *DiscordNotifier) Notify(ctx context.Context, notification PortNotification) error {
	if notifier == nil {
		return errors.New("Discord notifier is nil")
	}
	if ctx == nil {
		return errors.New("Discord notification context is nil")
	}
	if notification.PackID == "" || len(notification.OutputJSON) == 0 || len(notification.PreviewPNG) == 0 {
		return errors.New("Discord notification is incomplete")
	}
	// Discord components have a hard 4000-character limit, but the stored/
	// served OutputJSON is now plain flat JSON (not compressed) so people
	// can read and edit it directly - a real pack's plain JSON can easily
	// exceed that limit. Discord's message is the only place that still
	// needs a compact form, so compress just this one embed on the fly
	// rather than compressing the actual output artifact everyone else uses.
	var values map[string]any
	if err := json.Unmarshal(notification.OutputJSON, &values); err != nil {
		return fmt.Errorf("decode pack JSON for Discord: %w", err)
	}
	compactJSON, err := encodeCompressedJSON(values)
	if err != nil {
		return fmt.Errorf("compress pack JSON for Discord: %w", err)
	}
	outputComponent := fmt.Sprintf(":tickets: **Pack ID**:\n`%s`\n:package: **Output JSON**: ```%s```", notification.PackID, compactJSON)
	if len(outputComponent) > discordComponentTextLimit {
		return fmt.Errorf("compressed pack JSON is too large for one Discord component (%d characters)", len(outputComponent))
	}
	previewPNG, err := addPackIDToPreview(notification.PreviewPNG, notification.PackID)
	if err != nil {
		return fmt.Errorf("label Discord preview: %w", err)
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	previewFilename := "cone-hotbar-" + notification.PackID + ".png"
	heading := "## :orange_circle: Cone Website Logs \n**Texturepack ported successfully.**\n"
	if notification.BatchIndex > 0 && notification.BatchTotal >= notification.BatchIndex {
		heading = fmt.Sprintf(
			"## :orange_circle: Cone Batch Logs\n**BATCHING [%d/%d]**\n**Texturepack ported successfully.**\n",
			notification.BatchIndex, notification.BatchTotal,
		)
	}
	payload, err := json.Marshal(map[string]any{
		"flags": discordComponentsV2Flag,
		"allowed_mentions": map[string]any{
			"parse": []string{},
		},
		"attachments": []any{map[string]any{
			"id": 0, "filename": previewFilename, "description": "Cone texture-pack hotbar preview",
		}},
		"components": []any{map[string]any{
			"type": 17,
			"components": []any{
				map[string]any{"type": 10, "content": heading},
				map[string]any{"type": 14, "spacing": 1},
				map[string]any{"type": 10, "content": outputComponent},
				map[string]any{"type": 14},
				map[string]any{"type": 10, "content": ":frame_photo: **Texturepack Preview:**"},
				map[string]any{"type": 12, "items": []any{map[string]any{
					"media": map[string]any{"url": "attachment://" + previewFilename},
				}}},
			},
			"accent_color": discordAccentOrange,
		}},
	})
	if err != nil {
		return fmt.Errorf("encode Discord webhook payload: %w", err)
	}
	payloadPart, err := writer.CreateFormField("payload_json")
	if err != nil {
		return fmt.Errorf("create Discord payload field: %w", err)
	}
	if _, err := payloadPart.Write(payload); err != nil {
		return fmt.Errorf("write Discord payload: %w", err)
	}
	previewPart, err := writer.CreateFormFile("files[0]", previewFilename)
	if err != nil {
		return fmt.Errorf("create Discord preview attachment: %w", err)
	}
	if _, err := previewPart.Write(previewPNG); err != nil {
		return fmt.Errorf("write Discord preview attachment: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("finish Discord webhook body: %w", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, notifier.webhookURL, &body)
	if err != nil {
		return fmt.Errorf("create Discord webhook request: %w", err)
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response, err := notifier.client.Do(request)
	if err != nil {
		return fmt.Errorf("send Discord webhook: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 8<<10))
		return fmt.Errorf("Discord webhook returned %s: %s", response.Status, strings.TrimSpace(string(message)))
	}
	return nil
}
