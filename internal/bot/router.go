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

	dl "wa-elaina/downloader"
	"wa-elaina/internal/config"
	"wa-elaina/internal/db"
	"wa-elaina/internal/feature/anime"
	"wa-elaina/internal/feature/baimg"
	"wa-elaina/internal/feature/brat"
	"wa-elaina/internal/feature/hijabin"
	"wa-elaina/internal/feature/imggen" // Import image generation handler
	"wa-elaina/internal/feature/owner"
	"wa-elaina/internal/feature/peraturan"
	"wa-elaina/internal/feature/rvo"
	"wa-elaina/internal/feature/sticker"
	"wa-elaina/internal/feature/tagall"
	"wa-elaina/internal/feature/tkwrap"
	"wa-elaina/internal/feature/tts"
	"wa-elaina/internal/feature/vision"
	"wa-elaina/internal/feature/vn"
	"wa-elaina/internal/llm"
	"wa-elaina/internal/memory"
	"wa-elaina/internal/wa"
)

var reReplyCue = regexp.MustCompile(`(?i)\b(balas(in|lah)?|reply|jawab(in|lah)?)(\s+ini)?\b`)

type Router struct {
	cfg    config.Config
	send   *wa.Sender
	ready  *atomic.Bool
	reTrig *regexp.Regexp
	store  *db.Store

	owner     *owner.Detector
	ba        *baimg.Handler
	hijab     *hijabin.Handler
	vis       *vision.Handler
	vnote     *vn.Handler
	tts       *tts.Handler
	tiktok    *tkwrap.Handler
	rvo       *rvo.Handler
	tall      *tagall.Handler
	stik      *sticker.Handler
	anime     *anime.Handler
	brat      *brat.Handler
	imggen    *imggen.Handler // Tambah image generation handler
	peraturan *peraturan.Handler
}

func NewRouter(cfg config.Config, s *wa.Sender, ready *atomic.Bool, store *db.Store) *Router {
	trig := cfg.Trigger
	if trig == "" {
		trig = "elaina"
	}

	reTrig := regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(trig) + `\b`)

	rt := &Router{
		cfg:    cfg,
		send:   s,
		ready:  ready,
		reTrig: reTrig,
		store:  store,
		owner:  owner.NewFromEnv(),
		ba:     baimg.New(cfg),
		hijab:  hijabin.New(cfg, s),
		tiktok: tkwrap.New(cfg, s),
		rvo:    rvo.New(),
		tall:   tagall.New(trig),
		stik:   sticker.New(),
		imggen: imggen.New(cfg), // Initialize image generation handler
	}

	// Initialize handlers yang membutuhkan rt setelah struct dibuat
	rt.brat = brat.New(rt.reTrig)

	llm.Init(cfg)
	rt.vis = vision.New(cfg, s, rt.reTrig, rt.owner)
	rt.vnote = vn.New(cfg, s, rt.reTrig, rt.owner)
	rt.anime = anime.New(rt.reTrig, s)
	rt.peraturan = peraturan.New(store)
	rt.tts = tts.New(cfg, rt.reTrig)

	return rt
}

func (r *Router) HandleMessage(client *whatsmeow.Client, m *events.Message) {
	if m.Info.IsFromMe || !r.ready.Load() {
		return
	}

	to := m.Info.Chat
	txt := extractText(m)
	origTxt := txt
	senderJID := m.Info.Sender.String() // JID untuk tracking nama

	isOwner := r.owner.IsOwner(m.Info)
	r.owner.Debug(m.Info, isOwner)

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

	// PRIORITAS TERTINGGI: IMAGE GENERATION - cek pertama kali
	if r.imggen.TryHandle(client, m, origTxt, isOwner) {
		return
	}

	if cmd, rest, ok := parseBang(txt); ok {
		switch cmd {
		case "help":
			userName, _ := memory.GetUserName(senderJID)
			greeting := "Hai!"
			if userName != "" {
				greeting = "Hai " + userName + "!"
			}
			lines := []string{
				greeting + " Ini perintah yang bisa kamu gunakan:",
				"",
				"- !help : bantuan ringkas",
				"- !whoami : lihat JID/LID kamu",
				"- !tagall / elaina tagall : mention semua anggota grup",
				"- !rvo : buka media sekali lihat (reply ke pesannya)",
				"- ba / kirim gambar blue archive : gambar BA",
				"- elaina hijabin : berhijabkan gambar (kirim/quote gambar)",
				"- elaina vn <teks> : kirim voice note",
				"- kirim gambar + sebut '" + r.cfg.Trigger + "' : analisis gambar",
				"- vn sebut 'elaina' : transkrip & jawab",
				"- kirim link TikTok : unduh via TikWM",
				"- elaina brat <teks> : buat sticker brat",
				"- elaina buatin gambar <prompt> / !gambar <prompt> : generate gambar AI",
				"- !elaina persona elaina1|elaina2 : pilih persona AI (persist)",
				"- !elaina mode pro on|off : aktifkan Mode Pro (persist)",
				"",
				"Tips: katakan \"panggil aku [nama]\" supaya aku ingat namamu!",
			}
			replyText(context.Background(), client, m, strings.Join(lines, "\n"))
			return
		case "whoami":
			userName, _ := memory.GetUserName(senderJID)
			whoamiText := "Sender: " + m.Info.Sender.String() + "\nChat  : " + m.Info.Chat.String()
			if userName != "" {
				whoamiText += "\nNama tersimpan: " + userName
			}
			replyText(context.Background(), client, m, whoamiText)
			return
		case "anime":
			if r.anime != nil && r.anime.TryHandle(client, m, origTxt) {
				return
			}
		case "peraturan":
			if r.peraturan != nil && r.peraturan.TryCommand(client, m, rest, isOwner) {
				return
			}
		case "elaina":
			after := strings.TrimSpace(strings.TrimPrefix(txt, "!elaina"))
			parts := strings.Fields(after)
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
				replyText(context.Background(), client, m, "Persona disetel ke "+p+" untuk chat ini.")
				return
			}
			if len(parts) >= 3 && strings.EqualFold(parts[0], "mode") && strings.EqualFold(parts[1], "pro") {
				on := strings.EqualFold(parts[2], "on") || strings.EqualFold(parts[2], "enable")
				_ = r.store.SetPro(m.Info.Chat.String(), on)
				if on {
					replyText(context.Background(), client, m, "Mode Pro diaktifkan (persist).")
				} else {
					replyText(context.Background(), client, m, "Mode Pro dimatikan (persist).")
				}
				return
			}
			replyText(context.Background(), client, m, "Gunakan: !elaina persona elaina1|elaina2  atau  !elaina mode pro on|off")
			return
		}
	}

	if r.peraturan != nil {
		r.peraturan.HandleMessage(client, m, origTxt)
	}

	cmd, _, isCmd := parseBang(origTxt)
	isGroup := to.Server == types.GroupServer
	hasTrig := r.reTrig.MatchString(origTxt)
	isTagAllCmd := isCmd && strings.EqualFold(cmd, "tagall")

	hasQuoted := quotedImg || quotedAud || quotedText != ""
	if hasQuoted && !hasTrig && !isCmd {
		return
	}

	hasImage := m.Message.ImageMessage != nil
	hasVideo := m.Message.VideoMessage != nil
	if isGroup && (hasImage || hasVideo) && !hasTrig && !isCmd {
		return
	}

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

	allowRvoTagall := !isGroup || hasTrig || isTagAllCmd
	if allowRvoTagall {
		if r.rvo.TryHandle(client, m, txt) {
			return
		}
		if r.tall.TryHandle(client, m, txt) {
			return
		}
	}

	hasTikTokLink := len(dl.DetectTikTokURLs(tiktokText)) > 0

	allowTikTok := !isGroup || hasTrigTikTok || hasTikTokLink
	if allowTikTok && r.tiktok.TryHandle(tiktokText, to) {
		return
	}

	allowNonCommand := !isGroup || hasTrig
	if allowNonCommand {
		if r.ba.TryHandleText(context.Background(), client, m, txt, isOwner) {
			return
		}
		if r.hijab.TryHandle(client, m, txt, isOwner, r.reTrig) {
			return
		}

		// PRIORITAS BRAT STICKER - cek sebelum sticker biasa
		if r.brat.TryHandle(client, m, txt, isOwner) {
			return
		}

		// PRIORITASKAN STICKER SEBELUM VISION
		if r.stik.TryHandleTo(client, m.Info.Chat, m.Message, txt) {
			return
		}

		// Baru cek vision setelah sticker tidak match
		if r.vis.TryHandle(client, m, txt, isOwner) {
			return
		}

		if r.anime != nil && r.anime.TryHandle(client, m, txt) {
			return
		}

		if r.tts.TryHandle(client, m, txt) {
			return
		}
	}

	if r.vnote.TryHandle(client, m, isOwner) {
		return
	}

	if quotedText != "" && r.reTrig.MatchString(origTxt) {
		after := strings.TrimSpace(r.reTrig.ReplaceAllString(origTxt, ""))
		if after == "" || reReplyCue.MatchString(after) {
			txt = quotedText
		} else {
			txt = after + "\n\nKonteks (pesan yang di-reply): " + quotedText
		}
	}

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

	if strings.TrimSpace(txt) == "" {
		return
	}

	// Prioritas: Cek apakah ini permintaan perubahan nama SEBELUM masuk ke LLM
	if name, isNameRequest := memory.DetectNameRequest(txt); isNameRequest {
		if err := memory.SetUserName(senderJID, name); err == nil {
			reply := "*Oke! Mulai sekarang aku akan memanggilmu " + name + "* ✨\n\n_Senang berkenalan denganmu, " + name + "!_ Aku Elaina, penyihir cantik dan berbakat~ 🌟"
			txtOut, mentions := r.owner.Decorate(isOwner, reply)
			replyTextMention(context.Background(), client, m, txtOut, mentions)

			// Simpan interaksi ini ke memory
			_ = memory.SaveTurn(m.Info.Chat.String(), "user", txt)
			_ = memory.SaveTurn(m.Info.Chat.String(), "assistant", reply)
			return
		}
	}

	state, _ := r.store.Get(m.Info.Chat.String())

	hist, _ := memory.Load(m.Info.Chat.String())
	// Gunakan Chat JID untuk memory, Sender JID untuk nama
	ctxTxt := memory.BuildContext(hist, txt, senderJID)

	// Pass senderJID ke AskAsPersona untuk nama
	reply := llm.AskAsPersona(r.cfg, state.Persona, state.Pro, ctxTxt, senderJID, time.Now())

	_ = memory.SaveTurn(m.Info.Chat.String(), "user", txt)
	_ = memory.SaveTurn(m.Info.Chat.String(), "assistant", reply)

	txtOut, mentions := r.owner.Decorate(isOwner, reply)
	replyTextMention(context.Background(), client, m, txtOut, mentions)
}

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
