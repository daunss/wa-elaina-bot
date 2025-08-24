package ai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// GenerateHijabImage memanggil Gemini image-generation (2.x) via REST.
// keys: daftar API key (untuk rotasi saat limit), keyIndex: index aktif (diputar bila perlu).
func GenerateHijabImage(
	ctx context.Context,
	httpClient *http.Client,
	keys []string,
	keyIndex *int,
	mime string,
	imageBytes []byte,
	prompt string,
) ([]byte, error) {
	if len(keys) == 0 {
		return nil, fmt.Errorf("no GEMINI_IMG_KEYS configured")
	}
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	if mime == "" {
		mime = "image/jpeg"
	}

	// Urutan model dicoba: recommended -> fallback
	models := []string{
		"gemini-2.0-flash-preview-image-generation",
		"gemini-2.0-flash-exp-image-generation",
	}

	var lastErr error

	// Coba sebanyak jumlah key (rotasi jika diperlukan)
	for attempt := 0; attempt < len(keys); attempt++ {
		key := keys[*keyIndex]

		// Coba semua model pada key saat ini
		for _, model := range models {
			url := "https://generativelanguage.googleapis.com/v1beta/models/" + model + ":generateContent?key=" + key

			// PENTING: TEXT + IMAGE modalities
			reqBody := map[string]any{
				"generationConfig": map[string]any{
					"responseModalities": []string{"TEXT", "IMAGE"},
				},
				"contents": []map[string]any{
					{
						"role": "user",
						"parts": []map[string]any{
							{"text": prompt},
							{
								"inline_data": map[string]any{
									"mime_type": mime,
									"data":      base64.StdEncoding.EncodeToString(imageBytes),
								},
							},
						},
					},
				},
			}

			b, _ := json.Marshal(reqBody)
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
			req.Header.Set("Content-Type", "application/json")

			resp, err := httpClient.Do(req)
			if err != nil {
				lastErr = err
				continue // coba model berikutnya
			}
			rb, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			// 429 / resource_exhausted -> rotate key & pindah attempt berikutnya
			if resp.StatusCode == 429 || strings.Contains(strings.ToLower(string(rb)), "resource_exhausted") {
				lastErr = fmt.Errorf("rate limited: %s", resp.Status)
				rotateIndex(keyIndex, len(keys))
				break
			}
			// INVALID_ARGUMENT karena modalities/model -> coba model lain
			if resp.StatusCode == 400 && strings.Contains(strings.ToLower(string(rb)), "response modalities") {
				lastErr = fmt.Errorf("bad modalities for %s: %s", model, string(rb))
				continue
			}
			// Error lain >= 300 -> coba model lain
			if resp.StatusCode >= 300 {
				lastErr = fmt.Errorf("gemini image-gen %s: %s", resp.Status, string(rb))
				continue
			}

			// Parse gambar dari parts (inlineData / inline_data)
			var out struct {
				Candidates []struct {
					Content struct {
						Parts []map[string]any `json:"parts"`
					} `json:"content"`
				} `json:"candidates"`
			}
			if err := json.Unmarshal(rb, &out); err != nil {
				lastErr = err
				continue
			}
			if len(out.Candidates) == 0 || len(out.Candidates[0].Content.Parts) == 0 {
				lastErr = fmt.Errorf("no candidates/parts in response")
				continue
			}

			for _, p := range out.Candidates[0].Content.Parts {
				// dukung camelCase & snake_case
				if id, ok := p["inlineData"].(map[string]any); ok {
					if data, ok := id["data"].(string); ok && data != "" {
						return base64.StdEncoding.DecodeString(data)
					}
				}
				if id, ok := p["inline_data"].(map[string]any); ok {
					if data, ok := id["data"].(string); ok && data != "" {
						return base64.StdEncoding.DecodeString(data)
					}
				}
			}

			lastErr = fmt.Errorf("no image part in response (model=%s)", model)
		}

		// setelah semua model pada key ini dicoba -> rotate key
		rotateIndex(keyIndex, len(keys))
	}

	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("image-gen failed")
}

func rotateIndex(idx *int, n int) {
	if n <= 1 {
		return
	}
	*idx = (*idx + 1) % n
}
