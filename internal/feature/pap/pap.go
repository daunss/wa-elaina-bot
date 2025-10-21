package pap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types/events"
	pbf "google.golang.org/protobuf/proto"

	"wa-elaina/internal/config"
)

type Handler struct {
	urls    []string
	rePap   *regexp.Regexp
	httpc   *http.Client
	caption string

	mu  sync.Mutex
	rnd *rand.Rand
}

func New(cfg config.Config) *Handler {
	urls, err := loadLinks(cfg.PapLinksPath)
	if err != nil {
		log.Printf("pap: gagal memuat daftar link dari %q: %v", cfg.PapLinksPath, err)
	}

	botName := strings.TrimSpace(cfg.BotName)
	if botName == "" {
		botName = "Bot"
	}

	return &Handler{
		urls:    urls,
		rePap:   regexp.MustCompile(`(?i)\bpap\b`),
		httpc:   &http.Client{Timeout: 25 * time.Second},
		caption: "Pap dari " + botName,
		rnd:     rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func loadLinks(path string) ([]string, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("pap links path kosong")
	}

	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, err
	}

	var raw []string
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, err
	}

	var urls []string
	for _, u := range raw {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
			urls = append(urls, u)
		}
	}

	if len(urls) == 0 {
		return nil, errors.New("pap links kosong")
	}
	return urls, nil
}

func (h *Handler) TryHandle(client *whatsmeow.Client, m *events.Message, text string) bool {
	if len(h.urls) == 0 {
		return false
	}

	if !h.rePap.MatchString(text) {
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		url, idx := h.randomURL()
		if url == "" {
			break
		}
		if err := h.sendPap(ctx, client, m, url); err != nil {
			lastErr = err
			log.Printf("pap: gagal mengirim pap dari %s (attempt %d): %v", url, attempt+1, err)
			h.dropURL(idx)
			continue
		}
		return true
	}

	if lastErr != nil {
		replyText(ctx, client, m, "Maaf, aku belum bisa kirim pap sekarang.")
	}
	return true
}

func (h *Handler) randomURL() (string, int) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if len(h.urls) == 0 {
		return "", -1
	}
	if len(h.urls) == 1 {
		return h.urls[0], 0
	}
	idx := h.rnd.Intn(len(h.urls))
	return h.urls[idx], idx
}

func (h *Handler) dropURL(idx int) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if idx < 0 || idx >= len(h.urls) {
		return
	}
	url := h.urls[idx]
	h.urls = append(h.urls[:idx], h.urls[idx+1:]...)
	log.Printf("pap: menghapus link %s dari daftar setelah gagal diakses", url)
}

func (h *Handler) sendPap(ctx context.Context, client *whatsmeow.Client, m *events.Message, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	resp, err := h.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status download pap %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	upload, err := client.Upload(ctx, data, whatsmeow.MediaImage)
	if err != nil {
		return err
	}

	ci := &waProto.ContextInfo{
		StanzaID:      pbf.String(m.Info.ID),
		QuotedMessage: m.Message,
		Participant:   pbf.String(m.Info.Sender.String()),
		RemoteJID:     pbf.String(m.Info.Chat.String()),
	}

	_, err = client.SendMessage(ctx, m.Info.Chat, &waProto.Message{
		ImageMessage: &waProto.ImageMessage{
			URL:           pbf.String(upload.URL),
			DirectPath:    pbf.String(upload.DirectPath),
			MediaKey:      upload.MediaKey,
			FileEncSHA256: upload.FileEncSHA256,
			FileSHA256:    upload.FileSHA256,
			FileLength:    pbf.Uint64(uint64(len(data))),
			Mimetype:      pbf.String(resp.Header.Get("Content-Type")),
			Caption:       pbf.String(h.caption),
			ContextInfo:   ci,
		},
	})
	return err
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
