package bot

import (
	"context"
	"regexp"
	"strings"
	"sync/atomic"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	pbf "google.golang.org/protobuf/proto"

	"wa-elaina/internal/config"
	"wa-elaina/internal/feature/baimg"
	"wa-elaina/internal/feature/hijabin"
	"wa-elaina/internal/feature/owner"
	"wa-elaina/internal/feature/tkwrap"
	"wa-elaina/internal/feature/vn"
	"wa-elaina/internal/feature/vision"
	"wa-elaina/internal/llm"
	"wa-elaina/internal/wa"
)

type Router struct {
	cfg    config.Config
	send   *wa.Sender
	ready  *atomic.Bool
	reTrig *regexp.Regexp

	owner  *owner.Detector
	ba     *baimg.Handler
	hijab  *hijabin.Handler
	vis    *vision.Handler
	vnote  *vn.Handler
	tiktok *tkwrap.Handler
}

func NewRouter(cfg config.Config, s *wa.Sender, ready *atomic.Bool) *Router {
	trig := cfg.Trigger
	if trig == "" { trig = "elaina" }

	rt := &Router{
		cfg:    cfg,
		send:   s,
		ready:  ready,
		reTrig: regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(trig) + `\b`),
		owner:  owner.NewFromEnv(),
		ba:     baimg.New(cfg),
		hijab:  hijabin.New(cfg, s),
		tiktok: tkwrap.New(cfg, s),
	}
	llm.Init(cfg)
	rt.vis   = vision.New(cfg, s, rt.reTrig, rt.owner)
	rt.vnote = vn.New(cfg, s, rt.reTrig, rt.owner)
	return rt
}

func (r *Router) HandleMessage(client *whatsmeow.Client, m *events.Message) {
	if m.Info.IsFromMe || !r.ready.Load() { return }

	to := m.Info.Chat
	txt := extractText(m)
	isOwner := r.owner.IsOwner(m.Info)
	r.owner.Debug(m.Info, isOwner)

	// Commands
	if cmd, _, ok := parseBang(txt); ok {
		switch cmd {
		case "help":
			r.send.Text(wa.DestJID(to),
				"Perintah:\n• !help — bantuan\n• !whoami — lihat JID/LID kamu\n• ba / random ba / waifu ba — gambar Blue Archive\n• elaina hijabin — berhijabkan gambar (kirim/quote gambar)\n• Kirim gambar + sebut '"+r.cfg.Trigger+"' — analisis gambar\n• VN sebut 'elaina' — transkrip & jawab\n• Kirim link TikTok — unduh via TikWM")
		case "whoami":
			r.send.Text(wa.DestJID(to), "Sender: "+m.Info.Sender.String()+"\nChat  : "+m.Info.Chat.String())
		default:
			r.send.Text(wa.DestJID(to), "Perintah tidak dikenal. Ketik !help")
		}
		return
	}

	// BA
	if r.ba.TryHandleText(context.Background(), client, m, txt) { return }

	// Hijab
	if r.hijab.TryHandle(client, m, txt, isOwner, r.reTrig) { return }

	// Vision
	if r.vis.TryHandle(client, m, txt, isOwner) { return }

	// VN
	if r.vnote.TryHandle(client, m, isOwner) { return }

	// TikTok
	if r.tiktok.TryHandle(txt, to) { return }

	// Grup MANUAL perlu trigger
	isGroup := to.Server == types.GroupServer
	if isGroup && strings.EqualFold(r.cfg.Mode, "MANUAL") {
		low := strings.ToLower(txt)
		if !strings.Contains(low, r.cfg.Trigger) { return }
		txt = strings.TrimSpace(strings.ReplaceAll(low, r.cfg.Trigger, ""))
	}

	// LLM umum
	if strings.TrimSpace(txt) == "" { return }
	reply := llm.AskTextAsElaina(txt)
	r.sendWithOwner(client, to, reply, isOwner)
}

func (r *Router) sendWithOwner(client *whatsmeow.Client, to types.JID, reply string, isOwner bool) {
	txt, mentions := r.owner.Decorate(isOwner, reply)
	if len(mentions) == 0 {
		_ = r.send.Text(wa.DestJID(to), txt)
		return
	}
	ci := &waProto.ContextInfo{}
	for _, j := range mentions {
		ci.MentionedJID = append(ci.MentionedJID, j.String())
	}
	msg := &waProto.Message{
		ExtendedTextMessage: &waProto.ExtendedTextMessage{
			Text:        pbf.String(txt),
			ContextInfo: ci,
		},
	}
	_, _ = client.SendMessage(context.Background(), to, msg)
}

// helpers
func extractText(m *events.Message) string {
	if m.Message.GetConversation() != "" { return m.Message.GetConversation() }
	if ext := m.Message.GetExtendedTextMessage(); ext != nil && ext.GetText() != "" { return ext.GetText() }
	return ""
}
func parseBang(s string) (cmd, args string, ok bool) {
	t := strings.TrimSpace(s)
	if t == "" || !strings.HasPrefix(t, "!") { return "", "", false }
	t = strings.TrimPrefix(t, "!")
	parts := strings.Fields(t)
	if len(parts) == 0 { return "", "", false }
	return strings.ToLower(parts[0]), strings.TrimSpace(strings.TrimPrefix(t, parts[0])), true
}
