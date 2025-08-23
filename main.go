
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	_ "modernc.org/sqlite"
)

var (
	geminiKey  = os.Getenv("GEMINI_API_KEY")
	sessionDB  = getenv("SESSION_PATH", "session.db")
	botName    = getenv("BOT_NAME", "Elaina")
	httpClient = &http.Client{Timeout: 45 * time.Second}
)

func main() {
	if geminiKey == "" {
		log.Fatal("GEMINI_API_KEY kosong")
	}

	// Storage sesi (SQLite)
	container, err := sqlstore.New("sqlite", "file:"+sessionDB+"?_pragma=foreign_keys(1)", nil)
	if err != nil {
		log.Fatal(err)
	}
	device := container.GetFirstDevice()
	if device == nil {
		device = container.NewDevice()
	}

	client := whatsmeow.NewClient(device, nil)

	// Event handler
	client.AddEventHandler(func(evt interface{}) {
		switch v := evt.(type) {
		case *events.Message:
			handleMessage(client, v)
		}
	})

	// Connect/login
	if client.Store.ID == nil {
		qr, _ := client.GetQRChannel(context.Background())
		if err := client.Connect(); err != nil {
			log.Fatal("connect:", err)
		}
		for e := range qr {
			switch e.Event {
			case "code":
				// Tampilkan kode untuk di-scan (gunakan QR generator online)
				log.Println("Scan QR (code):", e.Code)
			case "success":
				log.Println("Login success")
			case "timeout", "error":
				log.Println("QR event:", e.Event)
			}
		}
	} else {
		if err := client.Connect(); err != nil {
			log.Fatal("connect:", err)
		}
	}

	// HTTP endpoints
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	http.HandleFunc("/send", func(w http.ResponseWriter, r *http.Request) {
		to := r.URL.Query().Get("to")
		text := r.URL.Query().Get("text")
		if to == "" || text == "" {
			http.Error(w, "need to & text", http.StatusBadRequest)
			return
		}
		j := types.NewJID(to, types.DefaultUserServer)
		_, err := client.SendMessage(context.Background(), j, &waProto.Message{
			Conversation: proto.String(text),
		})
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Write([]byte("sent"))
	})

	log.Println("HTTP on :7860")
	log.Fatal(http.ListenAndServe(":7860", nil))
}

func handleMessage(client *whatsmeow.Client, msg *events.Message) {
	if msg.Info.MessageSource.IsFromMe {
		return
	}
	jid := msg.Info.Sender

	userText := extractText(msg)
	if userText == "" {
		return
	}

	system := fmt.Sprintf(`Kamu %s: penyihir cerdas, hangat, sedikit playful. Bahasa Indonesia santai + sopan, gunakan emoji seperlunya.
Jika fakta: akurat & singkat. Jika opini: sebutkan alasan. Hindari SARA/eksplisit/berbahaya.`, botName)

	reply, err := askGemini(system, userText)
	if err != nil {
		reply = "Ups, Elaina lagi tersandung jaringan. Coba lagi ya ✨"
	}

	if len(reply) > 3500 {
		reply = reply[:3500] + "…"
	}

	_, sendErr := client.SendMessage(context.Background(), jid.ToNonAD(), &waProto.Message{
		Conversation: proto.String(reply),
	})
	if sendErr != nil {
		log.Println("send error:", sendErr)
	}
}

func extractText(m *events.Message) string {
	if m.Message.GetConversation() != "" {
		return m.Message.GetConversation()
	}
	if ext := m.Message.GetExtendedTextMessage(); ext != nil && ext.GetText() != "" {
		return ext.GetText()
	}
	return ""
}

// ---------- Gemini ----------

func askGemini(systemPrompt, userText string) (string, error) {
	if geminiKey == "" {
		return "", fmt.Errorf("GEMINI_API_KEY kosong")
	}
	url := "https://generativelanguage.googleapis.com/v1beta/models/gemini-1.5-flash:generateContent?key=" + geminiKey

	body := map[string]any{
		"system_instruction": map[string]any{
			"role":  "system",
			"parts": []map[string]string{{"text": systemPrompt}},
		},
		"contents": []map[string]any{
			{
				"role": "user",
				"parts": []map[string]string{
					{"text": userText},
				},
			},
		},
	}

	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("gemini error %s: %s", resp.Status, string(rb))
	}

	var out struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(rb, &out); err != nil {
		return "", err
	}
	if len(out.Candidates) == 0 || len(out.Candidates[0].Content.Parts) == 0 {
		return "Maaf, aku belum punya jawaban. Coba ulangi ya~", nil
	}
	return strings.TrimSpace(out.Candidates[0].Content.Parts[0].Text), nil
}

// ---------- Utils ----------
func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
