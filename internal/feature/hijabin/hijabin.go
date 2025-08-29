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
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types/events"
	pbf "google.golang.org/protobuf/proto"

	"wa-elaina/internal/config"
	"wa-elaina/internal/wa" // dipakai di New signature agar konsisten dengan router
)

type Handler struct {
	apiURL   string
	apiKey   string
	re       *regexp.Regexp
	client   *http.Client
	gemKeys  []string
	keyIndex int
}

func New(cfg config.Config, _ *wa.Sender) *Handler {
	url := os.Getenv("HIJABIN_API_URL")
	key := os.Getenv("HIJABIN_API_KEY")
	// ambil keys untuk Gemini: prefer GEMINI_IMG_KEYS, fallback GEMINI_KEYS, lalu cfg.GeminiKeys
	gk := keysFromCSV(os.Getenv("GEMINI_IMG_KEYS"))
	if len(gk) == 0 {
		gk = keysFromCSV(os.Getenv("GEMINI_KEYS"))
	}
	if len(gk) == 0 && len(cfg.GeminiKeys) > 0 {
		gk = cfg.GeminiKeys
	}
	return &Handler{
		apiURL:  strings.TrimSpace(url),
		apiKey:  strings.TrimSpace(key),
		re:      regexp.MustCompile(`(?i)\b(hijabin|kerudungi|berhijabkan)\b`),
		client:  &http.Client{Timeout: 60 * time.Second},
		gemKeys: gk,
	}
}

func (h *Handler) TryHandle(client *whatsmeow.Client, m *events.Message, text string, _ bool, reTrigger *regexp.Regexp) bool {
	// wajib ada kata kunci hijab atau trigger "elaina hijabin"
	if !h.re.MatchString(text) && !reTrigger.MatchString(text) {
		return false
	}

	// ambil gambar dari pesan atau quoted
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

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	blob, err := client.Download(ctx, img)
	if err != nil {
		replyText(ctx, client, m, "Gagal mengunduh gambar 😔")
		return true
	}

	out, mimeType, err := h.hijab(ctx, blob, img.GetMimetype())
	if err != nil {
		if h.apiURL == "" {
			replyText(ctx, client, m, "Gagal hijabin via Gemini. Pastikan GEMINI_IMG_KEYS / GEMINI_KEYS terpasang.")
		} else {
			replyText(ctx, client, m, "Gagal memproses hijabin. Coba lagi ya ✨")
		}
		return true
	}

	// upload & kirim sebagai reply
	up, err := client.Upload(ctx, out, whatsmeow.MediaImage)
	if err != nil {
		replyText(ctx, client, m, "Upload gambar hasil gagal.")
		return true
	}
	ci := &waProto.ContextInfo{
		StanzaID:      pbf.String(m.Info.ID),
		QuotedMessage: m.Message,
		Participant:   pbf.String(m.Info.Sender.String()),
		RemoteJID:     pbf.String(m.Info.Chat.String()),
	}
	_, _ = client.SendMessage(ctx, m.Info.Chat, &waProto.Message{
		ImageMessage: &waProto.ImageMessage{
			URL:           pbf.String(up.URL),
			DirectPath:    pbf.String(up.DirectPath),
			MediaKey:      up.MediaKey,
			FileEncSHA256: up.FileEncSHA256,
			FileSHA256:    up.FileSHA256,
			FileLength:    pbf.Uint64(uint64(len(out))),
			Mimetype:      pbf.String(mimeType),
			Caption:       pbf.String("Done~"),
			ContextInfo:   ci,
		},
	})
	return true
}

// --- Core proses hijab ---
// Jika HIJABIN_API_URL ada -> pakai API itu.
// Jika tidak -> fallback Gemini 2.x image generation.
func (h *Handler) hijab(ctx context.Context, img []byte, mimeType string) ([]byte, string, error) {
	mt := mimeType
	if mt == "" {
		mt = "image/jpeg"
	}

	if h.apiURL != "" {
		// Mode API eksternal
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

		// Terima format dataURL / base64 / raw bytes
		var out struct {
			Image string `json:"image"`
			Mime  string `json:"mime"`
		}
		if json.Unmarshal(rb, &out) == nil && out.Image != "" {
			data := out.Image
			if strings.HasPrefix(data, "data:") {
				if dec, newMT, ok := decodeDataURL(data); ok {
					if out.Mime != "" {
						newMT = out.Mime
					}
					return dec, newMT, nil
				}
			}
			if dec, err := base64.StdEncoding.DecodeString(data); err == nil {
				if out.Mime != "" {
					mt = out.Mime
				}
				return dec, mt, nil
			}
		}
		// fallback: treat as raw bytes
		return rb, mt, nil
	}

	// Fallback: Gemini
	prompt := "Tambahkan hijab/kerudung yang sopan dan natural. Jangan ubah identitas wajah. Hasil realistis."
	out, err := generateHijabGemini(ctx, h.client, h.gemKeys, &h.keyIndex, mt, img, prompt)
	if err != nil {
		return nil, "", err
	}
	return out, "image/jpeg", nil
}

// ---- Gemini 2.x inline ----
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
		return nil, errors.New("no Gemini API keys configured")
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

			// parse image from parts (inlineData / inline_data)
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

// ---- utils ----
func decodeDataURL(dataURL string) ([]byte, string, bool) {
	// data:[<mediatype>][;base64],<data>
	comma := strings.Index(dataURL, ",")
	if comma <= 0 {
		return nil, "", false
	}
	head, body := dataURL[:comma], dataURL[comma+1:]
	ct := "application/octet-stream"
	if mt, _, err := mime.ParseMediaType(strings.TrimPrefix(head, "data:")); err == nil {
		ct = mt
	}
	dec, err := base64.StdEncoding.DecodeString(body)
	if err != nil {
		return nil, "", false
	}
	return dec, ct, true
}

func keysFromCSV(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	ps := strings.Split(raw, ",")
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func rotateIndex(idx *int, n int) {
	if n <= 1 {
		return
	}
	*idx = (*idx + 1) % n
}

func replyText(ctx context.Context, client *whatsmeow.Client, m *events.Message, msg string) {
	ci := &waProto.ContextInfo{
		StanzaID:      pbf.String(m.Info.ID),
		QuotedMessage: m.Message,
		Participant:   pbf.String(m.Info.Sender.String()),
		RemoteJID:     pbf.String(m.Info.Chat.String()),
	}
	_, _ = client.SendMessage(ctx, m.Info.Chat, &waProto.Message{
		ExtendedTextMessage: &waProto.ExtendedTextMessage{
			Text:        pbf.String(msg),
			ContextInfo: ci,
		},
	})
}
