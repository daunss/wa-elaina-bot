package owner

import (
	"log"
	"os"
	"regexp"
	"strings"

	"go.mau.fi/whatsmeow/types"
)

type Detector struct {
	jid     *types.JID
	lid     *types.JID
	extras  []types.JID
	tag     string
	match   string // digits-only nomor
	debug   bool
}

func NewFromEnv() *Detector {
	d := &Detector{}
	// JID nomor
	if v := strings.TrimSpace(os.Getenv("OWNER_JID")); v != "" {
		if j, err := types.ParseJID(v); err == nil { d.jid = &j }
	} else if n := strings.TrimSpace(os.Getenv("OWNER_NUMBER")); n != "" {
		n = reNonDigit.ReplaceAllString(n, "")
		if j, err := types.ParseJID(n + "@s.whatsapp.net"); err == nil { d.jid = &j }
	}
	// LID
	if v := strings.TrimSpace(os.Getenv("OWNER_LID")); v != "" {
		if j, err := types.ParseJID(v); err == nil { d.lid = &j }
	}
	// extras
	if xs := strings.TrimSpace(os.Getenv("OWNER_IDS")); xs != "" {
		for _, s := range strings.Split(xs, ",") {
			s = strings.TrimSpace(s)
			if s == "" { continue }
			if j, err := types.ParseJID(s); err == nil { d.extras = append(d.extras, j) }
		}
	}
	d.tag = strings.TrimSpace(os.Getenv("OWNER_TAG"))
	if d.tag == "" { d.tag = "owner tercinta/sayang" }
	d.debug = strings.EqualFold(strings.TrimSpace(os.Getenv("OWNER_DEBUG")), "true")
	// digits match
	d.match = reNonDigit.ReplaceAllString(strings.TrimSpace(os.Getenv("OWNER_MATCH")), "")
	if d.match == "" && d.jid != nil { d.match = d.jid.User }
	return d
}

var reNonDigit = regexp.MustCompile(`\D+`)

func sameBare(a, b types.JID) bool { return a.User == b.User && a.Server == b.Server }

func (d *Detector) digits(s string) string { return reNonDigit.ReplaceAllString(s, "") }

func (d *Detector) IsOwner(info types.MessageInfo) bool {
	// 1) full JID number
	if d.jid != nil && (sameBare(info.Sender, *d.jid) || sameBare(info.Chat, *d.jid)) { return true }
	// 2) digits-only match (untuk toleransi LID)
	if d.match != "" {
		if d.digits(info.Sender.User) == d.match || d.digits(info.Chat.User) == d.match {
			return true
		}
	}
	// 3) LID exact
	if d.lid != nil && (sameBare(info.Sender, *d.lid) || sameBare(info.Chat, *d.lid)) { return true }
	// 4) extras
	for _, j := range d.extras {
		if sameBare(info.Sender, j) || sameBare(info.Chat, j) { return true }
	}
	return false
}

func (d *Detector) Debug(info types.MessageInfo, got bool) {
	if !d.debug { return }
	log.Printf("[OWNER-DBG] sender=%s chat=%s digits(sender)=%s digits(chat)=%s => isOwner=%t",
		info.Sender.String(), info.Chat.String(), d.digits(info.Sender.User), d.digits(info.Chat.User), got)
}

func (d *Detector) currentMention() *types.JID {
	if d.jid != nil { return d.jid }
	if d.lid != nil { return d.lid }
	if len(d.extras) > 0 { return &d.extras[0] }
	return nil
}

func (d *Detector) Decorate(isOwner bool, base string) (string, []types.JID) {
	if !isOwner { return base, nil }
	j := d.currentMention()
	if j == nil { return base, nil }
	prefix := "@" + j.User + " (" + d.tag + ")\n"
	return prefix + base, []types.JID{*j}
}
