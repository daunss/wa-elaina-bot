package rvo

import (
	"context"
	"log"
	"regexp"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types/events"
	pbf "google.golang.org/protobuf/proto"
)

type Handler struct {
	re *regexp.Regexp
}

func New() *Handler {
	// trigger sederhana: "rvo"
	return &Handler{
		re: regexp.MustCompile(`(?i)\brvo\b`),
	}
}

// TryHandle mengekstrak media view-once dari pesan yang di-reply lalu mengirim ulang sebagai media biasa.
func (h *Handler) TryHandle(client *whatsmeow.Client, m *events.Message, text string) bool {
	if !h.re.MatchString(text) {
		return false
	}

	xt := m.Message.GetExtendedTextMessage()
	if xt == nil || xt.ContextInfo == nil || xt.ContextInfo.QuotedMessage == nil {
		h.replyText(context.Background(), client, m, "Reply foto/video *view-once* lalu ketik *elaina rvo* ya ✨")
		return true
	}

	// Ambil pesan yang di-reply
	qm := xt.ContextInfo.GetQuotedMessage()
	// Unwrap semua kemungkinan pembungkus (ephemeral & berbagai versi view-once)
	inner := unwrapAll(qm)

	// Pastikan memang media view-once (foto/video)
	dl, mediaKind, origMime, caption := downloadable(inner)
	if dl == nil {
		h.replyText(context.Background(), client, m, "Pesan yang di-reply bukan media *view-once* (foto/video).")
		return true
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Unduh media asli
	blob, err := client.Download(ctx, dl)
	if err != nil {
		log.Printf("[RVO] download error: %v", err)
		h.replyText(ctx, client, m, "Gagal mengunduh media view-once 😔")
		return true
	}

	// Upload ulang sebagai media biasa
	var cat whatsmeow.MediaType
	switch mediaKind {
	case "image":
		cat = whatsmeow.MediaImage
	case "video":
		cat = whatsmeow.MediaVideo
	default:
		h.replyText(ctx, client, m, "Jenis media tidak didukung.")
		return true
	}

	up, err := client.Upload(ctx, blob, cat)
	if err != nil {
		log.Printf("[RVO] upload error: %v", err)
		h.replyText(ctx, client, m, "Gagal mengunggah ulang media 😔")
		return true
	}

	// Kirim ulang (reply ke pesan user yang mengetik rvo)
	ci := &waProto.ContextInfo{
		StanzaID:      pbf.String(m.Info.ID),
		QuotedMessage: m.Message,
		Participant:   pbf.String(m.Info.Sender.String()),
		RemoteJID:     pbf.String(m.Info.Chat.String()),
	}

	switch mediaKind {
	case "image":
		_, _ = client.SendMessage(ctx, m.Info.Chat, &waProto.Message{
			ImageMessage: &waProto.ImageMessage{
				URL:           pbf.String(up.URL),
				DirectPath:    pbf.String(up.DirectPath),
				MediaKey:      up.MediaKey,
				FileEncSHA256: up.FileEncSHA256,
				FileSHA256:    up.FileSHA256,
				FileLength:    pbf.Uint64(uint64(len(blob))),
				Mimetype:      pbf.String(fallback(origMime, "image/jpeg")),
				Caption:       pbf.String(strings.TrimSpace(caption)),
				ContextInfo:   ci,
			},
		})
	case "video":
		_, _ = client.SendMessage(ctx, m.Info.Chat, &waProto.Message{
			VideoMessage: &waProto.VideoMessage{
				URL:           pbf.String(up.URL),
				DirectPath:    pbf.String(up.DirectPath),
				MediaKey:      up.MediaKey,
				FileEncSHA256: up.FileEncSHA256,
				FileSHA256:    up.FileSHA256,
				FileLength:    pbf.Uint64(uint64(len(blob))),
				Mimetype:      pbf.String(fallback(origMime, "video/mp4")),
				Caption:       pbf.String(strings.TrimSpace(caption)),
				ContextInfo:   ci,
			},
		})
	}
	return true
}

// --------------------- helpers ---------------------

// unwrapAll membongkar semua lapisan pembungkus (Ephemeral, ViewOnce v1/v2/v2ext)
func unwrapAll(msg *waProto.Message) *waProto.Message {
	if msg == nil {
		return nil
	}
	guard := 0
	for guard < 10 && msg != nil {
		guard++
		switch {
		case msg.GetEphemeralMessage() != nil && msg.GetEphemeralMessage().Message != nil:
			msg = msg.GetEphemeralMessage().GetMessage()

		case msg.GetViewOnceMessageV2Extension() != nil && msg.GetViewOnceMessageV2Extension().Message != nil:
			msg = msg.GetViewOnceMessageV2Extension().GetMessage()

		case msg.GetViewOnceMessageV2() != nil && msg.GetViewOnceMessageV2().Message != nil:
			msg = msg.GetViewOnceMessageV2().GetMessage()

		case msg.GetViewOnceMessage() != nil && msg.GetViewOnceMessage().Message != nil:
			msg = msg.GetViewOnceMessage().GetMessage()

		default:
			return msg
		}
	}
	return msg
}

// downloadable mengembalikan objek yang bisa di-Download oleh whatsmeow,
// jenis media ("image"/"video"), mimetype, dan caption aslinya (jika ada).
func downloadable(msg *waProto.Message) (dl whatsmeow.DownloadableMessage, kind, mime, caption string) {
	if msg == nil {
		return nil, "", "", ""
	}
	if im := msg.GetImageMessage(); im != nil {
		return im, "image", im.GetMimetype(), im.GetCaption()
	}
	if vm := msg.GetVideoMessage(); vm != nil {
		return vm, "video", vm.GetMimetype(), vm.GetCaption()
	}
	return nil, "", "", ""
}

func (h *Handler) replyText(ctx context.Context, client *whatsmeow.Client, m *events.Message, s string) {
	ci := &waProto.ContextInfo{
		StanzaID:      pbf.String(m.Info.ID),
		QuotedMessage: m.Message,
		Participant:   pbf.String(m.Info.Sender.String()),
		RemoteJID:     pbf.String(m.Info.Chat.String()),
	}
	_, _ = client.SendMessage(ctx, m.Info.Chat, &waProto.Message{
		ExtendedTextMessage: &waProto.ExtendedTextMessage{
			Text:        pbf.String(s),
			ContextInfo: ci,
		},
	})
}

func fallback(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
