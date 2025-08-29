package rvo

import (
	"context"
	"regexp"
	"time"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types/events"
	pbf "google.golang.org/protobuf/proto"
)

type Handler struct {
	re *regexp.Regexp
}

func New(trigger string) *Handler {
	if trigger == "" {
		trigger = "elaina"
	}
	return &Handler{
		re: regexp.MustCompile(`(?i)^(?:!rvo|` + regexp.QuoteMeta(trigger) + `\s+rvo)$`),
	}
}

func (h *Handler) TryHandle(client *whatsmeow.Client, m *events.Message, text string) bool {
	if !h.re.MatchString(text) {
		return false
	}
	xt := m.Message.GetExtendedTextMessage()
	if xt == nil || xt.GetContextInfo() == nil || xt.GetContextInfo().GetQuotedMessage() == nil {
		replyText(context.Background(), client, m, "Reply ke media view-once dengan perintah ini.")
		return true
	}
	qm := xt.GetContextInfo().GetQuotedMessage()

	// Ambil ViewOnceMessage
	var inner *waProto.Message
	if v := qm.GetViewOnceMessageV2(); v != nil && v.GetMessage() != nil {
		inner = v.GetMessage()
	} else if v2 := qm.GetViewOnceMessageV2Extension(); v2 != nil && v2.GetMessage() != nil {
		inner = v2.GetMessage()
	} else {
		replyText(context.Background(), client, m, "Pesan yang di-reply bukan media view-once.")
		return true
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	switch {
	case inner.GetImageMessage() != nil:
		img := inner.GetImageMessage()
		blob, err := client.Download(ctx, img)
		if err != nil {
			replyText(ctx, client, m, "Gagal mengunduh gambar.")
			return true
		}
		return sendImageReply(ctx, client, m, blob, img.GetMimetype(), "RVO: gambar")

	case inner.GetVideoMessage() != nil:
		v := inner.GetVideoMessage()
		blob, err := client.Download(ctx, v)
		if err != nil {
			replyText(ctx, client, m, "Gagal mengunduh video.")
			return true
		}
		return sendVideoReply(ctx, client, m, blob, v.GetMimetype(), "RVO: video")
	}

	replyText(ctx, client, m, "Media view-once tidak dikenali.")
	return true
}

// --- helpers (kirim ulang sebagai reply biasa) ---
func sendImageReply(ctx context.Context, client *whatsmeow.Client, m *events.Message, b []byte, mime, caption string) bool {
	up, err := client.Upload(ctx, b, whatsmeow.MediaImage)
	if err != nil {
		replyText(ctx, client, m, "Gagal upload gambar.")
		return true
	}
	ci := &waProto.ContextInfo{
		StanzaID:      pbf.String(m.Info.ID),
		QuotedMessage: m.Message,
		Participant:   pbf.String(m.Info.Sender.String()),
		RemoteJID:     pbf.String(m.Info.Chat.String()),
	}
	_, _ = client.SendMessage(ctx, m.Info.Chat, &waProto.Message{
		ImageMessage: &waProto.ImageMessage{
			URL:           pbf.String(up.URL),
			DirectPath:    pbf.String(up.DirectPath),
			MediaKey:      up.MediaKey,
			FileEncSHA256: up.FileEncSHA256,
			FileSHA256:    up.FileSHA256,
			FileLength:    pbf.Uint64(uint64(len(b))),
			Mimetype:      pbf.String(mime),
			Caption:       pbf.String(caption),
			ContextInfo:   ci,
		},
	})
	return true
}

func sendVideoReply(ctx context.Context, client *whatsmeow.Client, m *events.Message, b []byte, mime, caption string) bool {
	up, err := client.Upload(ctx, b, whatsmeow.MediaVideo)
	if err != nil {
		replyText(ctx, client, m, "Gagal upload video.")
		return true
	}
	ci := &waProto.ContextInfo{
		StanzaID:      pbf.String(m.Info.ID),
		QuotedMessage: m.Message,
		Participant:   pbf.String(m.Info.Sender.String()),
		RemoteJID:     pbf.String(m.Info.Chat.String()),
	}
	_, _ = client.SendMessage(ctx, m.Info.Chat, &waProto.Message{
		VideoMessage: &waProto.VideoMessage{
			URL:           pbf.String(up.URL),
			DirectPath:    pbf.String(up.DirectPath),
			MediaKey:      up.MediaKey,
			FileEncSHA256: up.FileEncSHA256,
			FileSHA256:    up.FileSHA256,
			FileLength:    pbf.Uint64(uint64(len(b))),
			Mimetype:      pbf.String(mime),
			Caption:       pbf.String(caption),
			ContextInfo:   ci,
		},
	})
	return true
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
