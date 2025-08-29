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
	"wa-elaina/internal/wa"
)

type Handler struct {
	cfg    config.Config
	send   *wa.Sender
	reTrig *regexp.Regexp
	own    *owner.Detector
}

func New(cfg config.Config, s *wa.Sender, re *regexp.Regexp, own *owner.Detector) *Handler {
	return &Handler{cfg: cfg, send: s, reTrig: re, own: own}
}

var reMention = regexp.MustCompile(`(?i)\b(elaina|eleina|elena|elina|ela?ina)\b`)

func (h *Handler) TryHandle(client *whatsmeow.Client, m *events.Message, isOwner bool) bool {
	aud := m.Message.GetAudioMessage()
	if aud == nil { return false }

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	blob, err := client.Download(ctx, aud)
	if err != nil {
		_ = h.send.Text(wa.DestJID(m.Info.Chat), "Maaf, gagal mengambil voice note 😔")
		return true
	}
	tx := llm.Transcribe(blob, strings.ToLower(strings.TrimSpace(aud.GetMimetype())))
	if strings.TrimSpace(tx) == "" { return true }

	if !reMention.MatchString(tx) {
		if strings.EqualFold(getenv("VN_DEBUG_TRANSCRIPT","false"),"true") {
			_ = h.send.Text(wa.DestJID(m.Info.Chat), "📝 Transkrip: "+limit(tx, 120)+`\n(sebut "Elaina" agar aku membalas)`)
		}
		return true
	}
	clean := strings.TrimSpace(reMention.ReplaceAllString(tx, ""))
	if clean == "" { clean = tx }
	system := `Perankan "Elaina", penyihir cerdas & hangat. Bahasa Indonesia, ringkas, ramah.`
	reply := llm.AskText(system, clean)
	txt, mentions := h.own.Decorate(isOwner, reply)
	_ = sendTextMention(client, m.Info.Chat, txt, mentions)
	return true
}

func getenv(k, def string) string { if v:=os.Getenv(k); v!="" {return v}; return def }
func limit(s string, n int) string {
	w := strings.Fields(s); if len(w)<=n {return s}
	return strings.Join(w[:n], " ") + "…"
}

// kirim teks + mentions langsung via client (tanpa ketergantungan Sender.TextMention)
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
