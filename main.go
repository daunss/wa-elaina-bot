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
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
	"sync"
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

	// Hanya pakai deteksi URL dari package downloader
	dl "wa-elaina/downloader"

	// Fitur Blue Archive
	anime "wa-elaina/anime"
)

var (
	// ====== ENV / Runtime ======
	sessionDB   string
	botName     string
	mode        string
	trigger     string
	sendAPIKey  string // API key untuk /send

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

	// ====== Blue Archive ======
	baLinks     []string // cache link gambar
	baURL       string   // BA_LINKS_URL (remote JSON)
	baLocalPath string   // BA_LINKS_LOCAL (fallback file lokal)

	// ====== Rate limit untuk /send (token bucket per-IP) ======
	rlMu      sync.Mutex
	rlBuckets = map[string]*bucket{}
	rlCap     = mustAtoi(getenv("SEND_RATE_PER_MIN", "10")) // token per menit

	// ====== TikTok size limits (bytes), configurable via ENV ======
	ttMaxVideo int64 // default: 50 MB
	ttMaxImage int64 // default: 5 MB
	ttMaxDoc   int64 // default: 80 MB (fallback kirim sebagai dokumen)
	ttMaxSlides int  // default: 10 (batasi jumlah slide yang dikirim)
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

	// Blue Archive env
	baURL       = getenv("BA_LINKS_URL", "")
	baLocalPath = getenv("BA_LINKS_LOCAL", "anime/bluearchive_links.json")

	// API key untuk /send (opsional tapi disarankan)
	sendAPIKey = os.Getenv("SEND_API_KEY")
	if sendAPIKey == "" {
		log.Println("[WARN] SEND_API_KEY kosong: /send tidak terlindungi auth. Disarankan set SEND_API_KEY.")
	}

	// TikTok size limits
	ttMaxVideo  = int64(mustAtoi(getenv("TIKTOK_MAX_VIDEO_MB", "50"))) << 20
	ttMaxImage  = int64(mustAtoi(getenv("TIKTOK_MAX_IMAGE_MB", "5"))) << 20
	ttMaxDoc    = int64(mustAtoi(getenv("TIKTOK_MAX_DOC_MB", "80"))) << 20
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

	// ---------- HTTP endpoints ----------
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	http.HandleFunc("/help", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "Endpoints:\n"+
			"GET /healthz -> ok\n"+
			"GET /help -> bantuan ini\n"+
			"POST/GET /send?to=62xxxx&text=... (Header: X-API-Key)\n")
	})
	http.HandleFunc("/send", func(w http.ResponseWriter, r *http.Request) {
		// Auth via X-API-Key (opsional tapi direkomendasikan)
		if sendAPIKey != "" {
			if r.Header.Get("X-API-Key") != sendAPIKey {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
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
		if err := sendText(client, destJID(j), text); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Write([]byte("sent"))
	})

	// ---------- Server listen: PORT dari env (fallback 7860) ----------
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

	// ========== Command registry sederhana ==========
	if cmd, _, ok := parseCommand(userText); ok { // args diabaikan
		switch cmd {
		case "help":
			_ = sendText(client, destJID(to), "Perintah:\n• !help — bantuan\n• Kirim link TikTok: bot kirim video langsung + link audio; slide akan dikirim sebagai gambar.")
			return
		case "ping":
			_ = sendText(client, destJID(to), "pong")
			return
		}
		_ = sendText(client, destJID(to), "Perintah tidak dikenal. Ketik !help")
		return
	}

	// — TikTok auto (pakai TikWM saja) —
	if urls := dl.DetectTikTokURLs(userText); len(urls) > 0 {
		videoURL, audioURL, images, err := getTikTokFromTikwm(httpClient, urls)
		if err != nil {
			log.Println("tikwm:", err)
			_ = sendText(client, destJID(to), "Maaf, gagal mengambil media TikTok. Coba kirim lagi ya.")
			return
		}
		dst := destJID(to)

		// === SLIDES ===
		if len(images) > 0 {
			total := len(images)
			if ttMaxSlides > 0 && total > ttMaxSlides {
				total = ttMaxSlides
			}
			for i := 0; i < total; i++ {
				imgURL := images[i]
				// coba HEAD untuk cek ukuran
				size, ctype, _ := headInfo(imgURL)
				// jika terlalu besar untuk image, tapi masih muat dokumen -> kirim dokumen
				if size > 0 && ttMaxImage > 0 && size > ttMaxImage && (ttMaxDoc <= 0 || size <= ttMaxDoc) {
					data, mime, err := downloadBytes(imgURL, ttMaxDoc)
					if err != nil {
						log.Printf("download (doc) image slide %d error: %v", i+1, err)
						continue
					}
					if mime == "" { mime = ctype }
					if mime == "" { mime = "image/jpeg" }
					caption := fmt.Sprintf("TikTok 🖼️ slide %d/%d (dokumen)", i+1, total)
					if err := sendDocument(client, dst, data, mime, fmt.Sprintf("slide_%d.jpg", i+1), caption); err != nil {
						log.Printf("send document slide %d error: %v", i+1, err)
					}
					continue
				}
				// normal path: kirim sebagai foto
				data, mime, err := downloadBytes(imgURL, ttMaxImage)
				if err != nil {
					log.Printf("download image slide %d error: %v", i+1, err)
					continue
				}
				if mime == "" { mime = "image/jpeg" }
				caption := fmt.Sprintf("TikTok 🖼️ slide %d/%d", i+1, total)
				if err := sendImage(client, dst, data, mime, caption); err != nil {
					log.Printf("send image slide %d error: %v", i+1, err)
				}
			}
			// kirim link audio jika ada
			if strings.TrimSpace(audioURL) != "" {
				_ = sendText(client, dst, "🔊 Audio: "+audioURL)
			}
			return
		}

		// === VIDEO ===
		if strings.TrimSpace(videoURL) != "" {
			// cek ukuran via HEAD (kalau tersedia)
			size, ctype, _ := headInfo(videoURL)

			// fallback: kalau > batas video tapi <= batas dokumen, kirim sebagai dokumen
			if size > 0 && ttMaxVideo > 0 && size > ttMaxVideo && (ttMaxDoc <= 0 || size <= ttMaxDoc) {
				data, mime, err := downloadBytes(videoURL, ttMaxDoc)
				if err != nil {
					log.Printf("download (doc) video error: %v", err)
				} else {
					if mime == "" { mime = ctype }
					if mime == "" { mime = "video/mp4" }
					if err := sendDocument(client, dst, data, mime, "tiktok.mp4", "TikTok 🎬 (dokumen)"); err == nil {
						// setelah video terkirim, kirim link audio (bila ada)
						if strings.TrimSpace(audioURL) != "" {
							_ = sendText(client, dst, "🔊 Audio: "+audioURL)
						}
						return
					}
					log.Printf("send document error: %v", err)
				}
			}

			// normal path: kirim sebagai video (batas ttMaxVideo)
			data, mime, err := downloadBytes(videoURL, ttMaxVideo)
			if err != nil {
				log.Printf("download video error: %v", err)
			} else {
				if mime == "" { mime = "video/mp4" }
				if err := sendVideo(client, dst, data, mime, "TikTok 🎬"); err == nil {
					// setelah video terkirim, kirim link audio (bila ada)
					if strings.TrimSpace(audioURL) != "" {
						_ = sendText(client, dst, "🔊 Audio: "+audioURL)
					}
					return
				}
				log.Printf("send video error: %v", err)
			}
		}

		// === AUDIO ONLY (jarang, tapi antisipasi) ===
		if strings.TrimSpace(audioURL) != "" {
			_ = sendText(client, dst, "🔊 Audio: "+audioURL)
			return
		}

		// tidak ada yang bisa dikirim
		_ = sendText(client, dst, "Maaf, tidak menemukan media valid dari TikTok.")
		return
	}

	// — Mode MANUAL (grup) —
	if isGroup && mode == "MANUAL" {
		low := strings.ToLower(userText)
		if trigger == "" { trigger = "elaina" }

		found := false
		pos := -1

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

	// — Blue Archive —
	if anime.IsBARequest(userText) {
		if len(baLinks) == 0 {
			links, err := anime.LoadLinks(context.Background(), baLocalPath, baURL)
			if err != nil {
				log.Println("BA LoadLinks error:", err)
				_ = sendText(client, destJID(to), "Maaf, daftar gambar Blue Archive belum tersedia.")
				return
			}
			baLinks = links
			log.Printf("BA links loaded: %d (url=%v local=%s)", len(baLinks), baURL != "", baLocalPath)
		}
		if err := anime.SendRandomImage(context.Background(), client, destJID(to), baLinks); err != nil {
			log.Println("BA SendRandomImage error:", err)
			_ = sendText(client, destJID(to), "Gagal mengirim gambar Blue Archive.")
		}
		return
	}

	// — Apakah user minta VN? —
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

	// persona + info developer
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
	reply, err := askGemini(system, userText) // rotasi API key otomatis
	if err != nil {
		log.Printf("askGemini error: %v", err)
		reply = "Ups, Elaina lagi tersandung jaringan. Coba lagi ya ✨"
	}
	log.Printf("askGemini: got reply (len=%d)", len(reply))

	// VN → batasi kata + synthesize → kirim audio
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

	// default kirim teks
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

// ---------- ElevenLabs TTS ----------
func elevenTTS(text, voiceID, mime string) ([]byte, string, error) {
	if mime == "" { mime = "audio/ogg;codecs=opus" }
	mime = strings.ReplaceAll(mime, " ", "")

	url := "https://api.elevenlabs.io/v1/text-to-speech/" + voiceID
	reqBody := map[string]any{ "text": text }
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

// Kirim video
func sendVideo(client *whatsmeow.Client, to types.JID, video []byte, mime, caption string) error {
	up, err := client.Upload(context.Background(), video, whatsmeow.MediaVideo)
	if err != nil { return err }
	msg := &waProto.Message{
		VideoMessage: &waProto.VideoMessage{
			URL:           proto.String(up.URL),
			DirectPath:    proto.String(up.DirectPath),
			MediaKey:      up.MediaKey,
			FileEncSHA256: up.FileEncSHA256,
			FileSHA256:    up.FileSHA256,
			FileLength:    proto.Uint64(uint64(len(video))),
			Mimetype:      proto.String(mime),
			Caption:       proto.String(strings.TrimSpace(caption)),
		},
	}
	_, err = client.SendMessage(context.Background(), to, msg)
	if err != nil && strings.Contains(err.Error(), "479") {
		time.Sleep(2 * time.Second)
		_, err = client.SendMessage(context.Background(), to, msg)
	}
	return err
}

// Kirim gambar (untuk TikTok slide)
func sendImage(client *whatsmeow.Client, to types.JID, image []byte, mime, caption string) error {
	up, err := client.Upload(context.Background(), image, whatsmeow.MediaImage)
	if err != nil { return err }
	msg := &waProto.Message{
		ImageMessage: &waProto.ImageMessage{
			URL:           proto.String(up.URL),
			DirectPath:    proto.String(up.DirectPath),
			MediaKey:      up.MediaKey,
			FileEncSHA256: up.FileEncSHA256,
			FileSHA256:    up.FileSHA256,
			FileLength:    proto.Uint64(uint64(len(image))),
			Mimetype:      proto.String(mime),
			Caption:       proto.String(strings.TrimSpace(caption)),
		},
	}
	_, err = client.SendMessage(context.Background(), to, msg)
	if err != nil && strings.Contains(err.Error(), "479") {
		time.Sleep(2 * time.Second)
		_, err = client.SendMessage(context.Background(), to, msg)
	}
	return err
}

// Kirim sebagai dokumen (fallback untuk file besar)
func sendDocument(client *whatsmeow.Client, to types.JID, data []byte, mime, filename, caption string) error {
	up, err := client.Upload(context.Background(), data, whatsmeow.MediaDocument)
	if err != nil { return err }
	if filename == "" { filename = "file" }
	if mime == "" { mime = "application/octet-stream" }

	msg := &waProto.Message{
		DocumentMessage: &waProto.DocumentMessage{
			URL:           proto.String(up.URL),
			DirectPath:    proto.String(up.DirectPath),
			MediaKey:      up.MediaKey,
			FileEncSHA256: up.FileEncSHA256,
			FileSHA256:    up.FileSHA256,
			FileLength:    proto.Uint64(uint64(len(data))),
			Mimetype:      proto.String(mime),
			Title:         proto.String(filename),
			FileName:      proto.String(filename),
			Caption:       proto.String(strings.TrimSpace(caption)),
		},
	}
	_, err = client.SendMessage(context.Background(), to, msg)
	if err != nil && strings.Contains(err.Error(), "479") {
		time.Sleep(2 * time.Second)
		_, err = client.SendMessage(context.Background(), to, msg)
	}
	return err
}

// downloader generic dengan batas ukuran
func downloadBytes(u string, max int64) ([]byte, string, error) {
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	req.Header.Set("User-Agent", "wa-elaina-bot/1.0")
	resp, err := httpClient.Do(req)
	if err != nil { return nil, "", err }
	defer resp.Body.Close()

	ct := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if max > 0 && resp.ContentLength > 0 && resp.ContentLength > max {
		return nil, ct, fmt.Errorf("file too large: %d > %d", resp.ContentLength, max)
	}
	var reader io.Reader = resp.Body
	if max > 0 {
		reader = io.LimitReader(resp.Body, max+1)
	}
	data, err := io.ReadAll(reader)
	if err != nil { return nil, ct, err }
	if max > 0 && int64(len(data)) > max {
		return nil, ct, fmt.Errorf("file too large after read: %d > %d", len(data), max)
	}
	if resp.StatusCode >= 300 {
		return nil, ct, fmt.Errorf("http %s", resp.Status)
	}
	return data, ct, nil
}

// HEAD info untuk dapat Content-Length & Content-Type (kalau disediakan server)
func headInfo(u string) (size int64, ctype string, err error) {
	req, _ := http.NewRequest(http.MethodHead, u, nil)
	req.Header.Set("User-Agent", "wa-elaina-bot/1.0")
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()

	cl := resp.Header.Get("Content-Length")
	if cl != "" {
		if n, e := strconv.ParseInt(strings.TrimSpace(cl), 10, 64); e == nil && n > 0 {
			size = n
		}
	}
	ctype = strings.TrimSpace(resp.Header.Get("Content-Type"))
	return size, ctype, nil
}

// ========== Command parsing ==========
func parseCommand(s string) (cmd, args string, ok bool) {
	trim := strings.TrimSpace(s)
	if trim == "" { return "", "", false }
	if !strings.HasPrefix(trim, "!") { return "", "", false }
	trim = strings.TrimPrefix(trim, "!")
	parts := strings.Fields(trim)
	if len(parts) == 0 { return "", "", false }
	cmd = strings.ToLower(parts[0])
	args = strings.TrimSpace(strings.TrimPrefix(trim, parts[0]))
	return cmd, args, true
}

// ========== Helpers umum ==========
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

func minf(a, b float64) float64 { if a < b { return a }; return b }

// Rate limit: token bucket per menit
func allow(key string) bool {
	now := time.Now()
	rlMu.Lock()
	defer rlMu.Unlock()

	b, ok := rlBuckets[key]
	if !ok {
		b = &bucket{tokens: float64(rlCap), lastRefill: now}
		rlBuckets[key] = b
	}
	// refill
	elapsed := now.Sub(b.lastRefill).Minutes()
	if elapsed > 0 {
		b.tokens = minf(float64(rlCap), b.tokens + elapsed*float64(rlCap))
		b.lastRefill = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens -= 1
	return true
}

func clientIP(r *http.Request) string {
	// respect X-Forwarded-For jika ada
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		ip := strings.TrimSpace(parts[0])
		if ip != "" { return "ip:" + ip }
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil { return "ip:" + r.RemoteAddr }
	return "ip:" + host
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

/* =========================
   TikTok via TikWM (tanpa Vreden)
   ========================= */

type tikwmResp struct {
	Data struct {
		Play   string   `json:"play"`   // mp4 (no-wm)
		Music  string   `json:"music"`  // mp3 url
		Images []string `json:"images"` // slide photos (optional)
		// Type bisa ada, tapi tidak kita andalkan
	} `json:"data"`
}

func getTikTokFromTikwm(client *http.Client, urls []string) (videoURL, audioURL string, images []string, err error) {
	if client == nil { client = &http.Client{Timeout: 30 * time.Second} }

	// ambil URL pertama yang non-blank
	raw := ""
	for _, u := range urls {
		if strings.TrimSpace(u) != "" { raw = u; break }
	}
	if raw == "" {
		return "", "", nil, fmt.Errorf("tidak ada url tiktok valid")
	}

	api := "https://www.tikwm.com/api/?url=" + url.QueryEscape(strings.TrimSpace(raw))
	req, _ := http.NewRequest(http.MethodGet, api, nil)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "wa-elaina-bot/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return "", "", nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", "", nil, fmt.Errorf("tikwm HTTP %d: %s", resp.StatusCode, string(body))
	}
	var data tikwmResp
	if err := json.Unmarshal(body, &data); err != nil {
		return "", "", nil, err
	}
	v := strings.TrimSpace(data.Data.Play)
	a := strings.TrimSpace(data.Data.Music)
	imgs := data.Data.Images
	return v, a, imgs, nil
}
