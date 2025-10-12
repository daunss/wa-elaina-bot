package peraturan

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	pbf "google.golang.org/protobuf/proto"

	"wa-elaina/internal/db"
	"wa-elaina/internal/llm"
)

const redeemKeyword = "mengurangi warn"
const warnLimit = 5

type Handler struct {
	store   *db.Store
	mod     *llm.ModerationClient
	botName string
}

func New(store *db.Store) *Handler {
	mod := llm.NewModerationClientFromEnv()
	if store == nil {
		return nil
	}
	return &Handler{
		store:   store,
		mod:     mod,
		botName: botNameFromEnv(),
	}
}

func (h *Handler) Ready() bool {
	return h != nil && h.mod != nil && h.mod.Ready()
}

func (h *Handler) TryCommand(cli *whatsmeow.Client, m *events.Message, args string, isOwner bool) bool {
	if h == nil || cli == nil || m == nil {
		return false
	}
	if !h.mod.Ready() {
		h.replyText(cli, m, "PERATURAN_APIKEY belum diatur.")
		return true
	}
	if m.Info.Chat.Server != types.GroupServer {
		h.replyText(cli, m, "Perintah peraturan hanya berlaku di grup.")
		return true
	}

	canAdmin := isOwner || h.isAdmin(cli, m.Info.Chat, m.Info.Sender)
	if !canAdmin {
		h.replyText(cli, m, "Hanya admin grup atau owner bot yang bisa mengatur fitur peraturan.")
		return true
	}

	sub := strings.Fields(strings.ToLower(strings.TrimSpace(args)))
	if len(sub) == 0 {
		h.replyText(cli, m, "Gunakan: !peraturan on|off|sync|status|rules|clear @user\nUntuk mengurangi warn: sebut nama bot lalu tulis `saya mau mengurangi warn` dan ucapkan `subhanallah` tepat 5 kali.")
		return true
	}

	switch sub[0] {
	case "on":
		return h.enable(cli, m)
	case "off":
		return h.disable(cli, m)
	case "sync", "reload":
		return h.sync(cli, m)
	case "status":
		return h.status(cli, m)
	case "rules":
		return h.showRules(cli, m)
	case "clear":
		return h.clearWarn(cli, m, sub[1:])
	default:
		h.replyText(cli, m, "Perintah tidak dikenal. Gunakan: !peraturan on|off|sync|status|rules|clear @user\nPengurangan warn: sebut nama bot + \"saya mau mengurangi warn\" + `subhanallah` x5.")
		return true
	}
}

func (h *Handler) HandleMessage(cli *whatsmeow.Client, m *events.Message, text string) {
	if h == nil || cli == nil || m == nil || m.Info.Chat.Server != types.GroupServer {
		return
	}
	if !h.mod.Ready() {
		return
	}
	state, err := h.store.GetPeraturanState(m.Info.Chat.String())
	if err != nil || !state.Enabled || strings.TrimSpace(state.Rules) == "" {
		return
	}
	content := strings.TrimSpace(text)
	if content == "" {
		return
	}

	lower := strings.ToLower(content)
	isRedeem := strings.Contains(lower, strings.ToLower(h.botName)) &&
		(strings.Contains(lower, redeemKeyword) || strings.Contains(lower, "kurangi warn") || strings.Contains(lower, "kurangin warn"))

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	if isRedeem {
		h.handleRedeem(ctx, cli, m, state, content)
		return
	}

	h.handleWarn(ctx, cli, m, state, content)
}

func (h *Handler) handleWarn(ctx context.Context, cli *whatsmeow.Client, m *events.Message, state db.PeraturanState, content string) {
	res, err := h.mod.Evaluate(ctx, llm.ModerationInput{
		Mode:    llm.ModerationModeWarn,
		Rules:   state.Rules,
		BotName: h.botName,
		Message: content,
		UserID:  m.Info.Sender.User,
	})
	if err != nil {
		log.Printf("[PERATURAN] evaluate warn error: %v", err)
		return
	}
	if !res.Violation {
		return
	}

	reason := strings.TrimSpace(res.Reason)
	if reason == "" {
		reason = "Melanggar aturan grup."
	}

	h.revokeMessage(cli, m)

	rec, err := h.store.AddWarn(m.Info.Chat.String(), m.Info.Sender.String(), reason)
	if err != nil {
		log.Printf("[PERATURAN] add warn error: %v", err)
		return
	}
	warnText := fmt.Sprintf("*Peringatan %d/%d untuk @%s*\nAlasan: %s", rec.Count, warnLimit, m.Info.Sender.User, reason)
	h.sendMention(cli, m, warnText, []types.JID{m.Info.Sender})

	if rec.Count >= warnLimit {
		target := m.Info.Sender.ToNonAD()
		if _, err := cli.UpdateGroupParticipants(m.Info.Chat, []types.JID{target}, whatsmeow.ParticipantChangeRemove); err != nil {
			log.Printf("[PERATURAN] gagal keluarkan %s: %v", m.Info.Sender.String(), err)
			return
		}
		_ = h.store.ClearWarns(m.Info.Chat.String(), m.Info.Sender.String())
		h.sendMention(cli, m, fmt.Sprintf("*%s dikeluarkan karena melebihi batas peringatan.*", "@"+m.Info.Sender.User), []types.JID{m.Info.Sender})
	}
}

func (h *Handler) handleRedeem(ctx context.Context, cli *whatsmeow.Client, m *events.Message, state db.PeraturanState, content string) {
	res, err := h.mod.Evaluate(ctx, llm.ModerationInput{
		Mode:    llm.ModerationModeRedeem,
		Rules:   state.Rules,
		BotName: h.botName,
		Message: content,
		UserID:  m.Info.Sender.User,
	})
	if err != nil {
		log.Printf("[PERATURAN] evaluate redeem error: %v", err)
		return
	}
	if !res.Redeem {
		return
	}
	rec, err := h.store.DecrementWarn(m.Info.Chat.String(), m.Info.Sender.String())
	if err != nil {
		log.Printf("[PERATURAN] decrement warn error: %v", err)
		return
	}
	if rec.Count <= 0 {
		h.sendMention(cli, m, fmt.Sprintf("*%s, warn kamu sudah 0. Tetap jaga kedisiplinan ya.*", "@"+m.Info.Sender.User), []types.JID{m.Info.Sender})
		return
	}
	h.sendMention(cli, m, fmt.Sprintf("*Warn kamu berkurang menjadi %d/%d.*", rec.Count, warnLimit), []types.JID{m.Info.Sender})
}

func (h *Handler) enable(cli *whatsmeow.Client, m *events.Message) bool {
	info, err := cli.GetGroupInfo(m.Info.Chat)
	if err != nil {
		h.replyText(cli, m, "Gagal mengambil info grup: "+err.Error())
		return true
	}
	desc := strings.TrimSpace(info.GroupTopic.Topic)
	if desc == "" {
		h.replyText(cli, m, "Deskripsi grup kosong. Atur deskripsi berisi aturan terlebih dahulu.")
		return true
	}
	rules := sanitizeRules(desc)
	if err := h.store.SetPeraturanState(m.Info.Chat.String(), true, rules); err != nil {
		h.replyText(cli, m, "Gagal menyimpan aturan: "+err.Error())
		return true
	}
	h.replyText(cli, m, "Fitur peraturan aktif.\nAturan tersimpan sebanyak "+fmt.Sprint(len(strings.Split(rules, "\n")))+" baris.")
	return true
}

func (h *Handler) disable(cli *whatsmeow.Client, m *events.Message) bool {
	state, _ := h.store.GetPeraturanState(m.Info.Chat.String())
	if !state.Enabled {
		h.replyText(cli, m, "Fitur peraturan sudah nonaktif.")
		return true
	}
	if err := h.store.SetPeraturanState(m.Info.Chat.String(), false, state.Rules); err != nil {
		h.replyText(cli, m, "Gagal menonaktifkan: "+err.Error())
		return true
	}
	h.replyText(cli, m, "Fitur peraturan dinonaktifkan.")
	return true
}

func (h *Handler) sync(cli *whatsmeow.Client, m *events.Message) bool {
	info, err := cli.GetGroupInfo(m.Info.Chat)
	if err != nil {
		h.replyText(cli, m, "Gagal mengambil info grup: "+err.Error())
		return true
	}
	desc := strings.TrimSpace(info.GroupTopic.Topic)
	if desc == "" {
		h.replyText(cli, m, "Deskripsi grup kosong.")
		return true
	}
	if err := h.store.SetPeraturanState(m.Info.Chat.String(), true, sanitizeRules(desc)); err != nil {
		h.replyText(cli, m, "Gagal menyinkronkan aturan: "+err.Error())
		return true
	}
	h.replyText(cli, m, "Aturan diperbarui dari deskripsi grup.")
	return true
}

func (h *Handler) status(cli *whatsmeow.Client, m *events.Message) bool {
	state, err := h.store.GetPeraturanState(m.Info.Chat.String())
	if err != nil {
		h.replyText(cli, m, "Gagal memuat status: "+err.Error())
		return true
	}
	status := "Nonaktif"
	if state.Enabled {
		status = "Aktif"
	}
	builder := strings.Builder{}
	builder.WriteString(fmt.Sprintf("*Status:* %s\n", status))
	if state.Rules != "" {
		builder.WriteString("*Aturan tersimpan:* \n")
		lines := strings.Split(state.Rules, "\n")
		max := len(lines)
		if max > 6 {
			max = 6
		}
		for i := 0; i < max; i++ {
			builder.WriteString("- ")
			builder.WriteString(lines[i])
			builder.WriteString("\n")
		}
		if len(lines) > max {
			builder.WriteString(fmt.Sprintf("... (%d baris lagi)\n", len(lines)-max))
		}
	}
	warns, err := h.store.ListWarns(m.Info.Chat.String())
	if err == nil && len(warns) > 0 {
		builder.WriteString("\n*Daftar warn teratas:*\n")
		limit := len(warns)
		if limit > 5 {
			limit = 5
		}
		for i := 0; i < limit; i++ {
			builder.WriteString(fmt.Sprintf("%d. %s - %d/%d\n", i+1, warns[i].User, warns[i].Count, warnLimit))
		}
	}
	h.replyText(cli, m, builder.String())
	return true
}

func (h *Handler) showRules(cli *whatsmeow.Client, m *events.Message) bool {
	state, err := h.store.GetPeraturanState(m.Info.Chat.String())
	if err != nil {
		h.replyText(cli, m, "Gagal memuat aturan: "+err.Error())
		return true
	}
	if strings.TrimSpace(state.Rules) == "" {
		h.replyText(cli, m, "Belum ada aturan tersimpan.")
		return true
	}
	h.replyText(cli, m, "*Aturan grup:*\n"+state.Rules)
	return true
}

func (h *Handler) clearWarn(cli *whatsmeow.Client, m *events.Message, args []string) bool {
	target := h.extractMention(m)
	if target == "" {
		h.replyText(cli, m, "Sertakan mention pengguna yang ingin kamu hapus warn-nya.")
		return true
	}
	if err := h.store.ClearWarns(m.Info.Chat.String(), target); err != nil {
		h.replyText(cli, m, "Gagal menghapus warn: "+err.Error())
		return true
	}
	h.replyText(cli, m, fmt.Sprintf("Warn untuk %s direset.", target))
	return true
}

func (h *Handler) extractMention(m *events.Message) string {
	xt := m.Message.GetExtendedTextMessage()
	if xt == nil || xt.GetContextInfo() == nil {
		return ""
	}
	mentions := xt.GetContextInfo().GetMentionedJid()
	if len(mentions) == 0 {
		return ""
	}
	return mentions[0]
}

func (h *Handler) isAdmin(cli *whatsmeow.Client, group, sender types.JID) bool {
	info, err := cli.GetGroupInfo(group)
	if err != nil || info == nil {
		return false
	}
	for _, p := range info.Participants {
		if p.JID.String() == sender.String() {
			return p.IsAdmin || p.IsSuperAdmin
		}
	}
	return false
}

func (h *Handler) replyText(cli *whatsmeow.Client, m *events.Message, msg string) {
	ctx := context.Background()
	ci := &waProto.ContextInfo{
		StanzaID:      pbf.String(m.Info.ID),
		QuotedMessage: m.Message,
		Participant:   pbf.String(m.Info.Sender.String()),
		RemoteJID:     pbf.String(m.Info.Chat.String()),
	}
	message := &waProto.Message{
		ExtendedTextMessage: &waProto.ExtendedTextMessage{
			Text:        pbf.String(msg),
			ContextInfo: ci,
		},
	}
	if _, err := cli.SendMessage(ctx, m.Info.Chat, message); err != nil {
		log.Printf("[PERATURAN] gagal kirim balasan: %v", err)
	}
}

func (h *Handler) sendMention(cli *whatsmeow.Client, m *events.Message, text string, mentions []types.JID) {
	ctx := context.Background()
	ci := &waProto.ContextInfo{
		StanzaID:      pbf.String(m.Info.ID),
		QuotedMessage: m.Message,
		Participant:   pbf.String(m.Info.Sender.String()),
		RemoteJID:     pbf.String(m.Info.Chat.String()),
	}
	for _, j := range mentions {
		ci.MentionedJID = append(ci.MentionedJID, j.String())
	}
	msg := &waProto.Message{
		ExtendedTextMessage: &waProto.ExtendedTextMessage{
			Text:        pbf.String(text),
			ContextInfo: ci,
		},
	}
	if _, err := cli.SendMessage(ctx, m.Info.Chat, msg); err != nil {
		log.Printf("[PERATURAN] gagal kirim mention: %v", err)
	}
}

func sanitizeRules(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		l := strings.TrimSpace(line)
		if l == "" {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

func botNameFromEnv() string {
	if v := strings.TrimSpace(os.Getenv("BOT_NAME")); v != "" {
		return v
	}
	return "Bot"
}

func (h *Handler) revokeMessage(cli *whatsmeow.Client, m *events.Message) {
	if cli == nil || m == nil || m.Info.ID == "" {
		return
	}
	msg := cli.BuildRevoke(m.Info.Chat, m.Info.Sender, types.MessageID(m.Info.ID))
	if msg == nil {
		return
	}
	if _, err := cli.SendMessage(context.Background(), m.Info.Chat, msg); err != nil {
		log.Printf("[PERATURAN] gagal hapus pesan: %v", err)
	}
}
