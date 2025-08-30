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
	"wa-elaina/internal/wa"
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

	// Gemini key list (optional fallback)
	var gk []string
	if v := strings.TrimSpace(os.Getenv("GEMINI_IMG_KEYS")); v != "" {
		gk = keysFromCSV(v)
	} else if v := strings.TrimSpace(os.Getenv("GEMINI_KEYS")); v != "" {
		gk = keysFromCSV(v)
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

// HANYA aktif bila ada kata kunci hijab (tidak terpicu hanya karena "elaina")
func (h *Handler) TryHandle(client *whatsmeow.Client, m *events.Message, text string, _ bool, _ *regexp.Regexp) bool {
	if !h.re.MatchString(text) {
		return false
	}

	// Gambar dari pesan atau dari quoted
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
		replyText(ctx, client, m, "Gagal memproses hijabin. Coba lagi ya ✨")
		return true
	}

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

func (h *Handler) hijab(ctx context.Context, img []byte, mimeType string) ([]byte, string, error) {
	mt := mimeType
	if mt == "" {
		mt = "image/jpeg"
	}

	// 1) API eksternal (jika tersedia)
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
		return rb, mt, nil
	}

	// 2) Fallback Gemini (stub – silakan isi sesuai implementasi Anda)
	return generateHijabGemini(ctx, h.client, h.gemKeys, &h.keyIndex, mt, img,
		"Tambahkan hijab/kerudung yang sopan dan natural. Jangan ubah identitas wajah. Hasil realistis.")
}

// ---------- utilities ----------

func decodeDataURL(dataURL string) ([]byte, string, bool) {
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

// replyText helper (lokal agar tidak bergantung file lain)
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

// Stub fallback Gemini: ganti implementasi ini jika ingin benar-benar memakai Gemini
func generateHijabGemini(_ context.Context, _ *http.Client, _ []string, _ *int, _ string, _ []byte, _ string) ([]byte, string, error) {
	return nil, "", errors.New("gemini fallback not configured")
}
