package bot

import (
	"context"
	"log"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	pbf "google.golang.org/protobuf/proto"

	"wa-elaina/internal/config"
	"wa-elaina/internal/feature/baimg"
	"wa-elaina/internal/feature/hijabin"
	"wa-elaina/internal/feature/owner"
	"wa-elaina/internal/feature/rvo"
	"wa-elaina/internal/feature/tagall"
	"wa-elaina/internal/feature/tkwrap"
	"wa-elaina/internal/feature/tts"
	"wa-elaina/internal/feature/vn"
	"wa-elaina/internal/feature/vision"
	"wa-elaina/internal/llm"
	"wa-elaina/internal/wa"
	"wa-elaina/internal/db"
	"wa-elaina/internal/memory"
	"wa-elaina/internal/feature/sticker"
)

// cue kata-kunci yang menandakan "balas yang direply"
var reReplyCue = regexp.MustCompile(`(?i)\b(balas(in|lah)?|reply|jawab(in|lah)?)(\s+ini)?\b`)

type Router struct {
	cfg    config.Config
	send   *wa.Sender
	ready  *atomic.Bool
	reTrig *regexp.Regexp
	store  *db.Store

	owner  *owner.Detector
	ba     *baimg.Handler
	hijab  *hijabin.Handler
	vis    *vision.Handler
	vnote  *vn.Handler
	tts    *tts.Handler
	tiktok *tkwrap.Handler
	rvo    *rvo.Handler
	tall   *tagall.Handler

	// === Tambahan fitur sticker ===
	stik   *sticker.Handler
}

func NewRouter(cfg config.Config, s *wa.Sender, ready *atomic.Bool, store *db.Store) *Router {
	trig := cfg.Trigger
	if trig == "" {
		trig = "elaina"
	}
	rt := &Router{
		cfg:    cfg,
		send:   s,
		ready:  ready,
		reTrig: regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(trig) + `\b`),
		store:  store,
		owner:  owner.NewFromEnv(),
		ba:     baimg.New(cfg),
		hijab:  hijabin.New(cfg, s),
		tiktok: tkwrap.New(cfg, s),
		rvo:    rvo.New(),
		tall:   tagall.New(trig),
	}
	llm.Init(cfg)
	rt.vis = vision.New(cfg, s, rt.reTrig, rt.owner)
	rt.vnote = vn.New(cfg, s, rt.reTrig, rt.owner)
	rt.tts = tts.New(cfg, rt.reTrig)

	// === Tambahan: inisialisasi handler sticker ===
	rt.stik = sticker.New()

	return rt
}

func (r *Router) HandleMessage(client *whatsmeow.Client, m *events.Message) {
	if m.Info.IsFromMe || !r.ready.Load() {
		return
	}

	to := m.Info.Chat
	txt := extractText(m)
	origTxt := txt // simpan teks ASLI user untuk validasi trigger di MANUAL mode

	isOwner := r.owner.IsOwner(m.Info)
	r.owner.Debug(m.Info, isOwner)

	// ==== DETEKSI REPLY (QUOTED) ====
	var (
		quotedImg  = false
		quotedAud  = false
		quotedText = ""
	)
	if xt := m.Message.GetExtendedTextMessage(); xt != nil && xt.ContextInfo != nil {
		if qm := xt.GetContextInfo().GetQuotedMessage(); qm != nil {
			quotedImg = qm.GetImageMessage() != nil
			quotedAud = qm.GetAudioMessage() != nil
			if t := qm.GetConversation(); t != "" {
				quotedText = t
			} else if et := qm.GetExtendedTextMessage(); et != nil {
				quotedText = et.GetText()
			}
		}
	}
	if quotedImg || quotedAud || quotedText != "" {
		log.Printf("[REPLY] chat=%s quoted{img:%t aud:%t textLen:%d}", m.Info.Chat.String(), quotedImg, quotedAud, len(quotedText))
	}

	// ==== Commands (reply) / bantuan singkat ====
	if cmd, _, ok := parseBang(txt); ok {
		switch cmd {
		case "help":
			replyText(context.Background(), client, m,
				"Perintah:\n• !help — bantuan\n• !whoami — lihat JID/LID kamu\n• !tagall / elaina tagall — mention semua anggota grup\n• !rvo — buka media sekali lihat (reply ke pesannya)\n• ba / kirim gambar blue archive — gambar BA\n• elaina hijabin — berhijabkan gambar (kirim/quote gambar)\n• elaina vn <teks> — kirim voice note\n• Kirim gambar + sebut '"+r.cfg.Trigger+"' — analisis gambar\n• VN sebut 'elaina' — transkrip & jawab\n• Kirim link TikTok — unduh via TikWM\n• !elaina persona elaina1|elaina2 — pilih persona AI (persist)\n• !elaina mode pro on|off — aktifkan Mode Pro (persist)")
		case "whoami":
			replyText(context.Background(), client, m, "Sender: "+m.Info.Sender.String()+"\nChat  : "+m.Info.Chat.String())
		case "elaina":
			after := strings.TrimSpace(strings.TrimPrefix(txt, "!elaina"))
			parts := strings.Fields(after)
			// persona
			if len(parts) >= 2 && strings.EqualFold(parts[0], "persona") {
				p := strings.ToLower(parts[1])
				if p == "1" {
					p = "elaina1"
				}
				if p == "2" {
					p = "elaina2"
				}
				if p != "elaina1" && p != "elaina2" {
					replyText(context.Background(), client, m, "Persona tidak valid. Gunakan: elaina1 atau elaina2.")
					return
				}
				_ = r.store.SetPersona(m.Info.Chat.String(), p)
				replyText(context.Background(), client, m, "Persona disetel ke "+p+" untuk chat ini ✅")
				return
			}
			// mode pro
			if len(parts) >= 3 && strings.EqualFold(parts[0], "mode") && strings.EqualFold(parts[1], "pro") {
				on := strings.EqualFold(parts[2], "on") || strings.EqualFold(parts[2], "enable")
				_ = r.store.SetPro(m.Info.Chat.String(), on)
				if on {
					replyText(context.Background(), client, m, "Mode Pro diaktifkan (persist) ✨")
				} else {
					replyText(context.Background(), client, m, "Mode Pro dimatikan (persist).")
				}
				return
			}
			replyText(context.Background(), client, m, "Gunakan: !elaina persona elaina1|elaina2  atau  !elaina mode pro on|off")
			return
		}
	}

	// ----------- GATE & kebijakan trigger ----------
	cmd, _, isCmd := parseBang(origTxt)
	isGroup := to.Server == types.GroupServer
	hasTrig := r.reTrig.MatchString(origTxt)
	isTagAllCmd := isCmd && strings.EqualFold(cmd, "tagall")

	// === NEW: Early guard untuk semua pesan yang merupakan REPLY ===
	hasQuoted := quotedImg || quotedAud || quotedText != ""
	if hasQuoted && !hasTrig && !isCmd {
		return
	}

	// Khusus pesan gambar/video: di GRUP wajib ada trigger di CAPTION (atau command)
	hasImage := m.Message.ImageMessage != nil
	hasVideo := m.Message.VideoMessage != nil
	if isGroup && (hasImage || hasVideo) && !hasTrig && !isCmd {
		return
	}

	// ====== Deteksi TIKTOK (gabungkan field yang tersedia) ======
	tiktokText := origTxt
	hasTrigTikTok := hasTrig
	if ext := m.Message.GetExtendedTextMessage(); ext != nil {
		if s := ext.GetMatchedText(); s != "" {
			tiktokText += " " + s
			if r.reTrig.MatchString(s) {
				hasTrigTikTok = true
			}
		}
		if s := ext.GetText(); s != "" && r.reTrig.MatchString(s) {
			hasTrigTikTok = true
		}
	}

	// ====== Fitur PRIORITAS (langsung ke script, bukan LLM) ======
	allowRvoTagall := !isGroup || hasTrig || isTagAllCmd
	if allowRvoTagall {
		if r.rvo.TryHandle(client, m, txt) { return }
		if r.tall.TryHandle(client, m, txt) { return }
	}

	// === Tambahan: Sticker (img/gif → webp) — taruh sebelum TikTok agar URL tidak diambil handler lain ===
	if r.stik.TryHandleTo(client, m.Info.Chat, m.Message, txt) { return }

	allowTikTok := !isGroup || hasTrigTikTok
	if allowTikTok {
		if r.tiktok.TryHandle(tiktokText, to) { return }
	}

	// ----------- Non-command hanya boleh jika ada trigger (di grup) -----------
	allowNonCommand := !isGroup || hasTrig
	if allowNonCommand {
		if r.ba.TryHandleText(context.Background(), client, m, txt, isOwner) { return }
		if r.hijab.TryHandle(client, m, txt, isOwner, r.reTrig) { return }
		if r.vis.TryHandle(client, m, txt, isOwner) { return }
		if r.tts.TryHandle(client, m, txt) { return }
	}

	// VN
	if r.vnote.TryHandle(client, m, isOwner) { return }

	// ==== Reply-to-text: hanya aktif jika user menyebut trigger ====
	if quotedText != "" && r.reTrig.MatchString(origTxt) {
		after := strings.TrimSpace(r.reTrig.ReplaceAllString(origTxt, ""))
		if after == "" || reReplyCue.MatchString(after) {
			txt = quotedText
		} else {
			txt = after + "\n\nKonteks (pesan yang di-reply): " + quotedText
		}
	}

	// ==== Grup MANUAL: perlu trigger pada pesan asli user ====
	if isGroup && strings.EqualFold(r.cfg.Mode, "MANUAL") {
		if !r.reTrig.MatchString(origTxt) && !isTagAllCmd {
			return
		}
		clean := strings.TrimSpace(r.reTrig.ReplaceAllString(strings.ToLower(origTxt), ""))
		if clean == "" && quotedText != "" {
			txt = quotedText
		} else if clean != "" && txt == origTxt {
			txt = clean
		}
	}

	// ==== LLM (pakai persona + mode pro dari DB) ====
	if strings.TrimSpace(txt) == "" {
		return
	}
	state, _ := r.store.Get(m.Info.Chat.String())

	//muat memory & padukan ke prompt
	hist, _ := memory.Load(m.Info.Chat.String())
	ctxTxt := memory.BuildContext(hist, txt)

	reply := llm.AskAsPersona(r.cfg, state.Persona, state.Pro, ctxTxt, time.Now())

	//simpan turn terbaru
	_ = memory.SaveTurn(m.Info.Chat.String(), "user", txt)
	_ = memory.SaveTurn(m.Info.Chat.String(), "assistant", reply)

	txtOut, mentions := r.owner.Decorate(isOwner, reply)
	replyTextMention(context.Background(), client, m, txtOut, mentions)
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

// ---- small helpers ----
func extractText(m *events.Message) string {
	if m.Message.GetConversation() != "" {
		return m.Message.GetConversation()
	}
	if ext := m.Message.GetExtendedTextMessage(); ext != nil && ext.GetText() != "" {
		return ext.GetText()
	}
	if img := m.Message.GetImageMessage(); img != nil && img.GetCaption() != "" {
		return img.GetCaption()
	}
	if v := m.Message.GetVideoMessage(); v != nil && v.GetCaption() != "" {
		return v.GetCaption()
	}
	return ""
}

func parseBang(s string) (cmd, args string, ok bool) {
	t := strings.TrimSpace(s)
	if t == "" || !strings.HasPrefix(t, "!") {
		return "", "", false
	}
	t = strings.TrimPrefix(t, "!")
	parts := strings.Fields(t)
	if len(parts) == 0 {
		return "", "", false
	}
	return strings.ToLower(parts[0]), strings.TrimSpace(strings.TrimPrefix(t, parts[0])), true
}
