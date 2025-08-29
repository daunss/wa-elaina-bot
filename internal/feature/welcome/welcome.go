package welcome

import (
	"context"
	"os"
	"strings"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	pbf "google.golang.org/protobuf/proto"
)

type Handler struct {
	template string
}

func New() *Handler {
	tpl := strings.TrimSpace(os.Getenv("WELCOME_TEMPLATE"))
	if tpl == "" {
		tpl = "Selamat datang, {mention}! Kenalkan dirimu ya ✨"
	}
	return &Handler{template: tpl}
}

// Greet dipanggil dari main.go saat terdeteksi participant join.
func (h *Handler) Greet(client *whatsmeow.Client, group types.JID, participants []types.JID) {
	if group.Server != types.GroupServer || len(participants) == 0 {
		return
	}
	ci := &waProto.ContextInfo{}
	body := h.template

	// Untuk banyak participant, kita mention semua & ganti {mention} dengan mention pertama
	first := ""
	for i, j := range participants {
		ci.MentionedJID = append(ci.MentionedJID, j.String())
		if i == 0 {
			first = "@" + j.User
		}
	}
	body = strings.ReplaceAll(body, "{mention}", first)

	_, _ = client.SendMessage(context.Background(), group, &waProto.Message{
		ExtendedTextMessage: &waProto.ExtendedTextMessage{
			Text:        pbf.String(body),
			ContextInfo: ci,
		},
	})
}
