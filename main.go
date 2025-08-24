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
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/joho/godotenv"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"

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

	// VN detection
	reAskVoice = regexp.MustCompile(`(?i)\bvn\b|minta\s+suara|pakai\s+suara|voice(?:\s+note)?`)
	reMention  = regexp.MustCompile(`(?i)\belaina\b`)

	// Komponen fitur
	tiktokH *tiktok.Handler
	baMgr   *ba.Manager

	// Gemini
	geminiKeys  []string
	geminiIndex int

	// ElevenLabs (tetap untuk fitur VN TTS yang sudah ada)
	elAPIKey  string
	elVoiceID string
	elMime    string
)

func init() {
	godotenv.Load()
	cfg = config.Load()

	// cache beberapa untuk akses cepat
	elAPIKey = cfg.ElevenAPIKey
	elVoiceID = cfg.ElevenVoice
	elMime = cfg.ElevenMime
	geminiKeys = cfg.GeminiKeys
}

func main() {
	ctx := context.Background()
	dsn := "file:" + cfg.SessionDB + "?_pragma=foreign_keys(1)"
	dbLog := waLog.Stdout("Database", "INFO", true)
	container, err := sqlstore.New(ctx, "sqlite", dsn, dbLog)
	if err != nil { log.Fatal(err) }
	device, err := container.GetFirstDevice(ctx)
	if err != nil { log.Fatal(err) }
	if device == nil { device = container.NewDevice() }

	client := whatsmeow.NewClient(device, nil)

	// compose helpers
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

	// event handlers
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
		if err := client.Connect(); err != nil { log.Fatal("connect:", err) }
		for e := range qr {
			switch e.Event {
			case "code": log.Println("Scan QR (code):", e.Code)
			case "success": log.Println("Login success")
			default: log.Println("QR event:", e.Event)
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
	if msg.Info.IsFromMe || !waReady.Load() { return }

	to := msg.Info.Chat
	isGroup := to.Server == types.GroupServer

	// === 1) Image Q&A ===
	if img := msg.Message.GetImageMessage(); img != nil {
		go func() {
			// FIX: tambah context sesuai signature terbaru
			data, err := client.Download(context.Background(), img)
			if err != nil {
				_ = sender.Text(wa.DestJID(to), "Maaf, gagal mengunduh gambar 😔")
				return
			}
			caption := strings.TrimSpace(img.GetCaption())
			visionSystem := `Kamu Elaina — analis visual cerdas & hangat.
Jawab singkat dalam Bahasa Indonesia, akurat, dan to the point.
Jika ada pertanyaan pada caption/user, jawab langsung; jika tidak ada, berikan deskripsi ringkas (1–3 kalimat) dan 1 insight bermanfaat.`

			if caption == "" {
				caption = "Tolong jelaskan gambar ini secara ringkas."
			}
			reply, err := askGeminiVision(visionSystem, caption, data, img.GetMimetype())
			if err != nil || strings.TrimSpace(reply) == "" {
				_ = sender.Text(wa.DestJID(to), "Aku belum bisa membaca gambar itu sekarang, coba lagi ya ✨")
				return
			}
			if len(reply) > 3500 { reply = reply[:3500] + "…" }
			_ = sender.Text(wa.DestJID(to), reply)
		}()
		return
	}

	// === 2) VN → Text → Auto-reply (only if mention "elaina" OR 1:1) ===
	if aud := msg.Message.GetAudioMessage(); aud != nil {
		go func() {
			// FIX: tambah context sesuai signature terbaru
			data, err := client.Download(context.Background(), aud)
			if err != nil {
				_ = sender.Text(wa.DestJID(to), "Maaf, gagal mengambil voice note 😔")
				return
			}
			// Step 1: Transcribe
			transcribeSystem := `Transkripsikan audio berikut ke teks Bahasa Indonesia yang bersih dan mudah dibaca. Hanya kembalikan teks transkripnya tanpa tambahan apapun.`
			tx, err := askGeminiTranscribe(transcribeSystem, data, aud.GetMimetype())
			if err != nil || strings.TrimSpace(tx) == "" {
				_ = sender.Text(wa.DestJID(to), "Aku nggak bisa dengar jelas VN-nya. Kirim ulang ya.")
				return
			}
			// Step 2: Tentukan apakah harus jawab
			shouldReply := !isGroup || reMention.FindStringIndex(tx) != nil
			if !shouldReply {
				// Diam bila di grup dan tidak menyebut "elaina"
				return
			}
			// Optional: bersihkan mention agar prompt lebih natural
			clean := reMention.ReplaceAllString(tx, "")
			clean = strings.TrimSpace(clean)
			if clean == "" { clean = tx }

			// Step 3: Jawab pakai persona Elaina (teks)
			system := `Perankan "Elaina", penyihir cerdas & hangat.
Gunakan orang pertama ("aku/ku") & panggil pengguna "kamu".
Gaya santai-sopan, playful secukupnya, emoji hemat.
Fakta akurat; opini beri alasan singkat; hindari SARA/eksplisit/berbahaya.`
			reply, err := askGemini(system, clean)
			if err != nil || strings.TrimSpace(reply) == "" {
				reply = "Ups, Elaina lagi kelelahan mendengar. Coba lagi ya ✨"
			}
			if len(reply) > 3500 { reply = reply[:3500] + "…" }
			_ = sender.Text(wa.DestJID(to), reply)
		}()
		return
	}

	// === 3) Alur teks biasa ===
	userText := extractText(msg)
	if userText == "" { return }

	// Commands
	if cmd, _, ok := parseCommand(userText); ok {
		switch cmd {
		case "help":
			_ = sender.Text(wa.DestJID(to), "Perintah:\n• !help — bantuan\n• Kirim link TikTok: bot kirim video langsung + link audio; slide akan dikirim sebagai gambar.\n• Kirim gambar — aku analisis & jawab.\n• Kirim VN sebut 'Elaina' — aku transkrip & jawab.")
			return
		case "ping":
			_ = sender.Text(wa.DestJID(to), "pong")
			return
		default:
			_ = sender.Text(wa.DestJID(to), "Perintah tidak dikenal. Ketik !help")
			return
		}
	}

	// TikTok
	if tiktokH.TryHandle(userText, to) { return }

	// Mode MANUAL (grup) — trigger teks
	if isGroup && cfg.Mode == "MANUAL" {
		low := strings.ToLower(userText)
		tr := cfg.Trigger
		if tr == "" { tr = "elaina" }

		found := false
		if i := strings.Index(low, "@"+tr); i >= 0 {
			found = true
			userText = strings.TrimSpace(userText[:i] + userText[i+len("@"+tr):])
		} else if i := strings.Index(low, tr); i >= 0 {
			found = true
			userText = strings.TrimSpace(userText[:i] + userText[i+len(tr):])
		}
		if !found { return }
		if userText == "" { userText = "hai" }
	}

	// Blue Archive
	if baMgr.MaybeHandle(context.Background(), client, wa.DestJID(to), userText) { return }

	// Apakah user minta VN TTS jawaban?
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
			_ = sender.Text(wa.DestJID(to), "[VN off] "+reply)
			return
		}
		audio, mime, err := elevenTTS(reply, elVoiceID, elMime)
		if err != nil {
			_ = sender.Text(wa.DestJID(to), reply) // fallback teks
			return
		}
		dur := estimateSecondsFromText(reply)
		_ = sender.Audio(wa.DestJID(to), audio, mime, true, dur)
		return
	}

	// default kirim teks
	if len(reply) > 3500 { reply = reply[:3500] + "…" }
	_ = sender.Text(wa.DestJID(to), reply)
}

// ---------- Helpers ringan ----------

func extractText(m *events.Message) string {
	if m.Message.GetConversation() != "" { return m.Message.GetConversation() }
	if ext := m.Message.GetExtendedTextMessage(); ext != nil && ext.GetText() != "" { return ext.GetText() }
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
	if max <= 0 { return s }
	parts := strings.Fields(strings.TrimSpace(s))
	if len(parts) <= max { return strings.Join(parts, " ") }
	return strings.Join(parts[:max], " ") + "…"
}

func estimateSecondsFromText(s string) uint32 {
	n := float64(len(strings.Fields(s))) / 2.5 // ~150 kata/menit
	if n < 1 { n = 1 }
	if n > 300 { n = 300 }
	return uint32(n + 0.5)
}

// ---------- ElevenLabs TTS (sudah ada sebelumnya) ----------

func elevenTTS(text, voiceID, mime string) ([]byte, string, error) {
	if mime == "" { mime = "audio/ogg;codecs=opus" }
	mime = strings.ReplaceAll(mime, " ", "")

	url := "https://api.elevenlabs.io/v1/text-to-speech/" + voiceID
	reqBody := map[string]any{"text": text}
	b, _ := json.Marshal(reqBody)

	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	req.Header.Set("xi-api-key", elAPIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", mime)

	resp, err := httpClient.Do(req)
	if err != nil { return nil, "", err }
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
	if len(geminiKeys) <= 1 { return false }
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

	resp, err := httpClient.Do(req)
	if err != nil { return nil, 0, err }
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
		if err := json.Unmarshal(rb, &out); err != nil { return "", err }
		if len(out.Candidates) == 0 || len(out.Candidates[0].Content.Parts) == 0 {
			return "Maaf, aku belum punya jawaban. Coba ulangi ya~", nil
		}
		return strings.TrimSpace(out.Candidates[0].Content.Parts[0].Text), nil
	}
	if lastErr != nil { return "", lastErr }
	return "", fmt.Errorf("gemini: tidak ada respons")
}

// ---------- Gemini API (multimodal: TRANSCRIBE AUDIO) ----------

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
	resp, err := httpClient.Do(req)
	if err != nil { return nil, 0, err }
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
		if err := json.Unmarshal(rb, &out); err != nil { return "", err }
		if len(out.Candidates) == 0 || len(out.Candidates[0].Content.Parts) == 0 {
			return "", nil
		}
		return strings.TrimSpace(out.Candidates[0].Content.Parts[0].Text), nil
	}
	if lastErr != nil { return "", lastErr }
	return "", fmt.Errorf("gemini: tidak ada respons")
}

// ---------- Gemini API (multimodal: VISION IMAGE QA) ----------

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
	resp, err := httpClient.Do(req)
	if err != nil { return nil, 0, err }
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
		if err := json.Unmarshal(rb, &out); err != nil { return "", err }
		if len(out.Candidates) == 0 || len(out.Candidates[0].Content.Parts) == 0 {
			return "", nil
		}
		return strings.TrimSpace(out.Candidates[0].Content.Parts[0].Text), nil
	}
	if lastErr != nil { return "", lastErr }
	return "", fmt.Errorf("gemini: tidak ada respons")
}
