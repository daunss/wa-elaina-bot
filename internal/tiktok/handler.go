package tiktok

import (
	"fmt"
	"net/http"
	"strings"

	"go.mau.fi/whatsmeow/types"

	dl "wa-elaina/downloader"
	"wa-elaina/internal/util"
	"wa-elaina/internal/wa"
)

type Limits struct {
	Video  int64
	Image  int64
	Doc    int64
	Slides int
}

type Handler struct {
	Client *http.Client
	Send   *wa.Sender
	L      Limits
}

func (h *Handler) TryHandle(text string, chat types.JID) bool {
	urls := dl.DetectTikTokURLs(text)
	if len(urls) == 0 {
		return false
	}

	videoURL, audioURL, images, err := dl.GetTikTokFromTikwm(h.Client, urls)
	if err != nil {
		_ = h.Send.Text(wa.DestJID(chat), "Maaf, gagal mengambil media TikTok. Coba kirim lagi ya.")
		return true
	}

	dst := wa.DestJID(chat)

	// Slides
	if len(images) > 0 {
		total := len(images)
		if h.L.Slides > 0 && total > h.L.Slides {
			total = h.L.Slides
		}
		for i := 0; i < total; i++ {
			imgURL := images[i]
			size, ctype, _ := util.HeadInfo(h.Client, imgURL)

			// if too big for image but OK for doc
			if size > 0 && h.L.Image > 0 && size > h.L.Image && (h.L.Doc <= 0 || size <= h.L.Doc) {
				if data, mime, err := util.DownloadBytes(h.Client, imgURL, h.L.Doc); err == nil {
					if mime == "" { mime = ctype }
					if mime == "" { mime = "image/jpeg" }
					_ = h.Send.Document(dst, data, mime, fmt.Sprintf("slide_%d.jpg", i+1), fmt.Sprintf("TikTok 🖼️ slide %d/%d (dokumen)", i+1, total))
				}
				continue
			}

			if data, mime, err := util.DownloadBytes(h.Client, imgURL, h.L.Image); err == nil {
				if mime == "" { mime = "image/jpeg" }
				_ = h.Send.Image(dst, data, mime, fmt.Sprintf("TikTok 🖼️ slide %d/%d", i+1, total))
			}
		}
		if s := strings.TrimSpace(audioURL); s != "" {
			_ = h.Send.Text(dst, "🔊 Audio: "+s)
		}
		return true
	}

	// Video
	if s := strings.TrimSpace(videoURL); s != "" {
		size, ctype, _ := util.HeadInfo(h.Client, s)

		// fallback document
		if size > 0 && h.L.Video > 0 && size > h.L.Video && (h.L.Doc <= 0 || size <= h.L.Doc) {
			if data, mime, err := util.DownloadBytes(h.Client, s, h.L.Doc); err == nil {
				if mime == "" { mime = ctype }
				if mime == "" { mime = "video/mp4" }
				if h.Send.Document(dst, data, mime, "tiktok.mp4", "TikTok 🎬 (dokumen)") == nil {
					if a := strings.TrimSpace(audioURL); a != "" {
						_ = h.Send.Text(dst, "🔊 Audio: "+a)
					}
					return true
				}
			}
		}

		// normal video
		if data, mime, err := util.DownloadBytes(h.Client, s, h.L.Video); err == nil {
			if mime == "" { mime = "video/mp4" }
			if h.Send.Video(dst, data, mime, "TikTok 🎬") == nil {
				if a := strings.TrimSpace(audioURL); a != "" {
					_ = h.Send.Text(dst, "🔊 Audio: "+a)
				}
				return true
			}
		}
	}

	// Audio only (rare)
	if a := strings.TrimSpace(audioURL); a != "" {
		_ = h.Send.Text(dst, "🔊 Audio: "+a)
		return true
	}

	_ = h.Send.Text(dst, "Maaf, tidak menemukan media valid dari TikTok.")
	return true
}
