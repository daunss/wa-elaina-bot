package bot

import (
	"context"
	"log"
	"regexp"
	"strings"
	"sync/atomic"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

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
	cfg      config.Config
	send     *wa.Sender
	ready    *atomic.Bool
	reTrig   *regexp.Regexp
	owner    *owner.Detector
	ba       *baimg.Handler
	hijab    *hijabin.Handler
	vis      *vision.Handler
	vnote    *vn.Handler
	tiktok   *tkwrap.Handler
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
	// LLM helper
	llm.Init(cfg)
	// Vision & VN need llm + trigger
	rt.vis = vision.New(cfg, s, rt.reTrig, rt.owner)
	rt.vnote = vn.New(cfg, s, rt.reTrig, rt.owner)

	return rt
}

func (r *Router) HandleMessage(client *whatsmeow.Client, m *events.Message) {
	if m.Info.IsFromMe || !r.ready.Load() { return }

	to := m.Info.Chat
	text := extractText(m)
	isOwner := r.owner.IsOwner(m.Info)
	r.owner.Debug(m.Info, isOwner)

	// Commands (prefix "!")
	if cmd, _, ok := parseBang(text); ok {
		switch cmd {
		case "help":
			r.send.Text(wa.DestJID(to),
				"Perintah:\n• !help — bantuan\n• !whoami — lihat JID/LID kamu\n• ba: <nama> — gambar Blue Archive\n• Kirim link TikTok — unduh via TikWM\n• Kirim gambar + sebut '"+r.cfg.Trigger+"' — analisis gambar\n• Kirim VN & sebut 'elaina' — transkrip & jawab\n• elaina hijabin — berhijabkan gambar (kirim/quote gambar)")
		case "ping":
			r.send.Text(wa.DestJID(to), "pong")
		case "whoami":
			reply := "Sender: " + m.Info.Sender.String() +
				"\nChat  : " + m.Info.Chat.String()
			r.send.Text(wa.DestJID(to), reply)
		default:
			r.send.Text(wa.DestJID(to), "Perintah tidak dikenal. Ketik !help")
		}
		return
	}

	// BA Images (text trigger)
	if r.ba.TryHandleText(context.Background(), client, m, text, isOwner) { return }

	// Hijabin (image or reply image) — trigger harus ada
	if r.hijab.TryHandle(client, m, text, isOwner, r.reTrig) { return }

	// Vision (analisis gambar dengan trigger)
	if r.vis.TryHandle(client, m, text, isOwner) { return }

	// VN (dengan sebut 'elaina')
	if r.vnote.TryHandle(client, m, isOwner) { return }

	// TikTok
	if r.tiktok.TryHandle(text, to) { return }

	// Mode MANUAL di grup: wajib ada trigger
	isGroup := to.Server == types.GroupServer
	if isGroup && strings.EqualFold(r.cfg.Mode, "MANUAL") {
		low := strings.ToLower(text)
		found := strings.Contains(low, r.cfg.Trigger)
		if !found { return }
		text = strings.TrimSpace(strings.ReplaceAll(low, r.cfg.Trigger, ""))
	}

	// LLM obrolan umum
	if strings.TrimSpace(text) == "" { return }
	reply := llm.AskTextAsElaina(text)
	r.sendWithOwner(to, reply, isOwner)
}

func (r *Router) sendWithOwner(to types.JID, reply string, isOwner bool) {
	txt, mentions := r.owner.Decorate(isOwner, reply)
	_ = r.send.TextMention(wa.DestJID(to), txt, mentions)
}

// helpers
func extractText(m *events.Message) string {
	if m.Message.GetConversation() != "" { return m.Message.GetConversation() }
	if ext := m.Message.GetExtendedTextMessage(); ext != nil && ext.GetText() != "" {
		return ext.GetText()
	}
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
