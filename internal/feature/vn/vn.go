package vn

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
	cfg    config.Config
	reTrig *regexp.Regexp
	own    *owner.Detector
}

func New(cfg config.Config, _ *wa.Sender, re *regexp.Regexp, own *owner.Detector) *Handler {
	return &Handler{cfg: cfg, reTrig: re, own: own}
}

var reMention = regexp.MustCompile(`(?i)\b(elaina|eleina|elena|elina|ela?ina)\b`)

func (h *Handler) TryHandle(client *whatsmeow.Client, m *events.Message, isOwner bool) bool {
	// Cari audio di pesan ATAU di quoted
	aud := m.Message.GetAudioMessage()
	if aud == nil {
		if xt := m.Message.GetExtendedTextMessage(); xt != nil && xt.ContextInfo != nil {
			if qm := xt.GetContextInfo().GetQuotedMessage(); qm != nil {
				aud = qm.GetAudioMessage()
				// jika audio ada di quoted, pastikan teks pengguna menyebut "elaina"
				if aud != nil {
					txt := ""
					if m.Message.GetConversation() != "" {
						txt = m.Message.GetConversation()
					} else if xt := m.Message.GetExtendedTextMessage(); xt != nil {
						txt = xt.GetText()
					}
					if !reMention.MatchString(txt) {
						return false
					}
				}
			}
		}
	}
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
	userText := ""
	if m.Message.GetConversation() != "" {
		userText = m.Message.GetConversation()
	} else if xt := m.Message.GetExtendedTextMessage(); xt != nil {
		userText = xt.GetText()
	}
	if !reMention.MatchString(userText) && m.Message.GetAudioMessage() == nil {
		// VN dari quoted tapi user tidak menyebut Elaina → jangan balas
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
