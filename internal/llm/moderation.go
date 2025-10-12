package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const moderationModel = "gemini-2.0-flash-lite"

type ModerationMode string

const (
	ModerationModeWarn   ModerationMode = "WARN"
	ModerationModeRedeem ModerationMode = "REDEEM"
)

type ModerationClient struct {
	apiKey string
	system string
	httpc  *http.Client
}

type ModerationInput struct {
	Mode    ModerationMode
	Rules   string
	BotName string
	Message string
	UserID  string
}

type ModerationResult struct {
	Violation bool   `json:"violation"`
	Reason    string `json:"reason"`
	Redeem    bool   `json:"redeem"`
}

func NewModerationClientFromEnv() *ModerationClient {
	key := strings.TrimSpace(os.Getenv("PERATURAN_APIKEY"))
	system := strings.TrimSpace(os.Getenv("PERATURAN_PROMPT"))
	if system == "" {
		system = `Kamu adalah moderator WhatsApp. Evaluasi pesan berdasarkan aturan grup yang diberikan.
Selalu balas dengan JSON valid dengan format {"violation":bool,"reason":string,"redeem":bool}.
- Mode WARN: tentukan apakah pesan melanggar aturan. Jika ya, violation=true dan reason istimewa singkat (<=120 karakter) dalam Bahasa Indonesia sopan.
- Mode REDEEM: Tentukan apakah pesan adalah permintaan pengurangan warn yang sah dengan menyebut nama bot dan mengucap "subhanallah" tepat 5 kali. Jika sah, set redeem=true. Selain itu violation=false.
- Jika tidak melanggar, violation=false dan reason kosong.
Jangan tambahkan teks lain selain JSON.`
	}
	return &ModerationClient{
		apiKey: key,
		system: system,
		httpc:  &http.Client{Timeout: 45 * time.Second},
	}
}

func (c *ModerationClient) Ready() bool { return strings.TrimSpace(c.apiKey) != "" }

func (c *ModerationClient) Evaluate(ctx context.Context, in ModerationInput) (ModerationResult, error) {
	if !c.Ready() {
		return ModerationResult{}, errors.New("PERATURAN_APIKEY belum diatur")
	}
	payload := map[string]any{
		"system_instruction": map[string]any{
			"role": "system",
			"parts": []map[string]string{
				{"text": c.system},
			},
		},
		"contents": []map[string]any{
			{
				"role": "user",
				"parts": []map[string]string{
					{"text": buildModerationPrompt(in)},
				},
			},
		},
	}
	respText, status, err := c.send(ctx, payload)
	if err != nil {
		return ModerationResult{}, err
	}
	if status >= 300 {
		return ModerationResult{}, errors.New(respText)
	}
	clean := normalizeModerationJSON(respText)
	var res ModerationResult
	if err := json.Unmarshal([]byte(clean), &res); err != nil {
		return ModerationResult{}, fmt.Errorf("parse moderation response: %w (raw=%s)", err, respText)
	}
	return res, nil
}

func (c *ModerationClient) send(ctx context.Context, body any) (string, int, error) {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://generativelanguage.googleapis.com/v1beta/models/"+moderationModel+":generateContent?key="+c.apiKey,
		bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpc.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	var out struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if json.Unmarshal(rb, &out) == nil {
		if len(out.Candidates) > 0 && len(out.Candidates[0].Content.Parts) > 0 {
			return strings.TrimSpace(out.Candidates[0].Content.Parts[0].Text), resp.StatusCode, nil
		}
	}
	return strings.TrimSpace(string(rb)), resp.StatusCode, nil
}

func buildModerationPrompt(in ModerationInput) string {
	var sb strings.Builder
	sb.WriteString("Mode: ")
	sb.WriteString(string(in.Mode))
	sb.WriteString("\nBot: ")
	sb.WriteString(in.BotName)
	if in.UserID != "" {
		sb.WriteString("\nUser: ")
		sb.WriteString(in.UserID)
	}
	sb.WriteString("\nAturan grup:\n")
	sb.WriteString(strings.TrimSpace(in.Rules))
	sb.WriteString("\nPesan:\n")
	sb.WriteString(strings.TrimSpace(in.Message))
	sb.WriteString("\nBalas JSON sesuai format.")
	return sb.String()
}

func normalizeModerationJSON(s string) string {
	out := strings.TrimSpace(s)
	if strings.HasPrefix(out, "```") {
		out = strings.TrimPrefix(out, "```")
		out = strings.TrimSpace(out)
		if idx := strings.Index(out, "\n"); idx >= 0 {
			out = strings.TrimSpace(out[idx+1:])
		}
		if pos := strings.LastIndex(out, "```"); pos >= 0 {
			out = strings.TrimSpace(out[:pos])
		}
	}
	start := strings.Index(out, "{")
	end := strings.LastIndex(out, "}")
	if start >= 0 && end > start {
		out = out[start : end+1]
	}
	return strings.TrimSpace(out)
}
