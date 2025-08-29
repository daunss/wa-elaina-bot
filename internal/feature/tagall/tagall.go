package tagall

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
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
		re: regexp.MustCompile(`(?i)^(?:!tagall|` + regexp.QuoteMeta(trigger) + `\s+tagall)\b`),
	}
}

func (h *Handler) TryHandle(client *whatsmeow.Client, m *events.Message, text string) bool {
	if !h.re.MatchString(text) {
		return false
	}
	chat := m.Info.Chat
	if chat.Server != types.GroupServer {
		replyText(context.Background(), client, chat, m, "Perintah ini hanya untuk grup.")
		return true
	}

	ginfo, err := client.GetGroupInfo(chat)
	if err != nil || ginfo == nil || len(ginfo.Participants) == 0 {
		replyText(context.Background(), client, chat, m, "Gagal mengambil daftar anggota grup.")
		return true
	}

	mentions := make([]string, 0, len(ginfo.Participants))
	jids := make([]types.JID, 0, len(ginfo.Participants))
	for _, p := range ginfo.Participants {
		if p.JID.User == client.Store.ID.User {
			continue
		}
		mentions = append(mentions, fmt.Sprintf("@%s", p.JID.User))
		jids = append(jids, p.JID)
	}

	body := "Tag-all:\n" + strings.Join(mentions, " ")
	replyTextMention(context.Background(), client, chat, m, body, jids)
	return true
}

func replyText(ctx context.Context, client *whatsmeow.Client, to types.JID, quoted *events.Message, msg string) {
	ci := &waProto.ContextInfo{
		StanzaID:      pbf.String(quoted.Info.ID),
		QuotedMessage: quoted.Message,
		Participant:   pbf.String(quoted.Info.Sender.String()),
		RemoteJID:     pbf.String(quoted.Info.Chat.String()),
	}
	_, _ = client.SendMessage(ctx, to, &waProto.Message{
		ExtendedTextMessage: &waProto.ExtendedTextMessage{
			Text:        pbf.String(msg),
			ContextInfo: ci,
		},
	})
}

func replyTextMention(ctx context.Context, client *whatsmeow.Client, to types.JID, quoted *events.Message, text string, mentions []types.JID) {
	ci := &waProto.ContextInfo{
		StanzaID:      pbf.String(quoted.Info.ID),
		QuotedMessage: quoted.Message,
		Participant:   pbf.String(quoted.Info.Sender.String()),
		RemoteJID:     pbf.String(quoted.Info.Chat.String()),
	}
	for _, j := range mentions {
		ci.MentionedJID = append(ci.MentionedJID, j.String())
	}
	_, _ = client.SendMessage(ctx, to, &waProto.Message{
		ExtendedTextMessage: &waProto.ExtendedTextMessage{
			Text:        pbf.String(text),
			ContextInfo: ci,
		},
	})
}
