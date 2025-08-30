package hijabin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
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
	httpc    *http.Client
	gemKeys  []string
	keyIndex int
	debug    bool
}

func New(cfg config.Config, _ *wa.Sender) *Handler {
	url := strings.TrimSpace(os.Getenv("HIJABIN_API_URL"))
	key := strings.TrimSpace(os.Getenv("HIJABIN_API_KEY"))

	// dukung banyak nama env kunci Gemini
	var gk []string
	for _, name := range []string{"GEMINI_IMG_KEYS", "GEMINI_KEYS", "GEMINI_API_KEY"} {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			gk = appendKeys(gk, v)
		}
	}
	if len(gk) == 0 && len(cfg.GeminiKeys) > 0 {
		gk = cfg.GeminiKeys
	}

	dbg := strings.EqualFold(strings.TrimSpace(os.Getenv("HIJABIN_DEBUG")), "true")

	return &Handler{
		apiURL:  url,
		apiKey:  key,
		re:      regexp.MustCompile(`(?i)\b(hijab(in|kan)?|kerudung(i|kan)?|berhijabkan)\b`),
		httpc:   &http.Client{Timeout: 120 * time.Second},
		gemKeys: gk,
		debug:   dbg,
	}
}

func (h *Handler) TryHandle(client *whatsmeow.Client, m *events.Message, text string, _ bool, _ *regexp.Regexp) bool {
	if !h.re.MatchString(text) {
		return false
	}

	// ambil gambar utama atau quoted
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

	// batasi tipe file seperti contoh Node (jpeg/png)
	mt := img.GetMimetype()
	if !regexp.MustCompile(`^image/(jpe?g|png)$`).MatchString(strings.ToLower(mt)) {
		replyText(context.Background(), client, m, "Format tidak didukung. Kirim/reply **jpeg/jpg/png** ya ✨")
		return true
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	blob, err := client.Download(ctx, img)
	if err != nil {
		replyText(ctx, client, m, "Gagal mengunduh gambar 😔")
		if h.debug {
			log.Printf("[HIJABIN] download error: %v", err)
		}
		return true
	}

	if h.debug {
		log.Printf("[HIJABIN] start | apiURL=%q hasGemini=%t mimetype=%s size=%d",
			h.apiURL, len(h.gemKeys) > 0, mt, len(blob))
	}

	// proses
	out, outMT, err := h.processHijab(ctx, blob, mt)
	if err != nil {
		if errors.Is(err, errNotConfigured) {
			replyText(ctx, client, m, "Fitur hijabin belum dikonfigurasi. Set **HIJABIN_API_URL** (dan KEY jika perlu) *atau* **GEMINI_API_KEY/GEMINI_KEYS** di `.env`.")
		} else {
			replyText(ctx, client, m, "Gagal memproses hijabin. Coba lagi ya ✨")
		}
		if h.debug {
			log.Printf("[HIJABIN] process error: %v", err)
		}
		return true
	}

	up, err := client.Upload(ctx, out, whatsmeow.MediaImage)
	if err != nil {
		replyText(ctx, client, m, "Upload gambar hasil gagal.")
		if h.debug {
			log.Printf("[HIJABIN] upload error: %v", err)
		}
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
			Mimetype:      pbf.String(outMT),
			Caption:       pbf.String("*Selamat, gambar sudah berhijab.*"),
			ContextInfo:   ci,
		},
	})
	if h.debug {
		log.Printf("[HIJABIN] success | outMT=%s outSize=%d", outMT, len(out))
	}
	return true
}

var errNotConfigured = errors.New("hijab service not configured")

func (h *Handler) processHijab(ctx context.Context, img []byte, mimeType string) ([]byte, string, error) {
	mt := mimeType
	if mt == "" {
		mt = "image/png"
	}

	// 1) API eksternal (jika ada)
	if h.apiURL != "" {
		if h.debug {
			log.Printf("[HIJABIN] trying external API (JSON)…")
		}
		if out, outMT, err := h.callAPI_JSON(ctx, img, mt); err == nil {
			if h.debug {
				log.Printf("[HIJABIN] external JSON OK | outMT=%s size=%d", outMT, len(out))
			}
			return out, outMT, nil
		} else if h.debug {
			log.Printf("[HIJABIN] external JSON fail: %v", err)
		}
		if h.debug {
			log.Printf("[HIJABIN] trying external API (multipart)…")
		}
		if out, outMT, err := h.callAPI_Multipart(ctx, img, mt); err == nil {
			if h.debug {
				log.Printf("[HIJABIN] external multipart OK | outMT=%s size=%d", outMT, len(out))
			}
			return out, outMT, nil
		} else if h.debug {
			log.Printf("[HIJABIN] external multipart fail: %v", err)
		}
	}

	// 2) Gemini image generation (meniru contoh Node.js)
	if len(h.gemKeys) > 0 {
		if h.debug {
			log.Printf("[HIJABIN] trying Gemini image generation…")
		}
		out, outMT, err := h.callGemini(ctx, img, mt,
			"Tambahkan hijab yang menutupi rambut, leher, dan dada dengan sempurna pada wanita dalam gambar ini. Hijab harus natural dan sesuai warna pakaian. Pertahankan ekspresi wajah asli, jangan ada rambut terlihat keluar.")
		if err == nil {
			if h.debug {
				log.Printf("[HIJABIN] Gemini OK | outMT=%s size=%d", outMT, len(out))
			}
			return out, outMT, nil
		}
		if h.debug {
			log.Printf("[HIJABIN] Gemini fail: %v", err)
		}
		return nil, "", err
	}

	return nil, "", errNotConfigured
}

// ---------- API eksternal: JSON ----------
func (h *Handler) callAPI_JSON(ctx context.Context, img []byte, mt string) ([]byte, string, error) {
	b64 := base64.StdEncoding.EncodeToString(img)
	body, _ := json.Marshal(map[string]any{
		"image": "data:" + mt + ";base64," + b64,
	})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.apiURL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if h.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+h.apiKey)
	}
	resp, err := h.httpc.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if h.debug {
		log.Printf("[HIJABIN] external JSON status=%d ct=%s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	return parseAPIResponse(resp, mt, h.debug)
}

// ---------- API eksternal: multipart/form-data ----------
func (h *Handler) callAPI_Multipart(ctx context.Context, img []byte, mt string) ([]byte, string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, _ := w.CreateFormFile("image", "source"+extByMime(mt))
	_, _ = io.Copy(fw, bytes.NewReader(img))
	_ = w.WriteField("mime", mt)
	_ = w.Close()

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.apiURL, &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	if h.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+h.apiKey)
	}
	resp, err := h.httpc.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if h.debug {
		log.Printf("[HIJABIN] external multipart status=%d ct=%s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	return parseAPIResponse(resp, mt, h.debug)
}

// ---------- Parser respons generik ----------
func parseAPIResponse(resp *http.Response, fallbackMT string, dbg bool) ([]byte, string, error) {
	rb, _ := io.ReadAll(resp.Body)
	ct := resp.Header.Get("Content-Type")

	if dbg {
		log.Printf("[HIJABIN] parseAPIResponse ct=%q bytes=%d", ct, len(rb))
		if len(rb) > 256 {
			log.Printf("[HIJABIN] body(head)=%q", string(rb[:256]))
		} else {
			log.Printf("[HIJABIN] body=%q", string(rb))
		}
	}

	// raw bytes image
	if resp.StatusCode >= 200 && resp.StatusCode < 300 && strings.HasPrefix(ct, "image/") {
		mt := ct
		if mt == "" {
			mt = fallbackMT
		}
		return rb, mt, nil
	}

	// JSON → coba cari data-url/base64/url
	var j map[string]any
	if json.Unmarshal(rb, &j) == nil {
		if s, ok := j["image"].(string); ok && s != "" {
			if strings.HasPrefix(s, "data:") {
				if dec, mt, ok := decodeDataURL(s); ok {
					return dec, mt, nil
				}
			}
			if dec, err := base64.StdEncoding.DecodeString(s); err == nil {
				mt := stringValue(j["mime"], fallbackMT)
				return dec, mt, nil
			}
		}
		if s, ok := j["image_base64"].(string); ok && s != "" {
			if dec, err := base64.StdEncoding.DecodeString(s); err == nil {
				mt := stringValue(j["mime"], fallbackMT)
				return dec, mt, nil
			}
		}
		if u, ok := j["url"].(string); ok && u != "" {
			if out, mt, err := httpGetBytes(u); err == nil {
				if mt == "" {
					mt = fallbackMT
				}
				return out, mt, nil
			}
		}
	}

	return nil, "", fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(rb)))
}

func stringValue(v any, def string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return def
}

func extByMime(mt string) string {
	exts, _ := mime.ExtensionsByType(mt)
	if len(exts) > 0 {
		return exts[0]
	}
	switch mt {
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	default:
		return ".jpg"
	}
}

func httpGetBytes(u string) ([]byte, string, error) {
	res, err := http.Get(u)
	if err != nil {
		return nil, "", err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return b, res.Header.Get("Content-Type"), nil
}

// ---------- Gemini image generation ----------
func (h *Handler) callGemini(ctx context.Context, img []byte, mt string, prompt string) ([]byte, string, error) {
	if len(h.gemKeys) == 0 {
		return nil, "", errors.New("no gemini keys")
	}
	models := []string{
		"gemini-2.0-flash-exp-image-generation",
		"gemini-2.0-flash-preview-image-generation",
	}
	var lastErr error
	for attempt := 0; attempt < len(h.gemKeys); attempt++ {
		key := h.gemKeys[h.keyIndex]
		for _, model := range models {
			ep := "https://generativelanguage.googleapis.com/v1beta/models/" + model + ":generateContent?key=" + key

			body := map[string]any{
				"contents": []any{
					map[string]any{
						"parts": []any{
							map[string]any{"text": prompt},
							map[string]any{
								"inline_data": map[string]any{
									"mime_type": mt,
									"data":      base64.StdEncoding.EncodeToString(img),
								},
							},
						},
					},
				},
				"generationConfig": map[string]any{
					"response_mime_type": "image/png",
					"temperature":        0.8,
				},
			}

			b, _ := json.Marshal(body)
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, ep, bytes.NewReader(b))
			req.Header.Set("Content-Type", "application/json")
			resp, err := h.httpc.Do(req)
			if err != nil {
				lastErr = err
				if h.debug {
					log.Printf("[HIJABIN] gemini req error: %v", err)
				}
				continue
			}
			rb, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if h.debug {
				log.Printf("[HIJABIN] gemini status=%d bytes=%d", resp.StatusCode, len(rb))
			}
			if resp.StatusCode >= 300 {
				lastErr = fmt.Errorf("gemini %d: %s", resp.StatusCode, strings.TrimSpace(string(rb)))
				continue
			}
			var jr map[string]any
			if json.Unmarshal(rb, &jr) == nil {
				if cands, ok := jr["candidates"].([]any); ok && len(cands) > 0 {
					if cand, _ := cands[0].(map[string]any); cand != nil {
						if content, ok := cand["content"].(map[string]any); ok {
							if parts, ok := content["parts"].([]any); ok && len(parts) > 0 {
								for _, p := range parts {
									if pm, _ := p.(map[string]any); pm != nil {
										if txt, _ := pm["text"].(string); txt != "" && h.debug {
											log.Printf("[HIJABIN] gemini text: %s", strings.TrimSpace(txt))
										}
										if inl, ok := pm["inline_data"].(map[string]any); ok {
											if dataStr, _ := inl["data"].(string); dataStr != "" {
												dec, err := base64.StdEncoding.DecodeString(dataStr)
												if err == nil {
													outMT := mt
													if mm, _ := inl["mime_type"].(string); mm != "" {
														outMT = mm
													}
													return dec, outMT, nil
												}
											}
										}
									}
								}
							}
						}
					}
				}
			}
			lastErr = errors.New("gemini: output tidak berisi inline_data")
		}
		// ganti key untuk percobaan berikutnya
		h.keyIndex = (h.keyIndex + 1) % len(h.gemKeys)
	}
	if lastErr == nil {
		lastErr = errors.New("gemini: gagal menghasilkan gambar")
	}
	return nil, "", lastErr
}

// ---------- util ----------
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

func appendKeys(dst []string, raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return dst
	}
	for _, p := range strings.Split(raw, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			dst = append(dst, p)
		}
	}
	return dst
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
