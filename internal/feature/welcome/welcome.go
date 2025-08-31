package welcome

import (
	"context"
	"log"
	"os"
	"reflect"
	"strings"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	pbf "google.golang.org/protobuf/proto"
)

type Handler struct {
	enabled bool
	tmpl    string
}

func NewFromEnv() *Handler {
	enabled := true
	if v := strings.TrimSpace(os.Getenv("WELCOME_ENABLED")); v != "" {
		enabled = v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
	}
	tmpl := strings.TrimSpace(os.Getenv("WELCOME_TEXT"))
	if tmpl == "" {
		tmpl = "Halo {mentions}! Selamat datang di {group}. 🙂\nBaca deskripsi & patuhi aturan ya."
	}
	return &Handler{enabled: enabled, tmpl: tmpl}
}

// TryHandle: kompatibel lintas versi (ParticipantsUpdate / GroupParticipantsUpdate)
// Panggil dari router: wel.TryHandle(client, evt)
func (h *Handler) TryHandle(cli *whatsmeow.Client, evt interface{}) bool {
	if !h.enabled || evt == nil {
		return false
	}
	rv := reflect.ValueOf(evt)
	if rv.Kind() != reflect.Ptr || rv.IsNil() {
		return false
	}
	rt := rv.Type().String()
	// Hanya proses event peserta grup
	if !(strings.HasSuffix(rt, ".ParticipantsUpdate") || strings.HasSuffix(rt, ".GroupParticipantsUpdate")) {
		return false
	}
	ev := rv.Elem()
	if ev.Kind() != reflect.Struct {
		return false
	}

	// Ambil JID grup
	jidField := ev.FieldByName("JID")
	if !jidField.IsValid() {
		return false
	}
	jid, ok := jidField.Interface().(types.JID)
	if !ok {
		return false
	}

	// Ambil daftar participants
	partField := ev.FieldByName("Participants")
	if !partField.IsValid() || partField.Kind() != reflect.Slice {
		return false
	}

	// Kumpulkan yang Action == "add"
	added := make([]types.JID, 0, partField.Len())
	for i := 0; i < partField.Len(); i++ {
		p := partField.Index(i)
		if p.Kind() == reflect.Struct {
			jf := p.FieldByName("JID")
			af := p.FieldByName("Action")

			j, ok1 := jf.Interface().(types.JID)
			if !ok1 {
				continue
			}
			action := ""
			switch af.Kind() {
			case reflect.String:
				action = strings.ToLower(af.String())
			default:
				// coba String() method
				if af.CanInterface() {
					action = strings.ToLower(strings.TrimSpace(toString(af.Interface())))
				}
			}
			if action == "add" {
				added = append(added, j)
			}
		}
	}
	if len(added) == 0 {
		return false
	}

	// Mentions & mention JIDs
	mentions := make([]string, 0, len(added))
	mentionJIDs := make([]string, 0, len(added))
	for _, j := range added {
		mentions = append(mentions, "@"+j.User)
		mentionJIDs = append(mentionJIDs, j.String())
	}

	// Nama grup
	groupName := jid.String()
	if gi, err := cli.GetGroupInfo(jid); err == nil && gi != nil && gi.Name != "" {
		groupName = gi.Name
	}

	// Render template
	text := strings.ReplaceAll(h.tmpl, "{mentions}", strings.Join(mentions, " "))
	text = strings.ReplaceAll(text, "{group}", groupName)

	// ContextInfo untuk mention (tidak meng-quote system message agar kompatibel)
	ctxInfo := &waProto.ContextInfo{
		MentionedJID: mentionJIDs,
	}

	msg := &waProto.Message{
		ExtendedTextMessage: &waProto.ExtendedTextMessage{
			Text:        pbf.String(text),
			ContextInfo: ctxInfo,
		},
	}

	if _, err := cli.SendMessage(context.Background(), jid, msg); err != nil {
		log.Printf("[WELCOME] gagal kirim sambutan: %v", err)
	}
	return true
}

// Helper: ubah nilai ke string jika punya String() atau default fmt-like
func toString(v interface{}) string {
	if v == nil {
		return ""
	}
	// gunakan reflect untuk cari method String()
	val := reflect.ValueOf(v)
	if !val.IsValid() {
		return ""
	}
	// method dengan nama "String" tanpa arg dan return string
	if m := val.MethodByName("String"); m.IsValid() && m.Type().NumIn() == 0 && m.Type().NumOut() == 1 && m.Type().Out(0).Kind() == reflect.String {
		out := m.Call(nil)
		return out[0].String()
	}
	// fallback: gunakan %#v seperti fmt.Sprintf tanpa import fmt agar tidak unused
	// namun kita tetap butuh cara tanpa fmt; cukup reflect %+v-like sederhana:
	switch x := v.(type) {
	case string:
		return x
	}
	return ""
}
