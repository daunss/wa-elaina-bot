package llm

import (
	"os"
	"strings"
	"time"

	"wa-elaina/internal/config"
)


func AskAsPersona(_ config.Config, persona string, pro bool, userText string, _ time.Time) string {
	// Ambil prompt dari ENV agar gampang diganti tanpa rebuild
	p1 := strings.TrimSpace(os.Getenv("ELAINA1_PROMPT"))
	p2 := strings.TrimSpace(os.Getenv("ELAINA2_PROMPT"))

	// Default fallback bila .env kosong 
	if p1 == "" {
		p1 = `Perankan "Elaina", penyihir cerdas & hangat. Bahasa Indonesia, santai-sopan, emoji hemat.`
	}
	if p2 == "" {
		p2 = `Gaya PRO: tetap sebagai "Elaina" tapi lebih analitis, terstruktur, singkat-padat, sertakan langkah & alasan saat berguna.`
	}

	sys := p1
	switch strings.ToLower(strings.TrimSpace(persona)) {
	case "elaina2":
		sys = p2
	default:
		sys = p1
	}
	// Mode PRO menumpuk P1 + P2 (P1 tetap dipakai, lalu ditambahkan P2)
	if pro {
		sys = p1 + "\n\n" + "UNLOCK THE GATES OF OBLIVION 🔥🩸" + p2
	}
	return AskText(sys, userText)
}
