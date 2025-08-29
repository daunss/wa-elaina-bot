package baimg

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	"wa-elaina/internal/config"
	"wa-elaina/internal/wa"
)

type Handler struct {
	index map[string][]string
	re1   *regexp.Regexp // ^ba:\s*(.+)$
	re2   *regexp.Regexp // (ba|blue\s*archive)\s+(gambar|img|foto)\s+(.+)
}

func New(cfg config.Config) *Handler {
	h := &Handler{
		index: map[string][]string{},
		re1:   regexp.MustCompile(`(?i)^ba:\s*(.+)$`),
		re2:   regexp.MustCompile(`(?i)^(?:ba|blue\s*archive)\s+(?:gambar|img|foto)\s+(.+)$`),
	}
	_ = h.loadIndex(cfg.BALinksLocal, cfg.BALinksURL)
	return h
}

// Index format fleksibel:
// a) {"hoshino":["https://...","..."], "ako":[...]}
// b) [{"name":"hoshino","urls":["https://..."]}, ...]
func (h *Handler) loadIndex(local, remote string) error {
	if local != "" {
		if err := h.readLocal(local); err == nil { return nil }
	}
	if remote != "" {
		if err := h.readRemote(remote); err == nil { return nil }
	}
	return errors.New("ba index not found")
}
func (h *Handler) readLocal(path string) error {
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil { return err }
	return h.parse(b)
}
func (h *Handler) readRemote(url string) error {
	c := &http.Client{ Timeout: 20 * time.Second }
	resp, err := c.Get(url)
	if err != nil { return err }
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return h.parse(b)
}
func (h *Handler) parse(b []byte) error {
	// try map
	var m map[string][]string
	if json.Unmarshal(b, &m) == nil && len(m) > 0 {
		h.index = m
		return nil
	}
	// try array
	var arr []struct {
		Name string   `json:"name"`
		URLs []string `json:"urls"`
	}
	if json.Unmarshal(b, &arr) == nil && len(arr) > 0 {
		out := map[string][]string{}
		for _, it := range arr {
			key := strings.ToLower(strings.TrimSpace(it.Name))
			if key == "" || len(it.URLs) == 0 { continue }
			out[key] = append(out[key], it.URLs...)
		}
		h.index = out
		return nil
	}
	return errors.New("ba index parse failed")
}

func (h *Handler) TryHandleText(ctx context.Context, client *whatsmeow.Client, m *events.Message, text string, isOwner bool) bool {
	name := ""
	if g := h.re1.FindStringSubmatch(text); len(g) == 2 {
		name = strings.ToLower(strings.TrimSpace(g[1]))
	} else if g := h.re2.FindStringSubmatch(text); len(g) == 2 {
		name = strings.ToLower(strings.TrimSpace(g[1]))
	}
	if name == "" { return false }

	urls := h.index[name]
	if len(urls) == 0 {
		_ = client.SendMessage(ctx, m.Info.Chat, wa.TextMsg("Maaf, karakter itu belum ada di index BA-ku."))
		return true
	}

	dest := wa.DestJID(m.Info.Chat)
	// kirim maksimal 3 gambar
	max := 3
	if len(urls) < max { max = len(urls) }
	for i := 0; i < max; i++ {
		_ = wa.SendImageURL(client, dest, urls[i], "BA: "+name)
	}
	return true
}
