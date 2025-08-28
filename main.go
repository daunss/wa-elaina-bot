package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/joho/godotenv"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	pbf "google.golang.org/protobuf/proto"

	_ "modernc.org/sqlite"

	// internal packages
	"wa-elaina/internal/ba"
	"wa-elaina/internal/config"
	"wa-elaina/internal/httpapi"
	"wa-elaina/internal/tiktok"
	"wa-elaina/internal/wa"
)

var (
	// Konfigurasi terpusat
	cfg config.Config

	// WhatsApp runtime
	httpClient = &http.Client{Timeout: 45 * time.Second}
	waReady    atomic.Bool
	sender     *wa.Sender

	// Regex & gating
	reAskVoice = regexp.MustCompile(`(?i)\bvn\b|minta\s+suara|pakai\s+suara|voice(?:\s+note)?`)
	// Fuzzy untuk panggilan Elaina di hasil transkrip
	reMention = regexp.MustCompile(`(?i)\b(elaina|eleina|elena|elina|ela?ina)\b`)
	// Trigger untuk teks/caption (diisi saat init dari cfg.Trigger)
	reTrigger *regexp.Regexp

	// Komponen fitur
	tiktokH *tiktok.Handler
	baMgr   *ba.Manager

	// Gemini
	geminiKeys  []string
	geminiIndex int

	// ElevenLabs (opsional)
	elAPIKey  string
	elVoiceID string
	elMime    string
)

func init() {
	_ = godotenv.Load()
	cfg = config.Load()

	elAPIKey = cfg.ElevenAPIKey
	elVoiceID = cfg.ElevenVoice
	elMime = cfg.ElevenMime
	geminiKeys = cfg.GeminiKeys

	trig := cfg.Trigger
	if trig == "" {
		trig = "elaina"
	}
	reTrigger = regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(trig) + `\b`)
}

func main() {
	ctx := context.Background()
	dsn := "file:" + cfg.SessionDB + "?_pragma=foreign_keys(1)"
	dbLog := waLog.Stdout("Database", "INFO", true)
	container, err := sqlstore.New(ctx, "sqlite", dsn, dbLog)
	if err != nil {
		log.Fatal(err)
	}
	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		log.Fatal(err)
	}
	if device == nil {
		device = container.NewDevice()
	}

	client := whatsmeow.NewClient(device, nil)
	sender = wa.NewSender(client)

	tiktokH = &tiktok.Handler{
		Client: httpClient,
		Send:   sender,
		L: tiktok.Limits{
			Video:  cfg.TTMaxVideo,
			Image:  cfg.TTMaxImage,
			Doc:    cfg.TTMaxDoc,
			Slides: cfg.TTMaxSlides,
		},
	}
	baMgr = ba.New(cfg.BALinksURL, cfg.BALinksLocal)

	client.AddEventHandler(func(ev interface{}) {
		switch e := ev.(type) {
		case *events.Connected, *events.AppStateSyncComplete:
			waReady.Store(true)
			log.Println("WhatsApp state: READY (connected & app state synced)")
		case *events.Disconnected:
			waReady.Store(false)
			log.Println("WhatsApp state: DISCONNECTED")
		case *events.Message:
			if waReady.Load() {
				handleMessage(client, e)
			}
		}
	})

	log.Printf("Bot %s is running...", cfg.BotName)

	// connect WA
	if client.Store.ID == nil {
		qr, _ := client.GetQRChannel(context.Background())
		if err := client.Connect(); err != nil {
			log.Fatal("connect:", err)
		}
		for e := range qr {
			switch e.Event {
			case "code":
				log.Println("Scan QR (code):", e.Code)
			case "success":
				log.Println("Login success")
			default:
				log.Println("QR event:", e.Event)
			}
		}
	} else if err := client.Connect(); err != nil {
		log.Fatal("connect:", err)
	}

	// HTTP API (moved to package httpapi)
	api := httpapi.New(cfg, sender, &waReady)
	api.RegisterHandlers(http.DefaultServeMux)

	log.Printf("Mode: %s | Trigger: %q | HTTP :%s\n", cfg.Mode, cfg.Trigger, cfg.Port)
	log.Fatal(http.ListenAndServe(":"+cfg.Port, nil))
}

func handleMessage(client *whatsmeow.Client, msg *events.Message) {
	if msg.Info.IsFromMe || !waReady.Load() {
		return
	}
	to := msg.Info.Chat
	isGroup := to.Server == types.GroupServer

	// Optional: tandai terbaca saat diproses (no-op untuk kompatibilitas)
	markReadSafe(client, msg)

	// =====================================================================
	// 1) IMAGE — TIDAK otomatis. Wajib ada trigger di caption
	// =====================================================================
	if img := msg.Message.GetImageMessage(); img != nil {
		caption := strings.TrimSpace(img.GetCaption())
		if !reTrigger.MatchString(caption) {
			return // abaikan gambar tanpa trigger
		}
		go func(m *events.Message) {
			defer guard()
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			data, err := client.Download(ctx, img)
			if err != nil {
				_ = replyText(client, to, "Maaf, gagal mengunduh gambar 😔", m)
				return
			}
			userPrompt := strings.TrimSpace(reTrigger.ReplaceAllString(caption, ""))
			if userPrompt == "" {
				userPrompt = "Tolong jelaskan gambar ini secara ringkas."
			}
			system := `Kamu Elaina — analis visual cerdas & hangat.
Jawab singkat dalam Bahasa Indonesia, akurat, dan to the point.
Jika tidak ada pertanyaan eksplisit, berikan 1–3 kalimat deskripsi + 1 insight.`
			reply, err := askGeminiVision(system, userPrompt, data, img.GetMimetype())
			if err != nil || strings.TrimSpace(reply) == "" {
				reply = "Aku belum bisa membaca gambar itu sekarang, coba lagi ya ✨"
			}
			if len(reply) > 3500 {
				reply = reply[:3500] + "…"
			}
			_ = replyText(client, to, reply, m) // *** balas sebagai reply ***
		}(msg)
		return
	}

	// =====================================================================
	// 2) VN — transkrip; balas HANYA jika transkrip menyebut “elaina”
	// =====================================================================
	if aud := msg.Message.GetAudioMessage(); aud != nil {
		go func(m *events.Message) {
			defer guard()
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			data, err := client.Download(ctx, aud)
			if err != nil {
				_ = replyText(client, to, "Maaf, gagal mengambil voice note 😔", m)
				return
			}
			transcribeSystem := `Transkripsikan audio berikut ke teks Bahasa Indonesia yang bersih dan mudah dibaca. Hanya kembalikan teks transkripnya tanpa tambahan apapun.`
			mime := normalizeAudioMime(aud.GetMimetype())
			tx, err := askGeminiTranscribe(transcribeSystem, data, mime)
			if err != nil || strings.TrimSpace(tx) == "" {
				_ = replyText(client, to, "Aku nggak bisa dengar jelas VN-nya. Kirim ulang ya.", m)
				return
			}
			if !reMention.MatchString(tx) {
				if strings.EqualFold(getenv("VN_DEBUG_TRANSCRIPT", "false"), "true") {
					_ = replyText(client, to, "📝 Transkrip: "+limitWords(tx, 120)+`\n(sebut "Elaina" agar aku membalas)`, m)
				}
				return
			}
			clean := reMention.ReplaceAllString(tx, "")
			clean = strings.TrimSpace(clean)
			if clean == "" {
				clean = tx
			}
			system := `Perankan "Elaina", penyihir cerdas & hangat.
Gunakan orang pertama ("aku/ku") & panggil pengguna "kamu".
Gaya santai-sopan, playful secukupnya, emoji hemat.
Fakta akurat; opini beri alasan singkat; hindari SARA/eksplisit/berbahaya.`
			reply, err := askGemini(system, clean)
			if err != nil || strings.TrimSpace(reply) == "" {
				reply = "Ups, Elaina lagi kelelahan mendengar. Coba lagi ya ✨"
			}
			if len(reply) > 3500 {
				reply = reply[:3500] + "…"
			}
			_ = replyText(client, to, reply, m) // *** reply ke pesan VN ***
		}(msg)
		return
	}

	// =====================================================================
	// 3) TEKS — including REPLY ke media (image/audio)
	// =====================================================================
	userText := extractText(msg)
	if userText == "" {
		return
	}

	// Commands
	if cmd, _, ok := parseCommand(userText); ok {
		switch cmd {
		case "help":
			_ = replyText(client, to, "Perintah:\n• !help — bantuan\n• Kirim link TikTok: bot kirim video + link audio; slide jadi gambar.\n• Kirim gambar + sebut '"+cfg.Trigger+"' — aku analisis & jawab.\n• Kirim VN dan sebut 'Elaina' — aku transkrip & jawab.", msg)
			return
		case "ping":
			_ = replyText(client, to, "pong", msg)
			return
		default:
			_ = replyText(client, to, "Perintah tidak dikenal. Ketik !help", msg)
			return
		}
	}

	// === 3a) Jika ini REPLY ke media + ada trigger, proses media yang di-quote ===
	if xt := msg.Message.GetExtendedTextMessage(); xt != nil && xt.ContextInfo != nil && reTrigger.MatchString(userText) {
		qm := xt.GetContextInfo().GetQuotedMessage()
		switch {
		case qm.GetImageMessage() != nil:
			qimg := qm.GetImageMessage()
			go func(m *events.Message) {
				defer guard()
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()
				data, err := client.Download(ctx, qimg)
				if err != nil {
					_ = replyText(client, to, "Gagal mengambil gambar yang direply 😔", m)
					return
				}
				prompt := strings.TrimSpace(reTrigger.ReplaceAllString(userText, ""))
				if prompt == "" {
					prompt = "Tolong jelaskan gambar ini secara ringkas."
				}
				system := `Kamu Elaina — analis visual cerdas & hangat. Jawab ringkas & akurat.`
				reply, err := askGeminiVision(system, prompt, data, qimg.GetMimetype())
				if err != nil || strings.TrimSpace(reply) == "" {
					reply = "Aku belum bisa membaca gambar itu sekarang, coba lagi ya ✨"
				}
				if len(reply) > 3500 {
					reply = reply[:3500] + "…"
				}
				_ = replyText(client, to, reply, m) // reply ke teks pemicu
			}(msg)
			return

		case qm.GetAudioMessage() != nil:
			qaud := qm.GetAudioMessage()
			go func(m *events.Message) {
				defer guard()
				ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
				defer cancel()
				data, err := client.Download(ctx, qaud)
				if err != nil {
					_ = replyText(client, to, "Gagal mengambil VN yang direply 😔", m)
					return
				}
				tx, err := askGeminiTranscribe(`Transkripsikan audio ke teks Indonesia yang bersih.`, data, normalizeAudioMime(qaud.GetMimetype()))
				if err != nil || strings.TrimSpace(tx) == "" {
					_ = replyText(client, to, "VN tidak jelas, kirim ulang ya.", m)
					return
				}
				cleanPrompt := strings.TrimSpace(reTrigger.ReplaceAllString(userText, ""))
				if cleanPrompt == "" {
					cleanPrompt = "Ringkas isi VN berikut & jawab pertanyaan jika ada."
				}
				system := `Perankan "Elaina" — jawab ramah & to the point.`
				reply, _ := askGemini(system, cleanPrompt+"\n\nTranskrip:\n"+tx)
				if strings.TrimSpace(reply) == "" {
					reply = "Siap. Ada yang ingin ditanyakan dari VN tadi?"
				}
				_ = replyText(client, to, reply, m)
			}(msg)
			return
		}
	}

	// TikTok (tetap: berdasarkan URL dalam teks)
	if tiktokH.TryHandle(userText, to) {
		return
	}

	// Mode MANUAL (grup) — trigger teks
	if isGroup && cfg.Mode == "MANUAL" {
		low := strings.ToLower(userText)
		tr := cfg.Trigger
		if tr == "" {
			tr = "elaina"
		}
		found := false
		if i := strings.Index(low, "@"+tr); i >= 0 {
			found = true
			userText = strings.TrimSpace(userText[:i] + userText[i+len("@"+tr):])
		} else if i := strings.Index(low, tr); i >= 0 {
			found = true
			userText = strings.TrimSpace(userText[:i] + userText[i+len(tr):])
		}
		if !found {
			return
		}
		if userText == "" {
			userText = "hai"
		}
	}

	// Blue Archive
	if baMgr.MaybeHandle(context.Background(), client, wa.DestJID(to), userText) {
		return
	}

	// Permintaan VN untuk jawaban teks?
	wantVN := false
	if loc := reAskVoice.FindStringIndex(userText); loc != nil {
		wantVN = true
		before := strings.TrimSpace(userText[:loc[0]])
		after := strings.TrimSpace(userText[loc[1]:])
		switch {
		case before != "" && after != "":
			userText = strings.TrimSpace(before + " " + after)
		case after != "":
			userText = after
		case before != "":
			userText = before
		default:
			userText = ""
		}
	}

	// Persona
	system := `Perankan "Elaina", penyihir cerdas & hangat.
Gunakan orang pertama ("aku/ku") & panggil pengguna "kamu".
JANGAN menulis "Kamu Elaina" atau bicara orang ketiga.
Gaya santai-sopan, playful secukupnya, emoji hemat.
Fakta akurat; opini beri alasan singkat; hindari SARA/eksplisit/berbahaya.`
	if wantVN {
		system += "\nUntuk permintaan VN, jawablah sangat ringkas, mudah diucapkan, dan langsung ke inti."
	}
	if strings.TrimSpace(userText) == "" {
		userText = "Tolong jawab singkat dalam 1–2 kalimat."
	}

	// Gemini (teks)
	reply, err := askGemini(system, userText)
	if err != nil {
		reply = "Ups, Elaina lagi tersandung jaringan. Coba lagi ya ✨"
	}

	// VN → TTS (jawaban)
	if wantVN {
		reply = limitWords(reply, cfg.VNMaxWords)
		if elAPIKey == "" {
			_ = replyText(client, to, "[VN off] "+reply, msg)
			return
		}
		audio, mime, err := elevenTTS(reply, elVoiceID, elMime)
		if err != nil {
			_ = replyText(client, to, reply, msg)
			return
		}
		dur := estimateSecondsFromText(reply)
		// NOTE: kirim audio tanpa quote dulu (butuh upload + contextinfo). Fokus reply teks sesuai scope.
		_ = sender.Audio(wa.DestJID(to), audio, mime, true, dur)
		return
	}

	if len(reply) > 3500 {
		reply = reply[:3500] + "…"
	}
	_ = replyText(client, to, reply, msg) // default reply teks
}

// ================= Helpers =================

func guard() {
	if r := recover(); r != nil {
		log.Printf("panic: %v", r)
	}
}

func extractText(m *events.Message) string {
	if m.Message.GetConversation() != "" {
		return m.Message.GetConversation()
	}
	if ext := m.Message.GetExtendedTextMessage(); ext != nil && ext.GetText() != "" {
		return ext.GetText()
	}
	return ""
}

// parseCommand: deteksi perintah diawali "!"
func parseCommand(s string) (cmd, args string, ok bool) {
	trim := strings.TrimSpace(s)
	if trim == "" || !strings.HasPrefix(trim, "!") {
		return "", "", false
	}
	trim = strings.TrimPrefix(trim, "!")
	parts := strings.Fields(trim)
	if len(parts) == 0 {
		return "", "", false
	}
	cmd = strings.ToLower(parts[0])
	args = strings.TrimSpace(strings.TrimPrefix(trim, parts[0]))
	return cmd, args, true
}

func limitWords(s string, max int) string {
	if max <= 0 {
		return s
	}
	parts := strings.Fields(strings.TrimSpace(s))
	if len(parts) <= max {
		return strings.Join(parts, " ")
	}
	return strings.Join(parts[:max], " ") + "…"
}

func estimateSecondsFromText(s string) uint32 {
	n := float64(len(strings.Fields(s))) / 2.5 // ~150 kata/menit
	if n < 1 {
		n = 1
	}
	if n > 300 {
		n = 300
	}
	return uint32(n + 0.5)
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func normalizeAudioMime(m string) string {
	m = strings.ToLower(strings.TrimSpace(m))
	if i := strings.Index(m, ";"); i >= 0 {
		m = m[:i]
	}
	return strings.TrimSpace(m)
}

// ============ Reply helper (quote pesan pemicu) ============

// replyText mengirim balasan sebagai *reply* ke pesan 'quoted'.
func replyText(client *whatsmeow.Client, to types.JID, text string, quoted *events.Message) error {
	// tanpa quoted → fallback text biasa
	if quoted == nil {
		return sender.Text(wa.DestJID(to), text)
	}
	ci := &waProto.ContextInfo{
		StanzaID:     pbf.String(quoted.Info.ID),
		Participant:  pbf.String(quoted.Info.Sender.String()),
		RemoteJID:    pbf.String(quoted.Info.Chat.String()),
		QuotedMessage: quoted.Message,
	}
	msg := &waProto.Message{
		ExtendedTextMessage: &waProto.ExtendedTextMessage{
			Text:        pbf.String(text),
			ContextInfo: ci,
		},
	}
	_, err := client.SendMessage(context.Background(), to, msg)
	return err
}

// markReadSafe: no-op agar kompatibel lintas versi whatsmeow
func markReadSafe(client *whatsmeow.Client, m *events.Message) {}

// ---------- ElevenLabs TTS (opsional) ----------

func elevenTTS(text, voiceID, mime string) ([]byte, string, error) {
	if mime == "" {
		mime = "audio/ogg;codecs=opus"
	}
	mime = strings.ReplaceAll(mime, " ", "")

	url := "https://api.elevenlabs.io/v1/text-to-speech/" + voiceID
	reqBody := map[string]any{"text": text}
	b, _ := json.Marshal(reqBody)

	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	req.Header.Set("xi-api-key", elAPIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", mime)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("elevenlabs %s: %s", resp.Status, string(data))
	}
	return data, mime, nil
}

// ---------- Gemini API (teks) ----------

func getGeminiKey() string { return geminiKeys[geminiIndex] }

func rotateGeminiKey() bool {
	if len(geminiKeys) <= 1 {
		return false
	}
	geminiIndex = (geminiIndex + 1) % len(geminiKeys)
	return true
}

func callGemini(key, systemPrompt, userText string) ([]byte, int, error) {
	endpoint := "https://generativelanguage.googleapis.com/v1beta/models/gemini-1.5-flash:generateContent?key=" + key
	body := map[string]any{
		"system_instruction": map[string]any{"role": "system", "parts": []map[string]string{{"text": systemPrompt}}},
		"contents":           []map[string]any{{"role": "user", "parts": []map[string]string{{"text": userText}}}},
	}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	req = req.WithContext(ctx)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return rb, resp.StatusCode, fmt.Errorf("gemini %s: %s", resp.Status, string(rb))
	}
	return rb, resp.StatusCode, nil
}

func askGemini(systemPrompt, userText string) (string, error) {
	var lastErr error
	for i := 0; i < len(geminiKeys); i++ {
		key := getGeminiKey()
		rb, status, err := callGemini(key, systemPrompt, userText)
		if err != nil {
            lastErr = err
            if status == 429 || strings.Contains(strings.ToLower(err.Error()), "resource_exhausted") {
                if rotateGeminiKey() { continue }
            }
            break
        }
		var out struct {
			Candidates []struct {
				Content struct {
					Parts []struct{ Text string `json:"text"` } `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
		}
		if err := json.Unmarshal(rb, &out); err != nil {
			return "", err
		}
		if len(out.Candidates) == 0 || len(out.Candidates[0].Content.Parts) == 0 {
			return "Maaf, aku belum punya jawaban. Coba ulangi ya~", nil
		}
		return strings.TrimSpace(out.Candidates[0].Content.Parts[0].Text), nil
	}
	if lastErr != nil {
		return "", lastErr
	}
	return "", fmt.Errorf("gemini: tidak ada respons")
}

// ---------- Gemini API (TRANSCRIBE AUDIO) ----------

func callGeminiTranscribe(key, systemPrompt string, audio []byte, mime string) ([]byte, int, error) {
	endpoint := "https://generativelanguage.googleapis.com/v1beta/models/gemini-1.5-flash:generateContent?key=" + key
	b64 := base64.StdEncoding.EncodeToString(audio)
	body := map[string]any{
		"system_instruction": map[string]any{"role": "system", "parts": []map[string]string{{"text": systemPrompt}}},
		"contents": []map[string]any{{
			"role": "user",
			"parts": []any{
				map[string]any{"text": "Audio berikut untuk ditranskrip:"},
				map[string]any{"inlineData": map[string]any{"mimeType": mime, "data": b64}},
			},
		}},
	}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req = req.WithContext(ctx)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return rb, resp.StatusCode, fmt.Errorf("gemini %s: %s", resp.Status, string(rb))
	}
	return rb, resp.StatusCode, nil
}

func askGeminiTranscribe(systemPrompt string, audio []byte, mime string) (string, error) {
	var lastErr error
	for i := 0; i < len(geminiKeys); i++ {
		key := getGeminiKey()
		rb, status, err := callGeminiTranscribe(key, systemPrompt, audio, mime)
		if err != nil {
            lastErr = err
            if status == 429 || strings.Contains(strings.ToLower(err.Error()), "resource_exhausted") {
                if rotateGeminiKey() { continue }
            }
            break
        }
		var out struct {
			Candidates []struct {
				Content struct {
					Parts []struct{ Text string `json:"text"` } `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
		}
		if err := json.Unmarshal(rb, &out); err != nil {
			return "", err
		}
		if len(out.Candidates) == 0 || len(out.Candidates[0].Content.Parts) == 0 {
			return "", nil
		}
		return strings.TrimSpace(out.Candidates[0].Content.Parts[0].Text), nil
	}
	if lastErr != nil {
		return "", lastErr
	}
	return "", fmt.Errorf("gemini: tidak ada respons")
}

// ---------- Gemini API (VISION IMAGE QA) ----------

func callGeminiVision(key, systemPrompt, userPrompt string, image []byte, mime string) ([]byte, int, error) {
	endpoint := "https://generativelanguage.googleapis.com/v1beta/models/gemini-1.5-flash:generateContent?key=" + key
	b64 := base64.StdEncoding.EncodeToString(image)
	parts := []any{}
	if strings.TrimSpace(userPrompt) != "" {
		parts = append(parts, map[string]any{"text": userPrompt})
	}
	parts = append(parts, map[string]any{"inlineData": map[string]any{"mimeType": mime, "data": b64}})

	body := map[string]any{
		"system_instruction": map[string]any{"role": "system", "parts": []map[string]string{{"text": systemPrompt}}},
		"contents": []map[string]any{{
			"role":  "user",
			"parts": parts,
		}},
	}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req = req.WithContext(ctx)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return rb, resp.StatusCode, fmt.Errorf("gemini %s: %s", resp.Status, string(rb))
	}
	return rb, resp.StatusCode, nil
}

func askGeminiVision(systemPrompt, userPrompt string, image []byte, mime string) (string, error) {
	var lastErr error
	for i := 0; i < len(geminiKeys); i++ {
		key := getGeminiKey()
		rb, status, err := callGeminiVision(key, systemPrompt, userPrompt, image, mime)
		if err != nil {
            lastErr = err
            if status == 429 || strings.Contains(strings.ToLower(err.Error()), "resource_exhausted") {
                if rotateGeminiKey() { continue }
            }
            break
        }
		var out struct {
			Candidates []struct {
				Content struct {
					Parts []struct{ Text string `json:"text"` } `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
		}
		if err := json.Unmarshal(rb, &out); err != nil {
			return "", err
		}
		if len(out.Candidates) == 0 || len(out.Candidates[0].Content.Parts) == 0 {
			return "", nil
		}
		return strings.TrimSpace(out.Candidates[0].Content.Parts[0].Text), nil
	}
	if lastErr != nil {
		return "", lastErr
	}
	return "", fmt.Errorf("gemini: tidak ada respons")
}
