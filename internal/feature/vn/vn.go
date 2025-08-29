package vn

import (
	"context"
	"os"
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
	"wa-elaina/internal/wa" // <— FIX: dipakai di signature New
)

type Handler struct {
	cfg    config.Config
	reTrig *regexp.Regexp
	own    *owner.Detector
}

func New(cfg config.Config, _ *wa.Sender, re *regexp.Regexp, own *owner.Detector) *Handler {
	return &Handler{cfg: cfg, reTrig: re, own: own}
}

var reMention = regexp.MustCompile(`(?i)\b(elaina|eleina|elena|elina|ela?ina)\b`)

func (h *Handler) TryHandle(client *whatsmeow.Client, m *events.Message, isOwner bool) bool {
	aud := m.Message.GetAudioMessage()
	if aud == nil {
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	blob, err := client.Download(ctx, aud)
	if err != nil {
		replyText(ctx, client, m, "Maaf, gagal mengambil voice note 😔")
		return true
	}
	tx := llm.Transcribe(blob, strings.ToLower(strings.TrimSpace(aud.GetMimetype())))
	if strings.TrimSpace(tx) == "" {
		return true
	}

	// Hanya balas jika ada sebutan “Elaina”
	if !reMention.MatchString(tx) {
		if strings.EqualFold(getenv("VN_DEBUG_TRANSCRIPT", "false"), "true") {
			replyText(ctx, client, m, "📝 Transkrip: "+limit(tx, 120)+`\n(sebut "Elaina" agar aku membalas)`)
		}
		return true
	}
	clean := strings.TrimSpace(reMention.ReplaceAllString(tx, ""))
	if clean == "" {
		clean = tx
	}
	system := `Perankan "Elaina", penyihir cerdas & hangat. Bahasa Indonesia, ringkas, ramah.`
	reply := llm.AskText(system, clean)

	txt, mentions := h.own.Decorate(isOwner, reply)
	replyTextMention(ctx, client, m, txt, mentions)
	return true
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
func limit(s string, n int) string {
	w := strings.Fields(s)
	if len(w) <= n {
		return s
	}
	return strings.Join(w[:n], " ") + "…"
}

// ---- reply helpers ----
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
