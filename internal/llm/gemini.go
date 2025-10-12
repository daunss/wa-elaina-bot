package llm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"wa-elaina/internal/config"
)

var (
	keys []string
	idx  int
	httpc = &http.Client{ Timeout: 60 * time.Second }
)

func Init(cfg config.Config) { keys = cfg.GeminiKeys }

func getKey() string { if len(keys)==0 { return "" }; return keys[idx] }
func rotate() { if len(keys)>1 { idx=(idx+1)%len(keys) } }

func AskText(system, user string) string {
	var last string
	for i:=0; i<max(1,len(keys)); i++ {
		key := getKey(); if key=="" { return "LLM key belum diatur" }
		body := map[string]any{
			"system_instruction": map[string]any{"role":"system","parts":[]map[string]string{{"text":system}}},
			"contents": []map[string]any{{"role":"user","parts":[]map[string]string{{"text":user}}}},
		}
		s, status := send(key, body)
		if status==200 && s!="" { return s }
		last = s; rotate()
	}
	return strings.TrimSpace(last)
}

func AskTextAsElaina(user string) string {
	sys := `Perankan "Elaina", penyihir cerdas & hangat. Bahasa Indonesia, santai-sopan, emoji hemat.`
	return AskText(sys, user)
}

func AskVision(system, prompt string, img []byte, mime string) string {
	if mime=="" { mime="image/jpeg" }
	var last string
	for i:=0; i<max(1,len(keys)); i++ {
		key := getKey(); if key=="" { return "LLM key belum diatur" }
		b64 := base64.StdEncoding.EncodeToString(img)
		parts := []any{ map[string]any{"text": prompt}, map[string]any{"inlineData": map[string]any{"mimeType": mime, "data": b64}} }
		body := map[string]any{
			"system_instruction": map[string]any{"role":"system","parts":[]map[string]string{{"text":system}}},
			"contents": []map[string]any{{"role":"user","parts": parts }},
		}
		s, status := send(key, body)
		if status==200 && s!="" { return s }
		last = s; rotate()
	}
	return strings.TrimSpace(last)
}

func Transcribe(audio []byte, mime string) string {
	if mime=="" { mime="audio/ogg" }
	var last string
	for i:=0; i<max(1,len(keys)); i++ {
		key := getKey(); if key=="" { return "" }
		b64 := base64.StdEncoding.EncodeToString(audio)
		body := map[string]any{
			"system_instruction": map[string]any{"role":"system","parts":[]map[string]string{{"text":"Transkripsikan audio ke Bahasa Indonesia yang bersih."}}},
			"contents": []map[string]any{{"role":"user","parts":[]any{ map[string]any{"inlineData": map[string]any{"mimeType": mime, "data": b64}}}}},
		}
		s, status := send(key, body)
		if status==200 && s!="" { return s }
		last = s; rotate()
	}
	return strings.TrimSpace(last)
}

func send(key string, body any) (string, int) {
	endpoint := "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash-lite:generateContent?key="+key
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(b))
	req.Header.Set("Content-Type","application/json")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second); defer cancel()
	req = req.WithContext(ctx)

	resp, err := httpc.Do(req)
	if err != nil { return "LLM error: "+err.Error(), 0 }
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)

	var out struct{ Candidates []struct{ Content struct{ Parts []struct{ Text string `json:"text"` } `json:"parts"` } `json:"content"` } `json:"candidates"` }
	if json.Unmarshal(rb, &out) == nil && len(out.Candidates)>0 && len(out.Candidates[0].Content.Parts)>0 {
		return strings.TrimSpace(out.Candidates[0].Content.Parts[0].Text), resp.StatusCode
	}
	return strings.TrimSpace(string(rb)), resp.StatusCode
}
func max(a,b int) int { if a>b {return a}; return b }
