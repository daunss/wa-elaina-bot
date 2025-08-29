package vision

import (
	"context"
	"regexp"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	pbf "google.golang.org/protobuf/proto"

	"wa-elaina/internal/config"
	"wa-elaina/internal/feature/owner"
	"wa-elaina/internal/llm"
	"wa-elaina/internal/wa"
)

type Handler struct {
	cfg     config.Config
	send    *wa.Sender
	reTrig  *regexp.Regexp
	owner   *owner.Detector
}

func New(cfg config.Config, s *wa.Sender, re *regexp.Regexp, own *owner.Detector) *Handler {
	return &Handler{cfg: cfg, send: s, reTrig: re, owner: own}
}

func (h *Handler) TryHandle(client *whatsmeow.Client, m *events.Message, caption string, isOwner bool) bool {
	img := m.Message.GetImageMessage()
	if img == nil { return false }
	if !h.reTrig.MatchString(caption) { return false }

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	blob, err := client.Download(ctx, img)
	if err != nil {
		_ = h.send.Text(wa.DestJID(m.Info.Chat), "Maaf, gagal mengunduh gambar 😔")
		return true
	}
	prompt := strings.TrimSpace(h.reTrig.ReplaceAllString(caption, ""))
	if prompt == "" { prompt = "Tolong jelaskan gambar ini secara ringkas." }
	system := "Kamu Elaina — analis visual cerdas & hangat. Jawab ringkas, akurat, Bahasa Indonesia."
	reply := llm.AskVision(system, prompt, blob, img.GetMimetype())
	txt, mentions := h.owner.Decorate(isOwner, reply)
	_ = sendTextMention(client, m.Info.Chat, txt, mentions)
	return true
}

func sendTextMention(client *whatsmeow.Client, to types.JID, text string, mentions []types.JID) error {
	if len(mentions) == 0 {
		_, err := client.SendMessage(context.Background(), to, &waProto.Message{
			Conversation: pbf.String(text),
		})
		return err
	}
	ci := &waProto.ContextInfo{}
	for _, j := range mentions {
		ci.MentionedJID = append(ci.MentionedJID, j.String())
	}
	msg := &waProto.Message{
		ExtendedTextMessage: &waProto.ExtendedTextMessage{
			Text:        pbf.String(text),
			ContextInfo: ci,
		},
	}
	_, err := client.SendMessage(context.Background(), to, msg)
	return err
}
