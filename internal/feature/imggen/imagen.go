package imggen

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types/events"
	pbf "google.golang.org/protobuf/proto"

	"wa-elaina/internal/config"
)

type Handler struct {
	cfg     config.Config
	apiKeys []string
	reImg   *regexp.Regexp
	client  *http.Client
}

type GeminiRequest struct {
	Contents           []Content           `json:"contents"`
	GenerationConfig   *GenerationConfig   `json:"generationConfig,omitempty"`
}

type Content struct {
	Parts []Part `json:"parts"`
}

type Part struct {
	Text string `json:"text"`
}

type GenerationConfig struct {
	ResponseModalities []string `json:"responseModalities"`
}

type GeminiResponse struct {
	Candidates []Candidate `json:"candidates"`
	Error      *ErrorResp  `json:"error,omitempty"`
}

type Candidate struct {
	Content ResponseContent `json:"content"`
}

type ResponseContent struct {
	Parts []ResponsePart `json:"parts"`
}

type ResponsePart struct {
	Text       string      `json:"text,omitempty"`
	InlineData *InlineData `json:"inlineData,omitempty"`
}

type InlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type ErrorResp struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func New(cfg config.Config) *Handler {
	// Parse API keys dari environment variable
	geminiAPIKey := os.Getenv("GEMINI_API_KEY")
	if geminiAPIKey == "" {
		geminiAPIKey = ""
	}
	
	apiKeys := strings.Split(geminiAPIKey, ",")
	for i := range apiKeys {
		apiKeys[i] = strings.TrimSpace(apiKeys[i])
	}

	// Regex untuk detect image generation request
	reImg := regexp.MustCompile(`(?i)(elaina\s+buatin\s+gambar|!gambar)`)

	return &Handler{
		cfg:     cfg,
		apiKeys: apiKeys,
		reImg:   reImg,
		client:  &http.Client{Timeout: 60 * time.Second},
	}
}

func (h *Handler) TryHandle(client *whatsmeow.Client, m *events.Message, txt string, isOwner bool) bool {
	if !h.reImg.MatchString(txt) {
		return false
	}

	// Extract prompt dari text
	prompt := h.extractPrompt(txt)
	if prompt == "" {
		h.replyError(client, m, "Prompt gambar kosong. Contoh: elaina buatin gambar kucing lucu")
		return true
	}

	// Generate image
	go h.generateImage(client, m, prompt)
	return true
}

func (h *Handler) extractPrompt(txt string) string {
	// Remove trigger words dan ambil sisa text sebagai prompt
	cleaned := h.reImg.ReplaceAllString(txt, "")
	return strings.TrimSpace(cleaned)
}

func (h *Handler) generateImage(client *whatsmeow.Client, m *events.Message, prompt string) {
	// Try each API key until success
	for i, apiKey := range h.apiKeys {
		if apiKey == "" {
			continue
		}

		imageData, err := h.callGeminiAPI(apiKey, prompt)
		if err == nil && len(imageData) > 0 {
			h.sendImage(client, m, imageData, prompt)
			return
		}

		// Log error dan coba API key berikutnya
		if i < len(h.apiKeys)-1 {
			time.Sleep(1 * time.Second) // Brief delay before trying next key
		}
	}

	// Semua API key gagal
	h.replyError(client, m, "Gagal generate gambar. Semua API key limit atau error.")
}

func (h *Handler) callGeminiAPI(apiKey, prompt string) ([]byte, error) {
	// Menggunakan Gemini 2.0 Flash Preview Image Generation
	url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/gemini-2.0-flash-preview-image-generation:generateContent?key=%s", apiKey)

	// Enhanced prompt untuk image generation
	enhancedPrompt := fmt.Sprintf("Generate a high-quality image: %s", prompt)

	reqBody := GeminiRequest{
		Contents: []Content{{
			Parts: []Part{{
				Text: enhancedPrompt,
			}},
		}},
		GenerationConfig: &GenerationConfig{
			ResponseModalities: []string{"TEXT", "IMAGE"},
		},
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	resp, err := h.client.Post(url, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("API error: %d - %s", resp.StatusCode, string(body))
	}

	var genResp GeminiResponse
	if err := json.Unmarshal(body, &genResp); err != nil {
		return nil, err
	}

	if genResp.Error != nil {
		return nil, fmt.Errorf("Gemini error: %s", genResp.Error.Message)
	}

	if len(genResp.Candidates) == 0 {
		return nil, fmt.Errorf("no candidates generated")
	}

	// Extract image dari response
	for _, candidate := range genResp.Candidates {
		for _, part := range candidate.Content.Parts {
			if part.InlineData != nil && part.InlineData.Data != "" {
				// Decode base64 image data
				imageData, err := base64.StdEncoding.DecodeString(part.InlineData.Data)
				if err != nil {
					return nil, fmt.Errorf("failed to decode image data: %v", err)
				}
				return imageData, nil
			}
		}
	}

	return nil, fmt.Errorf("no image data found in response")
}

func (h *Handler) sendImage(client *whatsmeow.Client, m *events.Message, imageData []byte, prompt string) {
	// Upload image ke WhatsApp
	uploaded, err := client.Upload(context.Background(), imageData, whatsmeow.MediaImage)
	if err != nil {
		h.replyError(client, m, "Gagal upload gambar")
		return
	}

	// Send image message
	ci := &waProto.ContextInfo{
		StanzaID:      pbf.String(m.Info.ID),
		QuotedMessage: m.Message,
		Participant:   pbf.String(m.Info.Sender.String()),
		RemoteJID:     pbf.String(m.Info.Chat.String()),
	}

	imgMsg := &waProto.ImageMessage{
		URL:           pbf.String(uploaded.URL),
		DirectPath:    pbf.String(uploaded.DirectPath),
		MediaKey:      uploaded.MediaKey,
		Mimetype:      pbf.String("image/jpeg"),
		FileEncSHA256: uploaded.FileEncSHA256,
		FileSHA256:    uploaded.FileSHA256,
		FileLength:    pbf.Uint64(uint64(len(imageData))),
		Caption:       pbf.String(fmt.Sprintf("🎨 Generated: %s", prompt)),
		ContextInfo:   ci,
	}

	_, _ = client.SendMessage(context.Background(), m.Info.Chat, &waProto.Message{
		ImageMessage: imgMsg,
	})
}

func (h *Handler) replyError(client *whatsmeow.Client, m *events.Message, errMsg string) {
	ci := &waProto.ContextInfo{
		StanzaID:      pbf.String(m.Info.ID),
		QuotedMessage: m.Message,
		Participant:   pbf.String(m.Info.Sender.String()),
		RemoteJID:     pbf.String(m.Info.Chat.String()),
	}

	_, _ = client.SendMessage(context.Background(), m.Info.Chat, &waProto.Message{
		ExtendedTextMessage: &waProto.ExtendedTextMessage{
			Text:        pbf.String("❌ " + errMsg),
			ContextInfo: ci,
		},
	})
}