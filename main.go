package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
	"sync/atomic"
	"strconv"

	"github.com/joho/godotenv"
	"google.golang.org/protobuf/proto"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"

	_ "modernc.org/sqlite"

	// Downloader TikTok (sudah ada)
	// GANTI sesuai module kamu, contoh:
	// dl "github.com/daunk852/wa-elaina-bot/downloader"
	dl "wa-elaina/downloader"

	// Fitur Blue Archive (baru)
	anime "wa-elaina/anime"
)

var (
	// ====== ENV / Runtime ======
	sessionDB         string
	botName           string
	mode              string
	trigger           string

	// Gemini multi-keys
	geminiKeys  []string
	geminiIndex int

	// ElevenLabs
	elAPIKey   string
	elVoiceID  string
	elMime     string // "audio/mpeg" atau "audio/ogg;codecs=opus"

	httpClient = &http.Client{Timeout: 45 * time.Second}
	waReady    atomic.Bool

	// deteksi permintaan VN (boleh di mana saja)
	reAskVoice = regexp.MustCompile(`(?i)\bvn\b|minta\s+suara|pakai\s+suara|voice(?:\s+note)?`)

	// batas kata khusus VN
	vnMaxWords int

	// ====== Blue Archive ======
	baLinks      []string // cache link gambar
	baURL        string   // BA_LINKS_URL (remote JSON)
	baLocalPath  string   // BA_LINKS_LOCAL (fallback file lokal)
)

func init() {
	_ = godotenv.Load()

	// ---- Parse Gemini keys ----
	// Prioritas: GEMINI_API_KEYS, fallback ke GEMINI_API_KEY (boleh berisi banyak key dipisah koma)
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
	botName   = getenv("BOT_NAME", "Elaina")
	mode      = strings.ToUpper(getenv("MODE", "MANUAL"))
	trigger   = strings.ToLower(getenv("TRIGGER", "elaina"))

	// ElevenLabs
	elAPIKey  = os.Getenv("ELEVENLABS_API_KEY")
	elVoiceID = getenv("ELEVENLABS_VOICE_ID", "iWydkXKoiVtvdn4vLKp9")
	elMime    = getenv("ELEVENLABS_FORMAT", "audio/ogg;codecs=opus") // disarankan: audio/mpeg untuk kestabilan

	// VN_MAX_WORDS
	if n, err := strconv.Atoi(getenv("VN_MAX_WORDS", "80")); err == nil && n > 0 {
		vnMaxWords = n
	} else {
		vnMaxWords = 80
	}

	// Blue Archive env (baru)
	// Remote URL dari .env: BA_LINKS_URL
	// Fallback lokal: BA_LINKS_LOCAL (default "anime/bluearchive_links.json")
	baURL       = getenv("BA_LINKS_URL", "")
	baLocalPath = getenv("BA_LINKS_LOCAL", "anime/bluearchive_links.json")
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
			case "code":    log.Println("Scan QR (code):", e.Code)
			case "success": log.Println("Login success")
			default:        log.Println("QR event:", e.Event)
			}
		}
	} else if err := client.Connect(); err != nil {
		log.Fatal("connect:", err)
	}

	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	http.HandleFunc("/send", func(w http.ResponseWriter, r *http.Request) {
		to := r.URL.Query().Get("to")
		text := r.URL.Query().Get("text")
		if to == "" || text == "" {
			http.Error(w, "need to & text", http.StatusBadRequest)
			return
		}
		if !waReady.Load() {
			http.Error(w, "WA not ready", http.StatusServiceUnavailable)
			return
		}
		j := types.NewJID(to, types.DefaultUserServer)
		if err := sendText(client, destJID(j), text); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Write([]byte("sent"))
	})

	log.Printf("Mode: %s | Trigger: %q | HTTP :7860\n", mode, trigger)
	log.Fatal(http.ListenAndServe(":7860", nil))
}

func handleMessage(client *whatsmeow.Client, msg *events.Message) {
	if msg.Info.IsFromMe || !waReady.Load() { return }

	to := msg.Info.Chat
	isGroup := to.Server == types.GroupServer

	userText := extractText(msg)
	if userText == "" { return }

	// — TikTok auto (tetap ada, tidak diubah)
	if urls := dl.DetectTikTokURLs(userText); len(urls) > 0 {
		reply, err := dl.HandleTikTok(httpClient, urls)
		if err != nil {
			log.Println("tiktok:", err)
			reply = "Maaf, unduhan TikTok bermasalah. Coba kirim lagi ya."
		}
		_ = sendText(client, destJID(to), reply)
		return
	}

	// — Mode MANUAL (grup) —> trigger boleh di mana saja (tetap sama)
	if isGroup && mode == "MANUAL" {
		low := strings.ToLower(userText)
		if trigger == "" { trigger = "elaina" }

		found := false
		pos := -1

		// cari "@elaina" dulu, lalu "elaina"
		if i := strings.Index(low, "@"+trigger); i >= 0 {
			found = true
			pos = i
			userText = strings.TrimSpace(userText[:i] + userText[i+len("@"+trigger):])
		} else if i := strings.Index(low, trigger); i >= 0 {
			found = true
			pos = i
			userText = strings.TrimSpace(userText[:i] + userText[i+len(trigger):])
		}

		if !found {
			log.Printf("manual mode: trigger %q tidak ditemukan, abaikan (text=%q)", trigger, userText)
			return
		}
		log.Printf("manual mode: trigger ditemukan di index %d; setelah hapus trigger => %q", pos, userText)

		if userText == "" { userText = "hai" }
	}

	// — Blue Archive request (BARU) — berlaku setelah pembersihan trigger di grup
	//    Deteksi pakai anime.IsBARequest, kirim 1 gambar acak via anime.SendRandomImage
	//    Links dimuat sekali via anime.LoadLinks dari BA_LINKS_URL (remote) atau BA_LINKS_LOCAL.
	if anime.IsBARequest(userText) { // :contentReference[oaicite:4]{index=4}
		if len(baLinks) == 0 {
			links, err := anime.LoadLinks(context.Background(), baLocalPath, baURL) // :contentReference[oaicite:5]{index=5}
			if err != nil {
				log.Println("BA LoadLinks error:", err)
				_ = sendText(client, destJID(to), "Maaf, daftar gambar Blue Archive belum tersedia.")
				return
			}
			baLinks = links
			log.Printf("BA links loaded: %d (url=%v local=%s)", len(baLinks), baURL != "", baLocalPath)
		}
		if err := anime.SendRandomImage(context.Background(), client, destJID(to), baLinks); err != nil { // :contentReference[oaicite:6]{index=6}
			log.Println("BA SendRandomImage error:", err)
			_ = sendText(client, destJID(to), "Gagal mengirim gambar Blue Archive.")
		}
		return
	}

	// — Apakah user minta VN? (boleh di mana saja) — tetap sama
	wantVN := false
	if loc := reAskVoice.FindStringIndex(userText); loc != nil {
		wantVN = true
		before := strings.TrimSpace(userText[:loc[0]])
		after  := strings.TrimSpace(userText[loc[1]:])
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
		log.Printf("VN detected: cleaned text => %q", userText)
	}

	// persona + info developer — tetap sama
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

	log.Printf("askGemini: wantVN=%v | text=%q", wantVN, userText)
	reply, err := askGemini(system, userText) // <- tetap: rotasi API key otomatis
	if err != nil {
		log.Printf("askGemini error: %v", err)
		reply = "Ups, Elaina lagi tersandung jaringan. Coba lagi ya ✨"
	}
	log.Printf("askGemini: got reply (len=%d)", len(reply))

	// VN → batasi kata + synthesize → kirim audio — tetap sama
	if wantVN {
		reply = limitWords(reply, vnMaxWords)
		if elAPIKey == "" {
			_ = sendText(client, destJID(to), "[VN off] "+reply)
			return
		}
		log.Printf("TTS start: len=%d", len(reply))
		audio, mime, err := elevenTTS(reply, elVoiceID, elMime)
		if err != nil {
			log.Printf("tts error: %v", err)
			_ = sendText(client, destJID(to), reply) // fallback teks
			return
		}
		dur := estimateSecondsFromText(reply)
		dst := destJID(to)
		if err := sendAudio(client, dst, audio, mime, true, dur); err != nil {
			log.Printf("send audio error: %v", err)
			// Fallback: coba MP3 jika OGG/Opus bermasalah
			if !strings.Contains(strings.ToLower(mime), "audio/mpeg") {
				if a2, m2, e2 := elevenTTS(reply, elVoiceID, "audio/mpeg"); e2 == nil {
					if e3 := sendAudio(client, dst, a2, m2, true, dur); e3 == nil {
						log.Printf("send audio fallback MP3: success")
						return
					} else {
						log.Printf("send audio fallback MP3 error: %v", e3)
					}
				} else {
					log.Printf("tts fallback mp3 error: %v", e2)
				}
			}
			_ = sendText(client, dst, reply) // fallback teks terakhir
		} else {
			log.Printf("send audio -> %s | mime=%s | dur=%ds", dst, mime, dur)
		}
		return
	}

	// default kirim teks — tetap sama
	if len(reply) > 3500 { reply = reply[:3500] + "…" }
	log.Printf("send text: len=%d", len(reply))
	_ = sendText(client, destJID(to), reply)
}

// ---------- Helper tujuan kirim ----------
func destJID(j types.JID) types.JID {
	if j.Server == types.GroupServer {
		return j
	}
	return j.ToNonAD()
}

// ---------- Batas kata VN ----------
func limitWords(s string, max int) string {
	if max <= 0 {
		return s
	}
	s = strings.TrimSpace(s)
	parts := strings.Fields(s)
	if len(parts) <= max {
		return s
	}
	return strings.Join(parts[:max], " ") + "…"
}

// Perkiraan durasi (detik) agar player WA tidak macet 0:00
func estimateSecondsFromText(s string) uint32 {
	n := float64(len(strings.Fields(s))) / 2.5 // ~150 kata/menit
	if n < 1 { n = 1 }
	if n > 300 { n = 300 }
	return uint32(n + 0.5)
}

// ---------- ElevenLabs TTS (pakai default voice settings) ----------
func elevenTTS(text, voiceID, mime string) ([]byte, string, error) {
	if mime == "" { mime = "audio/ogg;codecs=opus" }
	mime = strings.ReplaceAll(mime, " ", "")

	url := "https://api.elevenlabs.io/v1/text-to-speech/" + voiceID
	reqBody := map[string]any{ "text": text } // tanpa voice_settings -> pakai default
	b, _ := json.Marshal(reqBody)

	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	req.Header.Set("xi-api-key", elAPIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", mime) // "audio/mpeg" atau "audio/ogg;codecs=opus"

	resp, err := httpClient.Do(req)
	if err != nil { return nil, "", err }
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("elevenlabs %s: %s", resp.Status, string(data))
	}
	return data, mime, nil
}

// ---------- Kirim pesan ----------
func sendText(client *whatsmeow.Client, to types.JID, text string) error {
	_, err := client.SendMessage(context.Background(), to, &waProto.Message{
		Conversation: proto.String(text),
	})
	if err != nil && strings.Contains(err.Error(), "479") {
		time.Sleep(2 * time.Second)
		_, err = client.SendMessage(context.Background(), to, &waProto.Message{
			Conversation: proto.String(text),
		})
	}
	return err
}

func sendAudio(client *whatsmeow.Client, to types.JID, audio []byte, mime string, ptt bool, seconds uint32) error {
	up, err := client.Upload(context.Background(), audio, whatsmeow.MediaAudio)
	if err != nil { return err }

	msg := &waProto.Message{
		AudioMessage: &waProto.AudioMessage{
			URL:           proto.String(up.URL),
			DirectPath:    proto.String(up.DirectPath),
			MediaKey:      up.MediaKey,
			FileEncSHA256: up.FileEncSHA256,
			FileSHA256:    up.FileSHA256,
			FileLength:    proto.Uint64(uint64(len(audio))),
			Mimetype:      proto.String(mime),
			PTT:           proto.Bool(ptt),
			Seconds:       proto.Uint32(seconds),
		},
	}
	_, err = client.SendMessage(context.Background(), to, msg)
	if err != nil && strings.Contains(err.Error(), "479") {
		time.Sleep(2 * time.Second)
		_, err = client.SendMessage(context.Background(), to, msg)
	}
	return err
}

// ---------- Helper Gemini: rotasi key & request ----------
func getGeminiKey() string {
	return geminiKeys[geminiIndex]
}

func rotateGeminiKey() bool {
	if len(geminiKeys) <= 1 { return false }
	geminiIndex = (geminiIndex + 1) % len(geminiKeys)
	log.Printf("Ganti ke Gemini API Key index %d", geminiIndex)
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
				// rotate & retry
				if rotateGeminiKey() {
					continue
				}
			}
			// error non-429: hentikan
			break
		}
		// sukses -> parse & return
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

// ---------- Util ----------
func extractText(m *events.Message) string {
	if m.Message.GetConversation() != "" { return m.Message.GetConversation() }
	if ext := m.Message.GetExtendedTextMessage(); ext != nil && ext.GetText() != "" { return ext.GetText() }
	return ""
}

func getenv(k, def string) string { if v := os.Getenv(k); v != "" { return v }; return def }
