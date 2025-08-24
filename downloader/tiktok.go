package downloader

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// TikTok (TikWM only)
// =========================

// Regex luas untuk berbagai bentuk URL TikTok:
// - vt.tiktok.com/...
// - www.tiktok.com/@user/video/ID
// - m.tiktok.com/...
// - tiktok.com/v/..., dsb.
var reTikTok = regexp.MustCompile(`https?://(?:vt\.)?tiktok\.com/[^\s]+|https?://(?:www\.|m\.)?tiktok\.com/(?:@[A-Za-z0-9._-]+/video/\d+|v/[^\s]+|[^\s]+)`)

// DetectTikTokURLs mengekstrak semua URL TikTok dari teks.
func DetectTikTokURLs(text string) []string {
	return reTikTok.FindAllString(text, -1)
}

// ExpandTikTokURL mengikuti redirect vt.tiktok.com ke URL final.
// Tidak wajib dipakai, tapi membantu TikWM mengenali link pendek.
func ExpandTikTokURL(client *http.Client, raw string) string {
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	u := strings.TrimSpace(raw)

	// HEAD dulu (hemat bandwidth)
	req, _ := http.NewRequest(http.MethodHead, u, nil)
	req.Header.Set("User-Agent", "wa-elaina-bot/1.0")
	if resp, err := client.Do(req); err == nil && resp != nil && resp.Request != nil && resp.Request.URL != nil {
		resp.Body.Close()
		return resp.Request.URL.String()
	}

	// Fallback GET
	req2, _ := http.NewRequest(http.MethodGet, u, nil)
	req2.Header.Set("User-Agent", "wa-elaina-bot/1.0")
	if resp2, err2 := client.Do(req2); err2 == nil && resp2 != nil && resp2.Request != nil && resp2.Request.URL != nil {
		io.Copy(io.Discard, resp2.Body)
		resp2.Body.Close()
		return resp2.Request.URL.String()
	}

	return u
}

// Struktur respons minimal dari TikWM yang kita butuhkan.
type tikwmResp struct {
	Data struct {
		Play   string   `json:"play"`   // URL mp4 (no watermark)
		Music  string   `json:"music"`  // URL mp3
		Images []string `json:"images"` // Daftar URL slide (jika konten berupa foto)
	} `json:"data"`
}

// getFromTikwm memanggil API TikWM dan mengekstrak video/audio/images.
func getFromTikwm(client *http.Client, link string) (video, audio string, images []string, err error) {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	api := "https://www.tikwm.com/api/?url=" + url.QueryEscape(strings.TrimSpace(link))

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
	video = strings.TrimSpace(data.Data.Play)
	audio = strings.TrimSpace(data.Data.Music)
	images = data.Data.Images
	return
}

// GetTikTokFromTikwm menerima daftar URL, memilih yang pertama,
// (opsional) expand vt.tiktok.com, lalu memanggil TikWM.
// Mengembalikan video, audio, dan list images (untuk Slide).
func GetTikTokFromTikwm(client *http.Client, urls []string) (videoURL, audioURL string, images []string, err error) {
	raw := firstNonEmptyURL(urls)
	if raw == "" {
		return "", "", nil, errors.New("tidak ada url tiktok valid")
	}
	final := ExpandTikTokURL(client, raw)
	return getFromTikwm(client, final)
}

// GetTikTokMedia kompatibel ke belakang: hanya mengembalikan video & audio.
// (Implementasi sekarang berbasis TikWM saja.)
func GetTikTokMedia(client *http.Client, urls []string) (videoURL, audioURL string, err error) {
	v, a, _, e := GetTikTokFromTikwm(client, urls)
	return v, a, e
}

// HandleTikTok menyusun fallback jawaban teks berisi link video/audio,
// serta info jumlah slide (jika ada). Semua data berbasis TikWM.
func HandleTikTok(client *http.Client, urls []string) (string, error) {
	v, a, imgs, err := GetTikTokFromTikwm(client, urls)
	if err != nil {
		return "", err
	}
	if v == "" && a == "" && len(imgs) == 0 {
		return "", errors.New("tidak ada media dari TikWM (video/audio/images kosong)")
	}

	var b strings.Builder
	b.WriteString("✨ *Unduhan TikTok*\n")
	if v != "" {
		b.WriteString("• Video: " + v + "\n")
	}
	if a != "" {
		b.WriteString("• Audio: " + a + "\n")
	}
	if len(imgs) > 0 {
		b.WriteString(fmt.Sprintf("• Slides: %d gambar\n", len(imgs)))
	}
	b.WriteString("_Gunakan link sebelum kedaluwarsa._")
	return b.String(), nil
}

func firstNonEmptyURL(urls []string) string {
	for _, u := range urls {
		if s := strings.TrimSpace(u); s != "" {
			return s
		}
	}
	return ""
}
