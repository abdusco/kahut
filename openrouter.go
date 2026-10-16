package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	defaultOpenRouterBase = "https://openrouter.ai/api/v1"
	maxAIResponseBytes    = 16 << 20
)

var (
	errAIUpstream   = errors.New("image generation failed")
	errAIRateLimit  = errors.New("image generation is rate limited or out of credit")
	errAINoImage    = errors.New("the model returned no image")
	errAIImageLarge = errors.New("generated image is too large")
)

// openRouter generates images through OpenRouter's chat completions API.
// The key and model come from the server environment and never reach clients.
type openRouter struct {
	key    string
	model  string
	base   string
	client *http.Client
}

// newOpenRouter returns nil when the key or model is missing, which turns the
// feature off.
func newOpenRouter() *openRouter {
	key := strings.TrimSpace(os.Getenv("OPENROUTER_API_KEY"))
	model := strings.TrimSpace(os.Getenv("OPENROUTER_MODEL"))
	if key == "" || model == "" {
		return nil
	}
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("OPENROUTER_BASE_URL")), "/")
	if base == "" {
		base = defaultOpenRouterBase
	}
	return &openRouter{key: key, model: model, base: base, client: &http.Client{Timeout: 90 * time.Second}}
}

type imagesResponse struct {
	Data []struct {
		B64 string `json:"b64_json"`
	} `json:"data"`
}

// Generate returns the bytes of the first image the model produced. It uses
// the /images endpoint, which image-only models require.
func (o *openRouter) Generate(ctx context.Context, prompt string) ([]byte, error) {
	// A fresh seed per call so regenerating gives a different image. Some
	// models reject the parameter, so a 400 is retried without it; those
	// models vary between calls anyway.
	res, err := o.post(ctx, map[string]any{"model": o.model, "prompt": prompt, "seed": rand.Int32()})
	if err == nil && res.StatusCode == http.StatusBadRequest {
		res.Body.Close()
		res, err = o.post(ctx, map[string]any{"model": o.model, "prompt": prompt})
	}
	if err != nil {
		return nil, errAIUpstream
	}
	defer res.Body.Close()
	switch {
	case res.StatusCode == http.StatusTooManyRequests || res.StatusCode == http.StatusPaymentRequired:
		return nil, errAIRateLimit
	case res.StatusCode != http.StatusOK:
		// The upstream message goes to the server log only, never to clients.
		var failure struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&failure)
		return nil, fmt.Errorf("%w: upstream status %d: %.300s", errAIUpstream, res.StatusCode, failure.Error.Message)
	}
	var out imagesResponse
	if err := json.NewDecoder(io.LimitReader(res.Body, maxAIResponseBytes)).Decode(&out); err != nil {
		return nil, errAIUpstream
	}
	if len(out.Data) == 0 || out.Data[0].B64 == "" {
		return nil, errAINoImage
	}
	encoded := out.Data[0].B64
	if base64.StdEncoding.DecodedLen(len(encoded)) > maxImageBytes+3 {
		return nil, errAIImageLarge
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(data) == 0 {
		return nil, errAINoImage
	}
	if len(data) > maxImageBytes {
		return nil, errAIImageLarge
	}
	return normalizeImage(data), nil
}

func (o *openRouter) post(ctx context.Context, payload map[string]any) (*http.Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.base+"/images", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+o.key)
	req.Header.Set("Content-Type", "application/json")
	return o.client.Do(req)
}
