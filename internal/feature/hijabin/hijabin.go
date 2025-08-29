package hijabin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types/events"

	"wa-elaina/internal/config"
	"wa-elaina/internal/wa"
)

type Handler struct {
	apiURL   string
	apiKey   string
	send     *wa.Sender
	re       *regexp.Regexp
	client   *http.Client
	gemKeys  []string
	keyIndex int
}

func New(cfg config.Config, s *wa.Sender) *Handler {
	url := os.Getenv("HIJABIN_API_URL")
	key := os.Getenv("HIJABIN_API_KEY")
	// Gemini keys bisa dari ENV GEMINI_IMG_KEYS (comma-separated) atau dari cfg.GeminiKeys
	gk := keysFromEnv("GEMINI_IMG_KEYS")
	if len(gk) == 0 && len(cfg.GeminiKeys) > 0 {
		gk = cfg.GeminiKeys
	}
	return &Handler{
		apiURL:  strings.TrimSpace(url),
		apiKey:  strings.TrimSpace(key),
		send:    s,
		re:      regexp.MustCompile(`(?i)\b(hijabin|kerudungi|berhijabkan)\b`),
		client:  &http.Client{Timeout: 60 * time.Second},
		gemKeys: gk,
	}
}

func (h *Handler) TryHandle(client *whatsmeow.Client, m *events.Message, text string, _ bool, reTrigger *regexp.Regexp) bool {
	// Wajib ada kata kunci hijab ATAU trigger (mis. "elaina hijabin")
	if !h.re.MatchString(text) && !reTrigger.MatchString(text) {
		return false
	}
	// Ambil gambar dari pesan ini atau quoted
	img := m.Message.GetImageMessage()
	if img == nil {
		if xt := m.Message.GetExtendedTextMessage(); xt != nil && xt.ContextInfo != nil {
			if qm := xt.GetContextInfo().GetQuotedMessage(); qm != nil {
				img = qm.GetImageMessage()
			}
		}
	}
	if img == nil {
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	blob, err := client.Download(ctx, img)
	if err != nil {
		_ = h.send.Text(wa.DestJID(m.Info.Chat), "Gagal mengunduh gambar 😔")
		return true
	}

	out, mimeType, err := h.hijab(ctx, blob, img.GetMimetype())
	if err != nil {
		if h.apiURL == "" {
			_ = h.send.Text(wa.DestJID(m.Info.Chat), "Gagal hijabin via Gemini. Pastikan GEMINI_IMG_KEYS/GEMINI_KEYS terpasang ya.")
		} else {
			_ = h.send.Text(wa.DestJID(m.Info.Chat), "Gagal memproses hijabin. Coba lagi ya ✨")
		}
		return true
	}
	_ = h.send.Image(wa.DestJID(m.Info.Chat), out, mimeType, "Done~")
	return true
}

// Jika HIJABIN_API_URL ada -> pakai API generic (kompat lama).
// Jika kosong -> fallback Gemini image-generation (inline di file ini).
func (h *Handler) hijab(ctx context.Context, img []byte, mimeType string) ([]byte, string, error) {
	mt := mimeType
	if mt == "" {
		mt = "image/jpeg"
	}

	if h.apiURL != "" {
		b64 := base64.StdEncoding.EncodeToString(img)
		payload := map[string]any{
			"image": "data:" + mt + ";base64," + b64,
		}
		b, _ := json.Marshal(payload)

		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.apiURL, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		if h.apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+h.apiKey)
		}

		resp, err := h.client.Do(req)
		if err != nil {
			return nil, "", err
		}
		defer resp.Body.Close()
		rb, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= 300 {
			return nil, "", errors.New(string(rb))
		}

		// Try parse
		var out struct {
			Image string `json:"image"`
			Mime  string `json:"mime"`
		}
		if json.Unmarshal(rb, &out) == nil && out.Image != "" {
			data := out.Image
			if strings.HasPrefix(data, "data:") {
				comma := strings.Index(data, ",")
				if comma > 0 {
					head := data[:comma]
					if ct, _, err := mime.ParseMediaType(strings.TrimPrefix(head, "data:")); err == nil {
						mt = ct
					}
					dec, err := base64.StdEncoding.DecodeString(data[comma+1:])
					if err == nil {
						return dec, mt, nil
					}
				}
			}
			dec, err := base64.StdEncoding.DecodeString(data)
			if err == nil {
				if out.Mime != "" {
					mt = out.Mime
				}
				return dec, mt, nil
			}
		}
		// fallback: raw bytes
		return rb, mt, nil
	}

	// Fallback: Gemini (inline)
	prompt := "Ubah gambar agar berhijab syar'i rapi & sopan; jangan ubah identitas wajah; hasil natural."
	imgOut, err := generateHijabGemini(ctx, h.client, h.gemKeys, &h.keyIndex, mt, img, prompt)
	if err != nil {
		return nil, "", err
	}
	return imgOut, "image/jpeg", nil
}

// ---- Gemini image-generation inline (tanpa paket eksternal) ----

func generateHijabGemini(
	ctx context.Context,
	httpClient *http.Client,
	keys []string,
	keyIndex *int,
	mime string,
	imageBytes []byte,
	prompt string,
) ([]byte, error) {
	if len(keys) == 0 {
		// fallback ke GEMINI_KEYS jika IMG_KEYS kosong
		keys = keysFromEnv("GEMINI_KEYS")
		if len(keys) == 0 {
			return nil, errors.New("no Gemini API keys configured")
		}
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 60 * time.Second}
	}
	if mime == "" {
		mime = "image/jpeg"
	}

	models := []string{
		"gemini-2.0-flash-preview-image-generation",
		"gemini-2.0-flash-exp-image-generation",
	}

	var lastErr error

	for attempt := 0; attempt < len(keys); attempt++ {
		key := keys[*keyIndex]

		for _, model := range models {
			url := "https://generativelanguage.googleapis.com/v1beta/models/" + model + ":generateContent?key=" + key

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
				continue
			}
			rb, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			bodyLower := strings.ToLower(string(rb))
			if resp.StatusCode == 429 || strings.Contains(bodyLower, "resource_exhausted") {
				lastErr = errors.New("rate limited")
				rotateIndex(keyIndex, len(keys))
				break
			}
			if resp.StatusCode == 400 && strings.Contains(bodyLower, "response modalities") {
				lastErr = errors.New("modalities unsupported on model " + model)
				continue
			}
			if resp.StatusCode >= 300 {
				lastErr = errors.New(resp.Status + ": " + string(rb))
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
				lastErr = errors.New("no candidates/parts in response")
				continue
			}
			for _, p := range out.Candidates[0].Content.Parts {
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
			lastErr = errors.New("no image part in response for model " + model)
		}
		rotateIndex(keyIndex, len(keys))
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, errors.New("image-gen failed")
}

func rotateIndex(idx *int, n int) {
	if n <= 1 {
		return
	}
	*idx = (*idx + 1) % n
}

func keysFromEnv(name string) []string {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
