package main

import (
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/klauspost/compress/zstd"
)

//go:embed default_config.json
var defaultConfigJSON []byte

var customRotations = map[string][]int{
	"GoldAppleRotation": {0, -90, 0},
	"ShearsRotation":    {0, 0, -35},
}

type compressedJSONEnvelope struct {
	Metadata any    `json:"m"`
	Type     string `json:"t"`
	ZBase64  string `json:"zbase64"`
}

func defaultPipelineValues() (map[string]any, error) {
	var values map[string]any
	if err := json.Unmarshal(defaultConfigJSON, &values); err != nil {
		return nil, fmt.Errorf("decode embedded default config: %w", err)
	}
	for field, rotation := range customRotations {
		values[field] = append([]int(nil), rotation...)
	}
	return values, nil
}

func encodeCompressedJSON(values map[string]any) ([]byte, error) {
	if values == nil {
		values = map[string]any{}
	}
	raw, err := json.Marshal(values)
	if err != nil {
		return nil, fmt.Errorf("encode raw port JSON: %w", err)
	}
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		return nil, fmt.Errorf("create zstd encoder: %w", err)
	}
	compressed := encoder.EncodeAll(raw, nil)
	if closeErr := encoder.Close(); closeErr != nil {
		return nil, fmt.Errorf("close zstd encoder: %w", closeErr)
	}
	return json.Marshal(compressedJSONEnvelope{
		Metadata: nil,
		Type:     "buffer",
		ZBase64:  base64.StdEncoding.EncodeToString(compressed),
	})
}

func decodeCompressedJSON(data []byte) (map[string]any, error) {
	var envelope compressedJSONEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, fmt.Errorf("decode compressed JSON envelope: %w", err)
	}
	if envelope.Metadata != nil || envelope.Type != "buffer" || envelope.ZBase64 == "" {
		return nil, errors.New("invalid compressed JSON envelope")
	}
	compressed, err := base64.StdEncoding.DecodeString(envelope.ZBase64)
	if err != nil {
		return nil, fmt.Errorf("decode zbase64: %w", err)
	}
	decoder, err := zstd.NewReader(nil)
	if err != nil {
		return nil, fmt.Errorf("create zstd decoder: %w", err)
	}
	raw, err := decoder.DecodeAll(compressed, nil)
	decoder.Close()
	if err != nil {
		return nil, fmt.Errorf("decompress port JSON: %w", err)
	}
	var values map[string]any
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, fmt.Errorf("decode decompressed port JSON: %w", err)
	}
	return values, nil
}
