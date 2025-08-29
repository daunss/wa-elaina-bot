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

	"wa-elaina/internal/ai"
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
	return &Handler{
		apiURL:  strings.TrimSpace(url),
		apiKey:  strings.TrimSpace(key),
		send:    s,
		re:      regexp.MustCompile(`(?i)\b(hijabin|kerudungi|berhijabkan)\b`),
		client:  &http.Client{Timeout: 60 * time.Second},
		gemKeys: cfg.GeminiKeys,
	}
}

func (h *Handler) TryHandle(client *whatsmeow.Client, m *events.Message, text string, _ bool, reTrigger *regexp.Regexp) bool {
	// Wajib ada kata kunci hijab ATAU trigger (mis. "elaina hijabin")
	if !h.re.MatchString(text) && !reTrigger.MatchString(text) {
		return false
	}
	// ambil gambar dari pesan ini atau quoted
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
			_ = h.send.Text(wa.DestJID(m.Info.Chat), "Gagal hijabin via Gemini. Pastikan GEMINI_API_KEYS terpasang ya.")
		} else {
			_ = h.send.Text(wa.DestJID(m.Info.Chat), "Gagal memproses hijabin. Coba lagi ya ✨")
		}
		return true
	}
	_ = h.send.Image(wa.DestJID(m.Info.Chat), out, mimeType, "Done~")
	return true
}

// Jika HIJABIN_API_URL ada -> pakai API generic (kompat lama).
// Jika kosong -> fallback Gemini image-generation.
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

	// Fallback: Gemini
	prompt := "Ubah gambar agar berhijab syar'i rapi & sopan; jangan ubah identitas wajah; hasil natural."
	imgOut, err := ai.GenerateHijabImage(ctx, h.client, h.gemKeys, &h.keyIndex, mt, img, prompt)
	if err != nil {
		return nil, "", err
	}
	return imgOut, "image/jpeg", nil
}
