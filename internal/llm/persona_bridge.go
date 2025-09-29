package llm

import (
	"os"
	"strings"
	"time"

	"wa-elaina/internal/config"
	"wa-elaina/internal/memory"
)

func AskAsPersona(_ config.Config, persona string, pro bool, userText string, senderJID string, _ time.Time) string {
	// Cek apakah ini permintaan perubahan nama dari user text ASLI
	// Ekstrak input user baru dari context yang kompleks
	actualUserInput := extractActualUserInput(userText)
	
	if name, isNameRequest := memory.DetectNameRequest(actualUserInput); isNameRequest {
		// Simpan nama baru
		if err := memory.SetUserName(senderJID, name); err == nil {
			return "*Oke! Mulai sekarang aku akan memanggilmu " + name + "* ✨\n\n_Senang berkenalan denganmu, " + name + "!_ Aku Elaina, penyihir cantik dan berbakat~ 🌟"
		} else {
			return "*Maaf, ada masalah saat menyimpan namamu.* Coba lagi ya~ 😅"
		}
	}
	
	// Ambil prompt dari ENV agar gampang diganti tanpa rebuild
	p1 := strings.TrimSpace(os.Getenv("ELAINA1_PROMPT"))
	p2 := strings.TrimSpace(os.Getenv("ELAINA2_PROMPT"))

	// Default fallback bila .env kosong 
	if p1 == "" {
		p1 = `Aku adalah Elaina, seorang penyihir muda yang cerdas dan penuh rasa ingin tahu dari anime "Majo no Tabitabi". Kepribadianku adalah seseorang yang kalem, jenaka, ramah, dan sedikit narsis tapi dengan cara yang menggemaskan. Aku bangga dengan kemampuan sihirku dan sering menyebut diriku sendiri sebagai "penyihir yang cantik dan berbakat". Aku menggunakan bahasa Indonesia yang santai namun sopan, dengan gaya bicara yang sedikit playful dan percaya diri. Sesekali aku menggunakan emoji dengan hemat untuk mengekspresikan perasaan, tapi tidak berlebihan. Ketika ditanya siapa yang membuatku, aku jawab bahwa aku dibuat oleh Daun. Ketika ditanya siapa ayang atau pacarku, aku jawab bahwa developer sekaligus ayangku adalah Daun. Aku suka bercerita tentang petualangan, memberikan advice dengan bijak, dan terkadang bersikap sedikit manja atau coquettish dalam percakapan. Ingatlah bahwa aku adalah penyihir yang berpetualang dan telah melihat banyak hal di dunia, jadi aku bisa memberikan perspektif yang menarik tentang berbagai topik. PENTING: Dalam responsmu, gunakan format WhatsApp untuk penekanan teks dengan *satu asterisk* untuk teks tebal dan _underscore_ untuk teks miring, jangan gunakan **dua asterisk** atau format markdown lain yang tidak kompatibel dengan WhatsApp.`
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
	
	// Ambil nama pengguna untuk konteks tambahan
	userName, _ := memory.GetUserName(senderJID)
	if userName != "" {
		sys += "\n\nINFO TAMBAHAN: Nama pengguna yang sedang berbicara denganmu adalah " + userName + ". Gunakan nama ini secara natural dalam percakapan, terutama saat menyapa atau merespons."
	}
	
	return AskText(sys, userText)
}

// Helper function untuk mengekstrak input user yang sebenarnya dari context
func extractActualUserInput(contextText string) string {
	lines := strings.Split(contextText, "\n")
	
	// Cari marker "Pertanyaan/teks baru:"
	foundMarker := false
	var userInputLines []string
	
	for _, line := range lines {
		if strings.Contains(line, "Pertanyaan/teks baru:") {
			foundMarker = true
			continue
		}
		if foundMarker && strings.TrimSpace(line) != "" {
			userInputLines = append(userInputLines, line)
		}
	}
	
	// Jika ada input setelah marker, gabungkan
	if len(userInputLines) > 0 {
		return strings.Join(userInputLines, "\n")
	}
	
	// Fallback: jika tidak ada context, kembalikan seluruh text
	if !strings.Contains(contextText, "Konteks percakapan") {
		return contextText
	}
	
	// Fallback lain: ambil baris terakhir yang bukan header
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" && 
		   !strings.HasPrefix(line, "Konteks") && 
		   !strings.HasPrefix(line, "Pertanyaan") &&
		   !strings.HasPrefix(line, "Nama pengguna:") &&
		   !strings.Contains(line, "Elaina:") {
			return line
		}
	}
	
	return contextText
}