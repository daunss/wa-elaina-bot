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
	"go.mau.fi/whatsmeow/types/events"

	"wa-elaina/internal/config"
	"wa-elaina/internal/wa"
)

type Handler struct {
	index map[string][]string
	re0   *regexp.Regexp // ^ba$ / ba random
	re1   *regexp.Regexp // ^ba:\s*(.+)$
	re2   *regexp.Regexp // (ba|blue\s*archive)\s+(gambar|img|foto)\s+(.+)
}

func New(cfg config.Config) *Handler {
	h := &Handler{
		index: map[string][]string{},
		re0:   regexp.MustCompile(`(?i)^(?:ba)(?:\s+(?:random|img|gambar|foto))?\s*$`),
		re1:   regexp.MustCompile(`(?i)^ba:\s*(.+)$`),
		re2:   regexp.MustCompile(`(?i)^(?:ba|blue\s*archive)\s+(?:gambar|img|foto)\s+(.+)$`),
	}
	_ = h.loadIndex(cfg.BALinksLocal, cfg.BALinksURL)
	return h
}

// Index format fleksibel:
// a) {"hoshino":["https://...","..."], "ako":[...]}
// b) [{"name":"hoshino","urls":["https://..."]}, ...]
// c) ["https://...","https://..."]                  <-- format lama (array string)
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
	// try map
	var m map[string][]string
	if json.Unmarshal(b, &m) == nil && len(m) > 0 {
		h.index = normalizeIndex(m)
		return nil
	}
	// try array of {name, urls}
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
	// try []string (format lama: daftar URL tanpa nama)
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

	// 1) "ba" / "ba random"
	if h.re0.MatchString(low) {
		urls := h.index["__all__"]
		if len(urls) == 0 {
			_, _ = client.SendMessage(ctx, m.Info.Chat, wa.TextMsg("Daftar BA (format lama) belum dimuat. Isi `anime/bluearchive_links.json` sebagai array URL atau set `BA_LINKS_LOCAL`."))
			return true
		}
		dest := wa.DestJID(m.Info.Chat)
		// kirim maksimal 3 random
		rand.Seed(time.Now().UnixNano())
		max := 3
		if max > len(urls) {
			max = len(urls)
		}
		perm := rand.Perm(len(urls))[:max]
		for _, i := range perm {
			_ = wa.SendImageURL(client, dest, urls[i], "Blue Archive 💙")
		}
		return true
	}

	// 2) "ba: <nama>" atau "ba gambar <nama>"
	var name string
	if g := h.re1.FindStringSubmatch(low); len(g) == 2 {
		name = strings.TrimSpace(g[1])
	} else if g := h.re2.FindStringSubmatch(low); len(g) == 2 {
		name = strings.TrimSpace(g[1])
	}
	if name == "" {
		return false
	}

	urls := h.index[name]
	if len(urls) == 0 {
		_, _ = client.SendMessage(ctx, m.Info.Chat, wa.TextMsg("Maaf, karakter itu belum ada di index BA-ku."))
		return true
	}

	dest := wa.DestJID(m.Info.Chat)
	// kirim maksimal 3 gambar berurutan
	max := 3
	if len(urls) < max {
		max = len(urls)
	}
	for i := 0; i < max; i++ {
		_ = wa.SendImageURL(client, dest, urls[i], "BA: "+name)
	}
	return true
}
