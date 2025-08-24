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

// Regex luas: vt.tiktok.com, www/m, @user/video/ID, v/...
var reTikTok = regexp.MustCompile(`https?://(?:vt\.)?tiktok\.com/[^\s]+|https?://(?:www\.|m\.)?tiktok\.com/(?:@[A-Za-z0-9._-]+/video/\d+|v/[^\s]+|[^\s]+)`)

func DetectTikTokURLs(text string) []string { return reTikTok.FindAllString(text, -1) }

// --- Expand shortlink vt.tiktok.com ke URL final ---
func ExpandTikTokURL(client *http.Client, raw string) string {
	if client == nil { client = &http.Client{Timeout: 20 * time.Second} }
	u := strings.TrimSpace(raw)

	req, _ := http.NewRequest(http.MethodHead, u, nil)
	req.Header.Set("User-Agent", "wa-elaina-bot/1.0")
	if resp, err := client.Do(req); err == nil && resp != nil && resp.Request != nil && resp.Request.URL != nil {
		resp.Body.Close()
		return resp.Request.URL.String()
	}
	req2, _ := http.NewRequest(http.MethodGet, u, nil)
	req2.Header.Set("User-Agent", "wa-elaina-bot/1.0")
	if resp2, err2 := client.Do(req2); err2 == nil && resp2 != nil && resp2.Request != nil && resp2.Request.URL != nil {
		io.Copy(io.Discard, resp2.Body)
		resp2.Body.Close()
		return resp2.Request.URL.String()
	}
	return u
}

// ---------- API Vreden ----------
type vredenResp struct {
	Status  int    `json:"status"`
	Creator string `json:"creator"`
	Result  struct {
		Status bool   `json:"status"`
		Msg    string `json:"msg"`
		Play   string `json:"play"`
		Audio  string `json:"audio"`
		Nowm   string `json:"nowm"`
		Video  string `json:"video"`
	} `json:"result"`
}

func callVreden(client *http.Client, link string) (video, audio string, err error) {
	api := "https://api.vreden.my.id/api/tiktok?url=" + url.QueryEscape(link)
	req, _ := http.NewRequest(http.MethodGet, api, nil)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "wa-elaina-bot/1.0")
	resp, err := client.Do(req)
	if err != nil { return "", "", err }
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("vreden HTTP %d: %s", resp.StatusCode, string(body))
	}
	var data vredenResp
	if err := json.Unmarshal(body, &data); err != nil { return "", "", err }
	if !data.Result.Status {
		return "", "", errors.New("vreden: " + firstNonEmpty(data.Result.Msg, "status=false"))
	}
	return firstNonEmpty(data.Result.Play, data.Result.Nowm, data.Result.Video), data.Result.Audio, nil
}

// ---------- API TikWM ----------
type tikwmResp struct {
	Data struct {
		Play  string `json:"play"`  // mp4 no-wm
		Music string `json:"music"` // mp3
	} `json:"data"`
}

func callTikwm(client *http.Client, link string) (video, audio string, err error) {
	api := "https://www.tikwm.com/api/?url=" + url.QueryEscape(link)
	req, _ := http.NewRequest(http.MethodGet, api, nil)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "wa-elaina-bot/1.0")
	resp, err := client.Do(req)
	if err != nil { return "", "", err }
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("tikwm HTTP %d: %s", resp.StatusCode, string(body))
	}
	var data tikwmResp
	if err := json.Unmarshal(body, &data); err != nil { return "", "", err }
	return strings.TrimSpace(data.Data.Play), strings.TrimSpace(data.Data.Music), nil
}

// ---------- Entry point (BALAS TEKS LINK) ----------
func HandleTikTok(client *http.Client, urls []string) (string, error) {
	if client == nil { client = &http.Client{Timeout: 30 * time.Second} }

	// pilih URL pertama yang valid lalu expand
	raw := ""
	for _, u := range urls {
	 if strings.TrimSpace(u) != "" { raw = u; break }
	}
	if raw == "" { return "", errors.New("tidak ada url tiktok valid") }

	final := ExpandTikTokURL(client, raw)

	// 1) Coba Vreden dulu
	video, audio, err := callVreden(client, final)
	if err != nil || (video == "" && audio == "") {
		// 2) Fallback ke TikWM
		v2, a2, err2 := callTikwm(client, final)
		if err2 != nil {
			if err != nil { return "", fmt.Errorf("%v; fallback: %v", err, err2) }
			return "", err2
		}
		video, audio = v2, a2
	}

	if video == "" && audio == "" {
		return "", errors.New("tidak ada URL video/audio pada respons")
	}

	var b strings.Builder
	b.WriteString("✨ *Unduhan TikTok*\n")
	if video != "" { b.WriteString("• Video: " + video + "\n") }
	if audio != "" { b.WriteString("• Audio: " + audio + "\n") }
	b.WriteString("_Gunakan link sebelum kedaluwarsa._")
	return b.String(), nil
}

// ---------- Entry point (MEDIA URL untuk upload ke WA) ----------
func GetTikTokMedia(client *http.Client, urls []string) (videoURL, audioURL string, err error) {
	if client == nil { client = &http.Client{Timeout: 30 * time.Second} }

	// pilih URL pertama yang valid lalu expand
	raw := ""
	for _, u := range urls {
		if strings.TrimSpace(u) != "" { raw = u; break }
	}
	if raw == "" { return "", "", errors.New("tidak ada url tiktok valid") }

	final := ExpandTikTokURL(client, raw)

	// 1) Coba Vreden dulu
	video, audio, err := callVreden(client, final)
	if err != nil || (strings.TrimSpace(video) == "" && strings.TrimSpace(audio) == "") {
		// 2) Fallback ke TikWM
		v2, a2, err2 := callTikwm(client, final)
		if err2 != nil {
			if err != nil { return "", "", fmt.Errorf("%v; fallback: %v", err, err2) }
			return "", "", err2
		}
		video, audio = v2, a2
	}
	video = strings.TrimSpace(video)
	audio = strings.TrimSpace(audio)
	if video == "" && audio == "" {
		return "", "", errors.New("tidak ada URL video/audio pada respons")
	}
	return video, audio, nil
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if strings.TrimSpace(v) != "" { return v }
	}
	return ""
}
