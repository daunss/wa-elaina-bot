package vision

import (
	"context"
	"regexp"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"           // <— FIX: needed for []types.JID
	"go.mau.fi/whatsmeow/types/events"
	pbf "google.golang.org/protobuf/proto"

	"wa-elaina/internal/config"
	"wa-elaina/internal/feature/owner"
	"wa-elaina/internal/llm"
	"wa-elaina/internal/wa"               // <— FIX: used in New signature
)

type Handler struct {
	cfg    config.Config
	reTrig *regexp.Regexp
	owner  *owner.Detector
}

func New(cfg config.Config, _ *wa.Sender, re *regexp.Regexp, own *owner.Detector) *Handler {
	return &Handler{cfg: cfg, reTrig: re, owner: own}
}

func (h *Handler) TryHandle(client *whatsmeow.Client, m *events.Message, caption string, isOwner bool) bool {
	img := m.Message.GetImageMessage()
	if img == nil {
		return false
	}
	if !h.reTrig.MatchString(caption) {
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	blob, err := client.Download(ctx, img)
	if err != nil {
		replyText(ctx, client, m, "Maaf, gagal mengunduh gambar 😔")
		return true
	}
	prompt := strings.TrimSpace(h.reTrig.ReplaceAllString(caption, ""))
	if prompt == "" {
		prompt = "Tolong jelaskan gambar ini secara ringkas."
	}
	system := "Kamu Elaina — analis visual cerdas & hangat. Jawab ringkas, akurat, Bahasa Indonesia."
	reply := llm.AskVision(system, prompt, blob, img.GetMimetype())

	txt, mentions := h.owner.Decorate(isOwner, reply)
	replyTextMention(ctx, client, m, txt, mentions)
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

func replyTextMention(ctx context.Context, client *whatsmeow.Client, m *events.Message, text string, mentions []types.JID) {
	ci := &waProto.ContextInfo{
		StanzaID:      pbf.String(m.Info.ID),
		QuotedMessage: m.Message,
		Participant:   pbf.String(m.Info.Sender.String()),
		RemoteJID:     pbf.String(m.Info.Chat.String()),
	}
	for _, j := range mentions {
		ci.MentionedJID = append(ci.MentionedJID, j.String())
	}
	_, _ = client.SendMessage(ctx, m.Info.Chat, &waProto.Message{
		ExtendedTextMessage: &waProto.ExtendedTextMessage{
			Text:        pbf.String(text),
			ContextInfo: ci,
		},
	})
}
