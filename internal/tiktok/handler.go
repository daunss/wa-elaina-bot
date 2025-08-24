package tiktok

import (
  "fmt"; "strings"
  "go.mau.fi/whatsmeow/types"
  dl "wa-elaina/downloader"
  "wa-elaina/internal/wa"
  "wa-elaina/internal/util"
)

type Limits struct{ Video, Image, Doc int64; Slides int }
type Handler struct{ H *util.HTTP; S *wa.Sender; L Limits }

func (h *Handler) TryHandle(text string, chat types.JID) (handled bool) {
  urls := dl.DetectTikTokURLs(text)
  if len(urls)==0 { return false }

  v, a, imgs, err := dl.GetTikTokFromTikwm(h.H.Client, urls)
  if err != nil { _ = h.S.Text(wa.DestJID(chat), "Maaf, gagal mengambil media TikTok."); return true }

  dst := wa.DestJID(chat)

  if len(imgs) > 0 {
    total := len(imgs); if h.L.Slides>0 && total>h.L.Slides { total = h.L.Slides }
    for i:=0; i<total; i++ {
      size, ctype, _ := util.HeadInfo(h.H.Client, imgs[i])
      if size>0 && h.L.Image>0 && size>h.L.Image && (h.L.Doc<=0 || size<=h.L.Doc) {
        data, mime, err := util.DownloadBytes(h.H.Client, imgs[i], h.L.Doc)
        if err==nil {
          if mime=="" { mime=ctype }; if mime=="" { mime="image/jpeg" }
          _ = h.S.Document(dst, data, mime, fmt.Sprintf("slide_%d.jpg", i+1), fmt.Sprintf("TikTok 🖼️ slide %d/%d (dokumen)", i+1, total))
        }
        continue
      }
      data, mime, err := util.DownloadBytes(h.H.Client, imgs[i], h.L.Image)
      if err==nil {
        if mime=="" { mime="image/jpeg" }
        _ = h.S.Image(dst, data, mime, fmt.Sprintf("TikTok 🖼️ slide %d/%d", i+1, total))
      }
    }
    if strings.TrimSpace(a)!="" { _ = h.S.Text(dst, "🔊 Audio: "+a) }
    return true
  }

  if strings.TrimSpace(v)!="" {
    size, ctype, _ := util.HeadInfo(h.H.Client, v)
    if size>0 && h.L.Video>0 && size>h.L.Video && (h.L.Doc<=0 || size<=h.L.Doc) {
      if data, mime, err := util.DownloadBytes(h.H.Client, v, h.L.Doc); err==nil {
        if mime=="" { mime=ctype }; if mime=="" { mime="video/mp4" }
        if h.S.Document(dst, data, mime, "tiktok.mp4", "TikTok 🎬 (dokumen)")==nil {
          if strings.TrimSpace(a)!="" { _ = h.S.Text(dst, "🔊 Audio: "+a) }
          return true
        }
      }
    }
    if data, mime, err := util.DownloadBytes(h.H.Client, v, h.L.Video); err==nil {
      if mime=="" { mime="video/mp4" }
      if h.S.Video(dst, data, mime, "TikTok 🎬")==nil {
        if strings.TrimSpace(a)!="" { _ = h.S.Text(dst, "🔊 Audio: "+a) }
        return true
      }
    }
  }

  if strings.TrimSpace(a)!="" { _ = h.S.Text(dst, "🔊 Audio: "+a); return true }
  _ = h.S.Text(dst, "Maaf, tidak menemukan media valid dari TikTok.")
  return true
}
