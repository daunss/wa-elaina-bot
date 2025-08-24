package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
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
	"wa-elaina/internal/tiktok"
	"wa-elaina/internal/wa"
)

var (
	// ====== ENV / Runtime ======
	sessionDB  string
	botName    string
	mode       string
	trigger    string
	sendAPIKey string // API key untuk /send

	// Gemini multi-keys
	geminiKeys  []string
	geminiIndex int

	// ElevenLabs
	elAPIKey  string
	elVoiceID string
	elMime    string // "audio/mpeg" atau "audio/ogg;codecs=opus"

	httpClient = &http.Client{Timeout: 45 * time.Second}
	waReady    atomic.Bool

	// deteksi permintaan VN (boleh di mana saja)
	reAskVoice = regexp.MustCompile(`(?i)\bvn\b|minta\s+suara|pakai\s+suara|voice(?:\s+note)?`)

	// batas kata khusus VN
	vnMaxWords int

	// ====== Rate limit untuk /send (token bucket per-IP) ======
	rlMu      sync.Mutex
	rlBuckets = map[string]*bucket{}
	rlCap     = mustAtoi(getenv("SEND_RATE_PER_MIN", "10")) // token per menit

	// ====== TikTok size limits (bytes), configurable via ENV ======
	ttMaxVideo  int64 // default: 50 MB
	ttMaxImage  int64 // default: 5 MB
	ttMaxDoc    int64 // default: 80 MB (fallback kirim sebagai dokumen)
	ttMaxSlides int   // default: 10 (batasi jumlah slide yang dikirim)

	// Blue Archive
	baURL       string
	baLocalPath string

	// composed helpers
	sender  *wa.Sender
	tiktokH *tiktok.Handler
	baMgr   *ba.Manager
)

type bucket struct {
	tokens     float64
	lastRefill time.Time
}

func init() {
	_ = godotenv.Load()

	// ---- Parse Gemini keys ----
	keysEnv := os.Getenv("GEMINI_API_KEYS")
	if keysEnv == "" {
		keysEnv = os.Getenv("GEMINI_API_KEY")
	}
	for _, part := range strings.Split(keysEnv, ",") {
		k := strings.TrimSpace(part)
		if k != "" {
			geminiKeys = append(geminiKeys, k)
		}
	}
	if len(geminiKeys) == 0 {
		log.Fatal("Tidak ada GEMINI_API_KEYS/GEMINI_API_KEY di .env (boleh beberapa key dipisah koma).")
	}
	geminiIndex = 0

	// ---- Lainnya ----
	sessionDB = getenv("SESSION_PATH", "session.db")
	botName = getenv("BOT_NAME", "Elaina")
	mode = strings.ToUpper(getenv("MODE", "MANUAL"))
	trigger = strings.ToLower(getenv("TRIGGER", "elaina"))

	// ElevenLabs
	elAPIKey = os.Getenv("ELEVENLABS_API_KEY")
	elVoiceID = getenv("ELEVENLABS_VOICE_ID", "iWydkXKoiVtvdn4vLKp9")
	elMime = getenv("ELEVENLABS_FORMAT", "audio/ogg;codecs=opus") // disarankan: audio/mpeg

	// VN_MAX_WORDS
	if n, err := strconv.Atoi(getenv("VN_MAX_WORDS", "80")); err == nil && n > 0 {
		vnMaxWords = n
	} else {
		vnMaxWords = 80
	}

	// Blue Archive env
	baURL = getenv("BA_LINKS_URL", "")
	baLocalPath = getenv("BA_LINKS_LOCAL", "anime/bluearchive_links.json")

	// API key untuk /send (opsional tapi disarankan)
	sendAPIKey = os.Getenv("SEND_API_KEY")
	if sendAPIKey == "" {
		log.Println("[WARN] SEND_API_KEY kosong: /send tidak terlindungi auth. Disarankan set SEND_API_KEY.")
	}

	// TikTok size limits
	ttMaxVideo = int64(mustAtoi(getenv("TIKTOK_MAX_VIDEO_MB", "50"))) << 20
	ttMaxImage = int64(mustAtoi(getenv("TIKTOK_MAX_IMAGE_MB", "5"))) << 20
	ttMaxDoc = int64(mustAtoi(getenv("TIKTOK_MAX_DOC_MB", "80"))) << 20
	ttMaxSlides = mustAtoi(getenv("TIKTOK_MAX_SLIDES", "10"))
}

func main() {
	ctx := context.Background()
	dsn := "file:" + sessionDB + "?_pragma=foreign_keys(1)"
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
			Video:  ttMaxVideo,
			Image:  ttMaxImage,
			Doc:    ttMaxDoc,
			Slides: ttMaxSlides,
		},
	}
	baMgr = ba.New(baURL, baLocalPath)

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
			} else {
				log.Println("skip message: WA not ready yet")
			}
		}
	})

	log.Printf("Bot %s is running...", botName)

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

	// ---------- HTTP endpoints ----------
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	http.HandleFunc("/help", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "Endpoints:\n"+
			"GET /healthz -> ok\n"+
			"GET /help -> bantuan ini\n"+
			"POST/GET /send?to=62xxxx&text=... (Header: X-API-Key)\n")
	})
	http.HandleFunc("/send", func(w http.ResponseWriter, r *http.Request) {
		// Auth via X-API-Key (opsional tapi direkomendasikan)
		if sendAPIKey != "" && r.Header.Get("X-API-Key") != sendAPIKey {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// Rate limit per-IP
		ip := clientIP(r)
		if !allow(ip) {
			http.Error(w, "rate limit", http.StatusTooManyRequests)
			return
		}
		to := r.URL.Query().Get("to")
		text := r.URL.Query().Get("text")
		if to == "" || text == "" {
			http.Error(w, "need 'to' & 'text'", http.StatusBadRequest)
			return
		}
		if !waReady.Load() {
			http.Error(w, "WA not ready", http.StatusServiceUnavailable)
			return
		}
		j := types.NewJID(to, types.DefaultUserServer)
		if err := sender.Text(wa.DestJID(j), text); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte("sent"))
	})

	// ---------- Server listen ----------
	p := getenv("PORT", "7860")
	log.Printf("Mode: %s | Trigger: %q | HTTP :%s\n", mode, trigger, p)
	log.Fatal(http.ListenAndServe(":"+p, nil))
}

func handleMessage(client *whatsmeow.Client, msg *events.Message) {
	if msg.Info.IsFromMe || !waReady.Load() { return }

	to := msg.Info.Chat
	isGroup := to.Server == types.GroupServer

	userText := extractText(msg)
	if userText == "" { return }

	// Commands
	if cmd, _, ok := parseCommand(userText); ok {
		switch cmd {
		case "help":
			_ = sender.Text(wa.DestJID(to), "Perintah:\n• !help — bantuan\n• Kirim link TikTok: bot kirim video langsung + link audio; slide akan dikirim sebagai gambar.")
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

	// Mode MANUAL (grup)
	if isGroup && mode == "MANUAL" {
		low := strings.ToLower(userText)
		if trigger == "" { trigger = "elaina" }
		found := false
		if i := strings.Index(low, "@"+trigger); i >= 0 {
			found = true
			userText = strings.TrimSpace(userText[:i] + userText[i+len("@"+trigger):])
		} else if i := strings.Index(low, trigger); i >= 0 {
			found = true
			userText = strings.TrimSpace(userText[:i] + userText[i+len(trigger):])
		}
		if !found { return }
		if userText == "" { userText = "hai" }
	}

	// Blue Archive
	if baMgr.MaybeHandle(context.Background(), client, wa.DestJID(to), userText) { return }

	// Apakah user minta VN?
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
Fakta akurat; opini beri alasan singkat; hindari SARA/eksplisit/berbahaya.
Catatan: Developer-ku adalah admin tersayang Daun.`
	if wantVN {
		system += "\nUntuk permintaan VN, jawablah sangat ringkas, mudah diucapkan, dan langsung ke inti."
	}
	if userText == "" {
		userText = "Tolong jawab singkat dalam 1–2 kalimat."
	}

	// Gemini
	reply, err := askGemini(system, userText)
	if err != nil {
		reply = "Ups, Elaina lagi tersandung jaringan. Coba lagi ya ✨"
	}

	// VN → TTS
	if wantVN {
		reply = limitWords(reply, vnMaxWords)
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

	// default kirim teks (trim panjang)
	if len(reply) > 3500 { reply = reply[:3500] + "…" }
	_ = sender.Text(wa.DestJID(to), reply)
}

// ---------- Helpers ringkas di main ----------

func extractText(m *events.Message) string {
	if m.Message.GetConversation() != "" { return m.Message.GetConversation() }
	if ext := m.Message.GetExtendedTextMessage(); ext != nil && ext.GetText() != "" { return ext.GetText() }
	return ""
}

func getenv(k, def string) string { if v := os.Getenv(k); v != "" { return v }; return def }

func mustAtoi(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n <= 0 { return 10 }
	return n
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

// parseCommand: deteksi perintah diawali "!", contoh: "!help", "!ping", "!cmd arg1 arg2"
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

// elevenTTS: panggil ElevenLabs → kembalikan audio bytes + MIME
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

// --- Rate limiting & HTTP helpers ---

func allow(key string) bool {
	now := time.Now()
	rlMu.Lock()
	defer rlMu.Unlock()

	b, ok := rlBuckets[key]
	if !ok {
		b = &bucket{tokens: float64(rlCap), lastRefill: now}
		rlBuckets[key] = b
	}
	elapsed := now.Sub(b.lastRefill).Minutes()
	if elapsed > 0 {
		if add := elapsed * float64(rlCap); b.tokens+add > float64(rlCap) {
			b.tokens = float64(rlCap)
		} else {
			b.tokens += add
		}
		b.lastRefill = now
	}
	if b.tokens < 1 { return false }
	b.tokens -= 1
	return true
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if parts := strings.Split(xff, ","); len(parts) > 0 {
			if ip := strings.TrimSpace(parts[0]); ip != "" { return "ip:" + ip }
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil { return "ip:" + r.RemoteAddr }
	return "ip:" + host
}

// ---------- Gemini API ----------

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
			Candidates []struct{ Content struct{ Parts []struct{ Text string `json:"text"` } `json:"parts"` } `json:"content"` } `json:"candidates"`
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
