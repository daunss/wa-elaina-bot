package config

import (
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

type Config struct {
	// General
	SessionDB string
	BotName   string
	Mode      string // MANUAL / PROD / etc.
	Trigger   string
	Port      string

	// Auth & rate limit
	SendAPIKey      string
	SendRatePerMin  int

	// Gemini
	GeminiKeys []string

	// ElevenLabs
	ElevenAPIKey string
	ElevenVoice  string
	ElevenMime   string
	VNMaxWords   int

	// Blue Archive
	BALinksURL   string
	BALinksLocal string

	// TikTok limits (bytes)
	TTMaxVideo  int64
	TTMaxImage  int64
	TTMaxDoc    int64
	TTMaxSlides int
}

// Load memuat konfigurasi dari .env + environment, dengan default aman.
func Load() Config {
	_ = godotenv.Load()

	cfg := Config{
		SessionDB:      getenv("SESSION_PATH", "session.db"),
		BotName:        getenv("BOT_NAME", "Elaina"),
		Mode:           strings.ToUpper(getenv("MODE", "MANUAL")),
		Trigger:        strings.ToLower(getenv("TRIGGER", "elaina")),
		Port:           getenv("PORT", "7860"),
		SendAPIKey:     os.Getenv("SEND_API_KEY"),
		SendRatePerMin: mustAtoi(getenv("SEND_RATE_PER_MIN", "10")),
		ElevenAPIKey:   os.Getenv("ELEVENLABS_API_KEY"),
		ElevenVoice:    getenv("ELEVENLABS_VOICE_ID", "iWydkXKoiVtvdn4vLKp9"),
		ElevenMime:     getenv("ELEVENLABS_FORMAT", "audio/ogg;codecs=opus"),
		VNMaxWords:     mustAtoi(getenv("VN_MAX_WORDS", "80")),
		BALinksURL:     getenv("BA_LINKS_URL", ""),
		BALinksLocal:   getenv("BA_LINKS_LOCAL", "anime/bluearchive_links.json"),
		TTMaxVideo:     int64(mustAtoi(getenv("TIKTOK_MAX_VIDEO_MB", "50"))) << 20,
		TTMaxImage:     int64(mustAtoi(getenv("TIKTOK_MAX_IMAGE_MB", "5"))) << 20,
		TTMaxDoc:       int64(mustAtoi(getenv("TIKTOK_MAX_DOC_MB", "80"))) << 20,
		TTMaxSlides:    mustAtoi(getenv("TIKTOK_MAX_SLIDES", "10")),
	}

	// Gemini keys: GEMINI_API_KEYS (comma) atau GEMINI_API_KEY (single)
	keysEnv := os.Getenv("GEMINI_API_KEYS")
	if keysEnv == "" {
		keysEnv = os.Getenv("GEMINI_API_KEY")
	}
	for _, part := range strings.Split(keysEnv, ",") {
		if k := strings.TrimSpace(part); k != "" {
			cfg.GeminiKeys = append(cfg.GeminiKeys, k)
		}
	}
	if len(cfg.GeminiKeys) == 0 {
		log.Fatal("Tidak ada GEMINI_API_KEYS/GEMINI_API_KEY di .env (boleh beberapa key dipisah koma).")
	}

	// Opsional enforce di PROD
	if cfg.Mode == "PROD" && cfg.SendAPIKey == "" {
		log.Println("[WARN] MODE=PROD tapi SEND_API_KEY kosong. /send akan terbuka tanpa auth!")
	}

	return cfg
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func mustAtoi(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n <= 0 {
		return 10
	}
	return n
}
