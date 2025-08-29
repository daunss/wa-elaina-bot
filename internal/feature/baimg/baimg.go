package baimg

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
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
	index map[string][]string
	reBA  *regexp.Regexp // ba / ba random
	reNat *regexp.Regexp // frasa natural: (kirim|minta).*blue archive
}

func New(cfg config.Config) *Handler {
	h := &Handler{
		index: map[string][]string{},
		reBA:  regexp.MustCompile(`(?i)^(?:ba)(?:\s+(?:random|img|gambar|foto))?\s*$`),
		reNat: regexp.MustCompile(`(?i)\b(kirim|minta|tolong|please).*(blue\s*archive)|\bblue\s*archive\b`),
	}
	_ = h.loadIndex(cfg.BALinksLocal, cfg.BALinksURL)
	return h
}

// Index fleksibel:
//  a) map[string][]string
//  b) [{"name":"hoshino","urls":["..."]}]
//  c) []string                 // format lama: daftar URL polos
func (h *Handler) loadIndex(local, remote string) error {
	if local != "" {
		if err := h.readLocal(local); err == nil {
			return nil
		}
	}
	if remote != "" {
		if err := h.readRemote(remote); err == nil {
			return nil
		}
	}
	return errors.New("ba index not found")
}

func (h *Handler) readLocal(path string) error {
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return err
	}
	return h.parse(b)
}

func (h *Handler) readRemote(url string) error {
	c := &http.Client{Timeout: 20 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return h.parse(b)
}

func (h *Handler) parse(b []byte) error {
	// a) map[name][]urls
	var m map[string][]string
	if json.Unmarshal(b, &m) == nil && len(m) > 0 {
		h.index = normalizeIndex(m)
		return nil
	}
	// b) array objek
	var arr []struct {
		Name string   `json:"name"`
		URLs []string `json:"urls"`
	}
	if json.Unmarshal(b, &arr) == nil && len(arr) > 0 {
		out := map[string][]string{}
		for _, it := range arr {
			key := strings.ToLower(strings.TrimSpace(it.Name))
			if key == "" || len(it.URLs) == 0 {
				continue
			}
			out[key] = append(out[key], it.URLs...)
		}
		h.index = normalizeIndex(out)
		return nil
	}
	// c) flat []string
	var flat []string
	if json.Unmarshal(b, &flat) == nil && len(flat) > 0 {
		h.index = map[string][]string{"__all__": sanitize(flat)}
		return nil
	}
	return errors.New("ba index parse failed")
}

func normalizeIndex(in map[string][]string) map[string][]string {
	out := make(map[string][]string, len(in))
	for k, v := range in {
		key := strings.ToLower(strings.TrimSpace(k))
		if key == "" {
			continue
		}
		out[key] = sanitize(v)
	}
	return out
}

func sanitize(in []string) []string {
	res := make([]string, 0, len(in))
	for _, u := range in {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
			res = append(res, u)
		}
	}
	return res
}

func (h *Handler) TryHandleText(ctx context.Context, client *whatsmeow.Client, m *events.Message, text string, _ bool) bool {
	low := strings.ToLower(strings.TrimSpace(text))

	// 1) Perintah pendek
	if h.reBA.MatchString(low) {
		return h.sendRandom(ctx, client, m)
	}

	// 2) Frasa natural berisi "blue archive"
	if h.reNat.MatchString(low) || strings.Contains(low, "blue archive") {
		return h.sendRandom(ctx, client, m)
	}

	// 3) "ba: <nama>" (opsional — tetap dukung)
	if strings.HasPrefix(low, "ba:") {
		name := strings.TrimSpace(strings.TrimPrefix(low, "ba:"))
		return h.sendByName(ctx, client, m, name)
	}
	return false
}

// ---- helpers kirim ----

func (h *Handler) flattenAll() []string {
	if all := h.index["__all__"]; len(all) > 0 {
		return all
	}
	// jika tak ada "__all__", gabungkan semua
	res := make([]string, 0, 64)
	for _, v := range h.index {
		res = append(res, v...)
	}
	return res
}

func (h *Handler) sendRandom(ctx context.Context, client *whatsmeow.Client, m *events.Message) bool {
	urls := h.flattenAll()
	if len(urls) == 0 {
		replyText(ctx, client, m, "Index Blue Archive kosong / gagal dimuat.")
		return true
	}
	rand.Seed(time.Now().UnixNano())
	n := 2
	if n > len(urls) {
		n = len(urls)
	}
	perm := rand.Perm(len(urls))[:n]
	for _, i := range perm {
		_ = sendImageURLReply(ctx, client, m, urls[i], "Blue Archive 💙")
	}
	return true
}

func (h *Handler) sendByName(ctx context.Context, client *whatsmeow.Client, m *events.Message, name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return false
	}
	urls := h.index[name]
	if len(urls) == 0 {
		replyText(ctx, client, m, "Maaf, karakter itu belum ada di index BA-ku.")
		return true
	}
	max := 3
	if len(urls) < max {
		max = len(urls)
	}
	for i := 0; i < max; i++ {
		_ = sendImageURLReply(ctx, client, m, urls[i], "BA: "+name)
	}
	return true
}

func replyText(ctx context.Context, client *whatsmeow.Client, m *events.Message, msg string) {
	ci := &waProto.ContextInfo{
		StanzaID:       pbf.String(m.Info.ID),
		QuotedMessage:  m.Message,
		Participant:    pbf.String(m.Info.Sender.String()),
		RemoteJID:      pbf.String(m.Info.Chat.String()),
	}
	_, _ = client.SendMessage(ctx, m.Info.Chat, &waProto.Message{
		ExtendedTextMessage: &waProto.ExtendedTextMessage{
			Text:        pbf.String(msg),
			ContextInfo: ci,
		},
	})
}

func sendImageURLReply(ctx context.Context, client *whatsmeow.Client, m *events.Message, url, caption string) error {
	// download
	httpc := &http.Client{Timeout: 25 * time.Second}
	resp, err := httpc.Get(url)
	if err != nil {
		replyText(ctx, client, m, "Gagal mengunduh gambar BA.")
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		replyText(ctx, client, m, "Gagal mengunduh gambar BA: "+resp.Status)
		return errors.New("bad status")
	}
	b, _ := io.ReadAll(resp.Body)
	// upload
	up, err := client.Upload(ctx, b, whatsmeow.MediaImage)
	if err != nil {
		replyText(ctx, client, m, "Gagal mengunggah gambar BA.")
		return err
	}
	ci := &waProto.ContextInfo{
		StanzaID:       pbf.String(m.Info.ID),
		QuotedMessage:  m.Message,
		Participant:    pbf.String(m.Info.Sender.String()),
		RemoteJID:      pbf.String(m.Info.Chat.String()),
	}
	_, err = client.SendMessage(ctx, m.Info.Chat, &waProto.Message{
		ImageMessage: &waProto.ImageMessage{
			URL:           pbf.String(up.URL),
			DirectPath:    pbf.String(up.DirectPath),
			MediaKey:      up.MediaKey,
			Mimetype:      pbf.String(resp.Header.Get("Content-Type")),
			FileEncSHA256: up.FileEncSHA256,
			FileSHA256:    up.FileSHA256,
			FileLength:    pbf.Uint64(uint64(len(b))),
			Caption:       pbf.String(caption),
			ContextInfo:   ci,
		},
	})
	return err
}
