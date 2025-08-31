package tts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types/events"
	pbf "google.golang.org/protobuf/proto"

	"wa-elaina/internal/config"
	"wa-elaina/internal/llm"
)

// Handler mengubah intent user -> naskah singkat (Gemini) -> audio (ElevenLabs)
type Handler struct {
	enabled bool

	// ElevenLabs
	elKey        string
	elVoice      string
	elModel      string
	outFmt       string // mp3_44100_192, mp3_44100_128, dll
	optLatency   int    // 0..4 (0 = kualitas terbaik)
	stability    float64
	similarity   float64
	style        float64
	speakerBoost bool
	rateHint     string // slow|normal|fast (mempengaruhi “rasa” bacaan via tanda baca)

	maxWords int // VN_MAX_WORDS

	reCmd  *regexp.Regexp
	reTrig *regexp.Regexp // di-inject dari router
	httpc  *http.Client

	verifyVoice bool // GET /v1/voices/{id} saat inisialisasi (opsional, via env)
}

func New(cfg config.Config, reTrigger *regexp.Regexp) *Handler {
	key := getenvFirst("ELEVEN_API_KEY", "ELEVENLABS_API_KEY")
	voice := getenvFirst("ELEVEN_VOICE_ID", "ELEVENLABS_VOICE_ID")

	// Default model yang umum dipakai sekarang
	model := strings.TrimSpace(os.Getenv("ELEVEN_MODEL_ID"))
	if model == "" {
		model = "eleven_multilingual_v2"
	}

	// Kualitas & latensi (latensi 0 = kualitas paling baik)
	outFmt := strings.TrimSpace(os.Getenv("ELEVEN_OUTPUT_FORMAT"))
	if outFmt == "" {
		outFmt = "mp3_44100_192"
	}
	optLatency := intFromEnv("ELEVEN_OPTIMIZE_LATENCY", 0) // 0..4

	h := &Handler{
		enabled:      key != "" && voice != "",
		elKey:        key,
		elVoice:      voice,
		elModel:      model,
		outFmt:       outFmt,
		optLatency:   clamp(optLatency, 0, 4),
		reCmd:        regexp.MustCompile(`(?i)\b(vn|voice\s*note|kirim(?:kan)?\s*vn|ucapkan|bacakan|katakan)\b`),
		reTrig:       reTrigger,
		httpc:        &http.Client{Timeout: 60 * time.Second},
		maxWords:     intFromEnv("VN_MAX_WORDS", 80),
		stability:    floatFromEnv("ELEVEN_STABILITY", 0.45), // sedikit lebih dinamis
		similarity:   floatFromEnv("ELEVEN_SIMILARITY", 0.90), // dekat ke karakter suara
		style:        floatFromEnv("ELEVEN_STYLE", 0.50),      // ekspresif sedang
		speakerBoost: boolFromEnv("ELEVEN_SPEAKER_BOOST", true),
		rateHint:     strings.ToLower(strings.TrimSpace(os.Getenv("ELEVEN_RATE_HINT"))),
		verifyVoice:  boolFromEnv("ELEVEN_VERIFY_VOICE", false),
	}

	if h.rateHint == "" {
		h.rateHint = "normal"
	}

	// Logging konfigurasi biar kelihatan saat start
	log.Printf("[TTS] Enabled=%v voiceID=%s model=%s fmt=%s latency=%d", h.enabled, h.elVoice, h.elModel, h.outFmt, h.optLatency)

	// Opsional: verifikasi voice id (sekali saat start)
	if h.enabled && h.verifyVoice {
		go func() {
			if name, err := h.getVoiceName(context.Background()); err != nil {
				log.Printf("[TTS] WARN verify voice: %v", err)
			} else {
				log.Printf("[TTS] Using voice: %s (%s)", name, h.elVoice)
			}
		}()
	}

	return h
}

// TryHandle: user minta VN → (1) buat naskah singkat (Gemini via llm.AskText),
// (2) TTS ElevenLabs, (3) kirim audio sebagai reply.
func (h *Handler) TryHandle(client *whatsmeow.Client, m *events.Message, userText string) bool {
	// wajib ada trigger
	if !h.reTrig.MatchString(userText) {
		return false
	}
	after := strings.TrimSpace(h.reTrig.ReplaceAllString(userText, ""))
	if after == "" && m.Message.GetExtendedTextMessage() == nil {
		return false
	}
	if !h.reCmd.MatchString(after) {
		return false
	}
	if !h.enabled {
		h.replyText(context.Background(), client, m,
			"Fitur VN belum dikonfigurasi. Set **ELEVEN_API_KEY/ELEVENLABS_API_KEY** dan **ELEVEN_VOICE_ID/ELEVENLABS_VOICE_ID** di `.env`, lalu restart bot ✨")
		return true
	}

	// --- Ambil maksud user ---
	intent := strings.TrimSpace(h.reCmd.ReplaceAllString(after, ""))
	if intent == "" {
		if xt := m.Message.GetExtendedTextMessage(); xt != nil && xt.ContextInfo != nil {
			if qm := xt.GetContextInfo().GetQuotedMessage(); qm != nil {
				if t := qm.GetConversation(); t != "" {
					intent = t
				} else if et := qm.GetExtendedTextMessage(); et != nil {
					intent = et.GetText()
				}
			}
		}
	}
	if intent == "" {
		h.replyText(context.Background(), client, m, "Tulis: *elaina vn <teks>* atau reply pesan lalu ketik *elaina vn* ya ✨")
		return true
	}

	// --- Buat naskah singkat via Gemini ---
	sys := fmt.Sprintf(`Kamu copywriter ramah untuk voice note WhatsApp.
Tulis SATU kalimat (maks %d kata), alami, hangat, jelas, tidak bertele-tele, langsung ke inti.
Jangan menyebut kata "voice note".`, h.maxWords)
	script := strings.TrimSpace(llm.AskText(sys, intent))
	if script == "" {
		script = intent
	}
	script = trimWords(script, h.maxWords)
	script = applyRateHint(script, h.rateHint)

	log.Printf("[TTS] script(%s): %q", h.rateHint, script)

	// --- TTS ElevenLabs ---
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	audio, mimeType, err := h.elevenLabsTTS(ctx, script)
	if err != nil {
		h.replyText(context.Background(), client, m, "TTS gagal. Pastikan kredensial ElevenLabs & voice ID benar.")
		log.Printf("[TTS] ERROR elevenLabsTTS: %v", err)
		return true
	}

	// --- Upload & kirim ---
	upCtx, upCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer upCancel()
	up, err := client.Upload(upCtx, audio, whatsmeow.MediaAudio)
	if err != nil {
		h.replyText(upCtx, client, m, "Gagal mengunggah audio 😔")
		log.Printf("[TTS] ERROR upload: %v", err)
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
			ContextInfo:   ci,
		},
	})
	return true
}

// ------------------- ElevenLabs -------------------

func (h *Handler) elevenLabsTTS(ctx context.Context, text string) ([]byte, string, error) {
	if h.elKey == "" || h.elVoice == "" {
		return nil, "", errors.New("elevenlabs not configured")
	}

	// Parameter kualitas & latensi melalui query
	ep := fmt.Sprintf(
		"https://api.elevenlabs.io/v1/text-to-speech/%s?output_format=%s&optimize_streaming_latency=%d",
		h.elVoice, h.outFmt, h.optLatency,
	)

	payload := map[string]any{
		"text":     text,
		"model_id": h.elModel,
		"voice_settings": map[string]any{
			"stability":         h.stability,
			"similarity_boost":  h.similarity,
			"style":             h.style,
			"use_speaker_boost": h.speakerBoost,
		},
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

// Opsional: GET /v1/voices/{voice_id} → untuk memastikan voice ada (debug)
func (h *Handler) getVoiceName(ctx context.Context) (string, error) {
	ep := fmt.Sprintf("https://api.elevenlabs.io/v1/voices/%s", h.elVoice)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ep, nil)
	req.Header.Set("xi-api-key", h.elKey)
	res, err := h.httpc.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		rb, _ := io.ReadAll(res.Body)
		return "", fmt.Errorf("verify voice %d: %s", res.StatusCode, strings.TrimSpace(string(rb)))
	}
	var jr struct{ Name string `json:"name"` }
	if err := json.NewDecoder(res.Body).Decode(&jr); err != nil {
		return "", err
	}
	if jr.Name == "" {
		return "(unknown)", nil
	}
	return jr.Name, nil
}

// ------------------- helpers -------------------

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

func getenvFirst(names ...string) string {
	for _, n := range names {
		if v := strings.TrimSpace(os.Getenv(n)); v != "" {
			return v
		}
	}
	return ""
}

func intFromEnv(name string, def int) int {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return def
}

func floatFromEnv(name string, def float64) float64 {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func boolFromEnv(name string, def bool) bool {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		switch strings.ToLower(v) {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
	}
	return def
}

func clamp(x, lo, hi int) int {
	if x < lo {
		return lo
	}
	if x > hi {
		return hi
	}
	return x
}

func trimWords(s string, max int) string {
	if max <= 0 {
		return s
	}
	parts := strings.Fields(s)
	if len(parts) <= max {
		return s
	}
	return strings.Join(parts[:max], " ")
}

// applyRateHint memberi “kesan” cepat/normal/slow via tanda baca & ritme.
func applyRateHint(s, rate string) string {
	rate = strings.ToLower(strings.TrimSpace(rate))
	switch rate {
	case "slow":
		if !strings.HasSuffix(s, ".") && !strings.HasSuffix(s, "!") && !strings.HasSuffix(s, "?") {
			s += "."
		}
		return strings.ReplaceAll(s, ",", ",  ") // jeda lebih terasa
	case "fast":
		s = strings.ReplaceAll(s, "…", "")
		s = strings.ReplaceAll(s, ", ", " ")
		return s
	default:
		return s
	}
}
