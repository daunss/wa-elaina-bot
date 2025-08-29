// main.go
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
	reMention  = regexp.MustCompile(`(?i)\b(elaina|eleina|elena|elina|ela?ina)\b`)
	reTrigger  *regexp.Regexp

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

	// Owner
	ownerJID    *types.JID  // nomor: 62xxxxxxxxxx@s.whatsapp.net
	ownerLID    *types.JID  // privacy JID: 1039.........@lid
	ownerExtras []types.JID // JID tambahan (comma-separated)
	ownerTag    string
	ownerDigits string // digits-only dari nomor owner (untuk toleransi LID)
	ownerDebug  bool
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

	// ==== OWNER from ENV ====
	// 1) Nomor (JID standar)
	if v := strings.TrimSpace(os.Getenv("OWNER_JID")); v != "" {
		if j, err := types.ParseJID(v); err == nil {
			ownerJID = &j
		}
	} else if num := strings.TrimSpace(os.Getenv("OWNER_NUMBER")); num != "" {
		num = strings.NewReplacer("+", "", " ", "", "-", "").Replace(num)
		if j, err := types.ParseJID(num + "@s.whatsapp.net"); err == nil {
			ownerJID = &j
		}
	}
	// 2) LID (privacy JID), contoh: 103929669005392@lid
	if v := strings.TrimSpace(os.Getenv("OWNER_LID")); v != "" {
		if j, err := types.ParseJID(v); err == nil {
			ownerLID = &j
		}
	}
	// 3) JID tambahan (opsional), pisahkan dengan koma
	if xs := strings.TrimSpace(os.Getenv("OWNER_IDS")); xs != "" {
		for _, s := range strings.Split(xs, ",") {
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			if j, err := types.ParseJID(s); err == nil {
				ownerExtras = append(ownerExtras, j)
			}
		}
	}

	ownerTag = strings.TrimSpace(os.Getenv("OWNER_TAG"))
	if ownerTag == "" {
		ownerTag = "owner tercinta/sayang"
	}
	ownerDebug = strings.EqualFold(strings.TrimSpace(os.Getenv("OWNER_DEBUG")), "true")

	// angka yang akan dicocokkan (untuk LID/variasi lain)
	ownerDigits = strings.TrimSpace(os.Getenv("OWNER_MATCH"))
	if ownerDigits == "" && ownerJID != nil {
		ownerDigits = digitsOnly(ownerJID.User)
	} else {
		ownerDigits = digitsOnly(ownerDigits)
	}

	// Log verifikasi
	if ownerJID != nil {
		log.Printf("Owner JID (number): %s", ownerJID.String())
	}
	if ownerLID != nil {
		log.Printf("Owner JID (LID)   : %s", ownerLID.String())
	}
	if len(ownerExtras) > 0 {
		var ss []string
		for _, j := range ownerExtras {
			ss = append(ss, j.String())
		}
		log.Printf("Owner extras       : %s", strings.Join(ss, ", "))
	}
	if ownerDigits != "" {
		log.Printf("Owner digits match : %s", ownerDigits)
	}
	if ownerJID == nil && ownerLID == nil && len(ownerExtras) == 0 {
		log.Printf("Owner IDs NOT set. Set OWNER_JID/OWNER_LID/OWNER_IDS")
	}
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

	// HTTP API
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
	isOwner := isSenderOwnerFromInfo(msg.Info)
	debugOwner(msg.Info, isOwner)

	// ==== IMAGE (butuh trigger di caption)
	if img := msg.Message.GetImageMessage(); img != nil {
		caption := strings.TrimSpace(img.GetCaption())
		if !reTrigger.MatchString(caption) {
			return
		}
		go func(m *events.Message) {
			defer guard()
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			data, err := client.Download(ctx, img)
			if err != nil {
				_ = replyTextMention(client, to, "Maaf, gagal mengunduh gambar 😔", m, nil)
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
			text, mentions := decorateOwnerMention(isOwner, reply)
			_ = replyTextMention(client, to, text, m, mentions)
		}(msg)
		return
	}

	// ==== VN (balas hanya jika sebut "elaina")
	if aud := msg.Message.GetAudioMessage(); aud != nil {
		go func(m *events.Message) {
			defer guard()
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			data, err := client.Download(ctx, aud)
			if err != nil {
				_ = replyTextMention(client, to, "Maaf, gagal mengambil voice note 😔", m, nil)
				return
			}
			tx, err := askGeminiTranscribe(
				`Transkripsikan audio berikut ke teks Bahasa Indonesia yang bersih dan mudah dibaca. Hanya kembalikan teks transkripnya.`,
				data, normalizeAudioMime(aud.GetMimetype()),
			)
			if err != nil || strings.TrimSpace(tx) == "" {
				_ = replyTextMention(client, to, "Aku nggak bisa dengar jelas VN-nya. Kirim ulang ya.", m, nil)
				return
			}
			if !reMention.MatchString(tx) {
				if strings.EqualFold(getenv("VN_DEBUG_TRANSCRIPT", "false"), "true") {
					_ = replyTextMention(client, to, "📝 Transkrip: "+limitWords(tx, 120)+`\n(sebut "Elaina" agar aku membalas)`, m, nil)
				}
				return
			}
			clean := strings.TrimSpace(reMention.ReplaceAllString(tx, ""))
			if clean == "" {
				clean = tx
			}
			system := `Perankan "Elaina", penyihir cerdas & hangat.
Gunakan orang pertama ("aku/ku") & panggil pengguna "kamu".
Gaya santai-sopan, playful secukupnya, emoji hemat.`
			reply, err := askGemini(system, clean)
			if err != nil || strings.TrimSpace(reply) == "" {
				reply = "Ups, Elaina lagi kelelahan mendengar. Coba lagi ya ✨"
			}
			if len(reply) > 3500 {
				reply = reply[:3500] + "…"
			}
			text, mentions := decorateOwnerMention(isOwner, reply)
			_ = replyTextMention(client, to, text, m, mentions)
		}(msg)
		return
	}

	// ==== TEKS (termasuk reply ke media)
	userText := extractText(msg)
	if userText == "" {
		return
	}

	// Commands
	if cmd, _, ok := parseCommand(userText); ok {
		switch cmd {
		case "help":
			_ = replyTextMention(client, to,
				"Perintah:\n• !help — bantuan\n• !whoami — lihat JID pengirim (buat setting owner)\n• Kirim link TikTok: bot kirim video + link audio; slide jadi gambar.\n• Kirim gambar + sebut '"+cfg.Trigger+"' — aku analisis & jawab.\n• Kirim VN dan sebut 'Elaina' — aku transkrip & jawab.", msg, nil)
			return
		case "ping":
			_ = replyTextMention(client, to, "pong", msg, nil)
			return
		case "whoami":
			info := msg.Info
			reply := "Sender: " + info.Sender.String() +
				"\nChat  : " + info.Chat.String() +
				"\nDigits(sender): " + digitsOnly(info.Sender.User) +
				"\nDigits(chat)  : " + digitsOnly(info.Chat.User)
			_ = replyTextMention(client, to, reply, msg, nil)
			return
		default:
			_ = replyTextMention(client, to, "Perintah tidak dikenal. Ketik !help", msg, nil)
			return
		}
	}

	// Reply ke media + trigger
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
					_ = replyTextMention(client, to, "Gagal mengambil gambar yang direply 😔", m, nil)
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
				text, mentions := decorateOwnerMention(isOwner, reply)
				_ = replyTextMention(client, to, text, m, mentions)
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
					_ = replyTextMention(client, to, "Gagal mengambil VN yang direply 😔", m, nil)
					return
				}
				tx, err := askGeminiTranscribe(`Transkripsikan audio ke teks Indonesia yang bersih.`, data, normalizeAudioMime(qaud.GetMimetype()))
				if err != nil || strings.TrimSpace(tx) == "" {
					_ = replyTextMention(client, to, "VN tidak jelas, kirim ulang ya.", m, nil)
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
				text, mentions := decorateOwnerMention(isOwner, reply)
				_ = replyTextMention(client, to, text, m, mentions)
			}(msg)
			return
		}
	}

	// TikTok
	if tiktokH.TryHandle(userText, to) {
		return
	}

	// Mode MANUAL (grup)
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

	// VN → TTS
	if wantVN {
		reply = limitWords(reply, cfg.VNMaxWords)
		if elAPIKey == "" {
			text, mentions := decorateOwnerMention(isOwner, "[VN off] "+reply)
			_ = replyTextMention(client, to, text, msg, mentions)
			return
		}
		audio, mime, err := elevenTTS(reply, elVoiceID, elMime)
		if err != nil {
			text, mentions := decorateOwnerMention(isOwner, reply)
			_ = replyTextMention(client, to, text, msg, mentions)
			return
		}
		dur := estimateSecondsFromText(reply)
		_ = sender.Audio(wa.DestJID(to), audio, mime, true, dur)
		return
	}

	if len(reply) > 3500 {
		reply = reply[:3500] + "…"
	}
	text, mentions := decorateOwnerMention(isOwner, reply)
	_ = replyTextMention(client, to, text, msg, mentions)
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

// ======== Owner detection & debug ========

func digitsOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func jidBareEqual(a, b types.JID) bool {
	return a.User == b.User && a.Server == b.Server
}

func isSenderOwnerFromInfo(info types.MessageInfo) bool {
	// 1) cocokkan JID nomor (bare JID)
	if ownerJID != nil && (jidBareEqual(info.Sender, *ownerJID) || jidBareEqual(info.Chat, *ownerJID)) {
		return true
	}
	// 2) cocokkan digit-only (nomor) untuk toleransi LID vs nomor
	if ownerDigits != "" {
		if digitsOnly(info.Sender.User) == ownerDigits || digitsOnly(info.Chat.User) == ownerDigits {
			return true
		}
	}
	// 3) cocokkan JID LID (exact)
	if ownerLID != nil && (jidBareEqual(info.Sender, *ownerLID) || jidBareEqual(info.Chat, *ownerLID)) {
		return true
	}
	// 4) cocokkan ke daftar tambahan (OWNER_IDS)
	for _, j := range ownerExtras {
		if jidBareEqual(info.Sender, j) || jidBareEqual(info.Chat, j) {
			return true
		}
	}
	return false
}

func debugOwner(info types.MessageInfo, got bool) {
	if !ownerDebug {
		return
	}
	log.Printf("[OWNER-DBG] sender=%s chat=%s digits(sender)=%s digits(chat)=%s => isOwner=%t",
		info.Sender.String(), info.Chat.String(), digitsOnly(info.Sender.User), digitsOnly(info.Chat.User), got)
}

// ======== Reply helpers (quote + mentions) ========

func replyTextMention(client *whatsmeow.Client, to types.JID, text string, quoted *events.Message, mentions []types.JID) error {
	if quoted == nil && len(mentions) == 0 {
		return sender.Text(wa.DestJID(to), text)
	}
	ci := &waProto.ContextInfo{}
	if quoted != nil {
		ci.StanzaID = pbf.String(quoted.Info.ID)
		ci.Participant = pbf.String(quoted.Info.Sender.String())
		ci.RemoteJID = pbf.String(quoted.Info.Chat.String())
		ci.QuotedMessage = quoted.Message
	}
	if len(mentions) > 0 {
		var ms []string
		for _, j := range mentions {
			ms = append(ms, j.String())
		}
		ci.MentionedJID = ms
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

func currentOwnerMentionJID() *types.JID {
	if ownerJID != nil {
		return ownerJID
	}
	if ownerLID != nil {
		return ownerLID
	}
	if len(ownerExtras) > 0 {
		return &ownerExtras[0]
	}
	return nil
}

func decorateOwnerMention(isOwner bool, base string) (string, []types.JID) {
	if !isOwner {
		return base, nil
	}
	j := currentOwnerMentionJID()
	if j == nil {
		return base, nil
	}
	prefix := "@" + j.User + " (" + ownerTag + ")\n"
	return prefix + base, []types.JID{*j}
}

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
				if rotateGeminiKey() {
					continue
				}
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

// ---------- Gemini API (transcribe audio) ----------

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
				if rotateGeminiKey() {
					continue
				}
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

// ---------- Gemini API (vision) ----------

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
				if rotateGeminiKey() {
					continue
				}
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
