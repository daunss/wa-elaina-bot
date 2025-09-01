package tagall

import (
	"context"
	"regexp"
	"strings"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	pbf "google.golang.org/protobuf/proto"
)

type Handler struct {
	re   *regexp.Regexp // mendeteksi kata "tagall"
	trig string
}

func New(trigger string) *Handler {
	return &Handler{
		re:   regexp.MustCompile(`(?i)\btagall\b`),
		trig: trigger,
	}
}

// TryHandle: aktif jika di GRUP dan user mengetik:
// - "!tagall", atau
// - "elaina tagall" (atau ada kata "tagall" + trigger ditangani oleh router)
func (h *Handler) TryHandle(client *whatsmeow.Client, m *events.Message, text string) bool {
	// Hanya relevan di grup
	if m.Info.Chat.Server != types.GroupServer {
		return false
	}

	t := strings.TrimSpace(text)
	isCmd := strings.HasPrefix(t, "!")
	isBangTagAll := false
	if isCmd {
		parts := strings.Fields(strings.TrimPrefix(t, "!"))
		if len(parts) > 0 && strings.EqualFold(parts[0], "tagall") {
			isBangTagAll = true
		}
	}

	// Deteksi pola
	if !isBangTagAll && !h.re.MatchString(t) {
		return false
	}

	// Ambil info grup
	gi, err := client.GetGroupInfo(m.Info.Chat)
	if err != nil || gi == nil || len(gi.Participants) == 0 {
		return false
	}

	// Kumpulkan semua JID member
	var all []types.JID
	for _, p := range gi.Participants {
		all = append(all, p.JID)
	}

	// Kirim mention per-batch agar aman (WA kadang limit besar)
	const batch = 25
	for i := 0; i < len(all); i += batch {
		end := i + batch
		if end > len(all) {
			end = len(all)
		}
		sub := all[i:end]
		h.sendMention(client, m, sub, "👋 Halo semuanya, hadir ya!")
	}

	return true
}

func (h *Handler) sendMention(client *whatsmeow.Client, m *events.Message, jids []types.JID, text string) {
	ci := &waProto.ContextInfo{
		StanzaID:      pbf.String(m.Info.ID),
		QuotedMessage: m.Message,
		Participant:   pbf.String(m.Info.Sender.String()),
		RemoteJID:     pbf.String(m.Info.Chat.String()),
	}
	for _, j := range jids {
		ci.MentionedJID = append(ci.MentionedJID, j.String())
	}

	msg := &waProto.Message{
		ExtendedTextMessage: &waProto.ExtendedTextMessage{
			Text:        pbf.String(text),
			ContextInfo: ci,
		},
	}
	_, _ = client.SendMessage(context.Background(), m.Info.Chat, msg)
}
