package tts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
)

type Handler struct {
	enabled bool

	// ElevenLabs
	elKey   string
	elVoice string
	elModel string

	reCmd  *regexp.Regexp
	reTrig *regexp.Regexp // dari router
	httpc  *http.Client
}

func New(cfg config.Config, reTrigger *regexp.Regexp) *Handler {
	key := os.Getenv("ELEVEN_API_KEY")
	if key == "" {
		key = os.Getenv("ELEVENLABS_API_KEY")
	}
	voice := os.Getenv("ELEVEN_VOICE_ID")
	model := os.Getenv("ELEVEN_MODEL_ID")
	if model == "" {
		model = "eleven_monolingual_v1"
	}
	return &Handler{
		enabled: key != "" && voice != "",
		elKey:   key,
		elVoice: voice,
		elModel: model,
		reCmd:   regexp.MustCompile(`(?i)\b(vn|voice\s*note|kirim\s*vn|kirimkan\s*vn|ucapkan|bacakan|katakan)\b`),
		reTrig:  reTrigger,
		httpc:   &http.Client{Timeout: 60 * time.Second},
	}
}

// TryHandle mengirim VN jika user minta:
// - "elaina vn <teks>" / "elaina kirim(vn) ..." / "elaina bacakan ..."
// - reply teks lalu ketik "elaina vn"
func (h *Handler) TryHandle(client *whatsmeow.Client, m *events.Message, userText string) bool {
	// kalau user menyinggung VN tapi belum di-set env → beri panduan & hentikan
	if !h.reTrig.MatchString(userText) {
		return false
	}
	after := strings.TrimSpace(h.reTrig.ReplaceAllString(userText, ""))
	if after == "" && m.Message.GetExtendedTextMessage() == nil {
		return false
	}
	wantTTS := h.reCmd.MatchString(after)
	if !wantTTS {
		return false
	}
	if !h.enabled {
		h.replyText(context.Background(), client, m,
			"Fitur VN belum dikonfigurasi. Set **ELEVEN_API_KEY** dan **ELEVEN_VOICE_ID** di `.env`, lalu restart bot ✨")
		return true
	}

	// quoted text (kalau user reply)
	var quotedText string
	if xt := m.Message.GetExtendedTextMessage(); xt != nil && xt.ContextInfo != nil {
		if qm := xt.GetContextInfo().GetQuotedMessage(); qm != nil {
			if t := qm.GetConversation(); t != "" {
				quotedText = t
			} else if et := qm.GetExtendedTextMessage(); et != nil {
				quotedText = et.GetText()
			}
		}
	}

	// bersihkan frasa perintah VN agar yang dibacakan hanya isi
	speak := strings.TrimSpace(h.reCmd.ReplaceAllString(after, ""))
	if speak == "" && quotedText != "" {
		speak = quotedText
	}
	if speak == "" {
		h.replyText(context.Background(), client, m, "Tulis: *elaina vn <teks>* atau reply pesan lalu ketik *elaina vn* ya ✨")
		return true
	}

	ttsCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	audio, mimeType, err := h.elevenLabsTTS(ttsCtx, speak)
	if err != nil {
		h.replyText(context.Background(), client, m, "TTS gagal. Pastikan kredensial ElevenLabs benar.")
		return true
	}

	upCtx, upCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer upCancel()
	up, err := client.Upload(upCtx, audio, whatsmeow.MediaAudio)
	if err != nil {
		h.replyText(upCtx, client, m, "Gagal mengunggah audio 😔")
		return true
	}
	ci := &waProto.ContextInfo{
		StanzaID:      pbf.String(m.Info.ID),
		QuotedMessage: m.Message,
		Participant:   pbf.String(m.Info.Sender.String()),
		RemoteJID:     pbf.String(m.Info.Chat.String()),
	}
	_, _ = client.SendMessage(upCtx, m.Info.Chat, &waProto.Message{
		AudioMessage: &waProto.AudioMessage{
			URL:           pbf.String(up.URL),
			DirectPath:    pbf.String(up.DirectPath),
			MediaKey:      up.MediaKey,
			FileEncSHA256: up.FileEncSHA256,
			FileSHA256:    up.FileSHA256,
			FileLength:    pbf.Uint64(uint64(len(audio))),
			Mimetype:      pbf.String(mimeType), // "audio/mpeg"
			// Note: set Ptt=true jika pakai OGG/Opus.
			ContextInfo: ci,
		},
	})
	return true
}

// --- ElevenLabs ---
func (h *Handler) elevenLabsTTS(ctx context.Context, text string) ([]byte, string, error) {
	if h.elKey == "" || h.elVoice == "" {
		return nil, "", errors.New("elevenlabs not configured")
	}
	ep := fmt.Sprintf("https://api.elevenlabs.io/v1/text-to-speech/%s", h.elVoice)
	payload := map[string]any{
		"text":     text,
		"model_id": h.elModel,
	}
	b, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, ep, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "audio/mpeg")
	req.Header.Set("xi-api-key", h.elKey)
	res, err := h.httpc.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		rb, _ := io.ReadAll(res.Body)
		return nil, "", fmt.Errorf("elevenlabs %d: %s", res.StatusCode, strings.TrimSpace(string(rb)))
	}
	audio, _ := io.ReadAll(res.Body)
	return audio, "audio/mpeg", nil
}

// ---- reply helper ----
func (h *Handler) replyText(ctx context.Context, client *whatsmeow.Client, m *events.Message, msg string) {
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
