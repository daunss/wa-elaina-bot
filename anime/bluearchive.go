package anime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"google.golang.org/protobuf/proto"
)

// Trigger sederhana untuk permintaan gambar Blue Archive
func IsBARequest(text string) bool {
	t := strings.ToLower(strings.TrimSpace(text))
	return strings.Contains(t, "blue archive") ||
		strings.Contains(t, "random ba") ||
		strings.Contains(t, "waifu ba") ||
		t == "ba" || t == "kirim ba"
}

// Muat daftar link gambar.
func LoadLinks(ctx context.Context, localPath, remoteURL string) ([]string, error) {
	var links []string

	// 1) Coba remote JSON (opsional)
	if strings.TrimSpace(remoteURL) != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, remoteURL, nil)
		if err == nil {
			client := &http.Client{Timeout: 15 * time.Second}
			resp, err := client.Do(req)
			if err == nil {
				defer resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					if err := json.NewDecoder(resp.Body).Decode(&links); err == nil && len(links) > 0 {
						return sanitizeLinks(links), nil
					}
				}
			}
		}
	}

	// 2) Fallback: file lokal
	if strings.TrimSpace(localPath) == "" {
		return nil, errors.New("LoadLinks: localPath kosong dan remoteURL gagal")
	}
	data, err := os.ReadFile(localPath)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &links); err != nil {
		return nil, err
	}
	links = sanitizeLinks(links)
	if len(links) == 0 {
		return nil, errors.New("LoadLinks: links kosong setelah sanitasi")
	}
	return links, nil
}

// Kirim 1 gambar acak dari daftar links.
func SendRandomImage(ctx context.Context, client *whatsmeow.Client, to types.JID, links []string) error {
	if len(links) == 0 {
		return errors.New("SendRandomImage: list kosong")
	}
	rand.Seed(time.Now().UnixNano())
	link := links[rand.Intn(len(links))]

	// Unduh gambar
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return err
	}
	httpc := &http.Client{Timeout: 25 * time.Second}
	resp, err := httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New("download gagal: " + resp.Status)
	}
	imgBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	// Deteksi MIME
	mime := resp.Header.Get("Content-Type")
	if mime == "" || !strings.HasPrefix(mime, "image/") {
		mime = http.DetectContentType(imgBytes)
		if !strings.HasPrefix(mime, "image/") {
			mime = "image/jpeg"
		}
	}

	// Upload ke WhatsApp, lalu kirim
	up, err := client.Upload(ctx, imgBytes, whatsmeow.MediaImage)
	if err != nil {
		return err
	}
	_, err = client.SendMessage(ctx, to, &waProto.Message{
		ImageMessage: &waProto.ImageMessage{
			URL:           proto.String(up.URL),          
			DirectPath:    proto.String(up.DirectPath),
			MediaKey:      up.MediaKey,
			Mimetype:      proto.String(mime),
			FileEncSHA256: up.FileEncSHA256,              
			FileSHA256:    up.FileSHA256,                
			FileLength:    proto.Uint64(uint64(len(imgBytes))),
			Caption:       proto.String("Blue Archive 💙"),
		},
	})
	return err
}

// ------- helpers -------

func sanitizeLinks(in []string) []string {
	out := make([]string, 0, len(in))
	for _, u := range in {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
			out = append(out, u)
		}
	}
	return out
}
