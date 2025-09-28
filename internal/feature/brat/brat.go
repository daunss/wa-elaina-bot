package brat

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/golang/freetype"
	"golang.org/x/image/font/gofont/goregular"
	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types/events"
	pbf "google.golang.org/protobuf/proto"
)

var reBrat = regexp.MustCompile(`(?i)\b(brat)\b`)

type Handler struct {
	triggerRegex *regexp.Regexp
}

func New(triggerRegex *regexp.Regexp) *Handler {
	return &Handler{
		triggerRegex: triggerRegex,
	}
}

func (h *Handler) TryHandle(client *whatsmeow.Client, m *events.Message, text string, isOwner bool) bool {
	if !h.triggerRegex.MatchString(text) {
		return false
	}
	
	if !reBrat.MatchString(text) {
		return false
	}

	log.Printf("[BRAT] Processing: %s", text)

	stickerText := h.extractStickerText(text)
	if strings.TrimSpace(stickerText) == "" {
		stickerText = "brat"
	}

	log.Printf("[BRAT] Extracted text: '%s'", stickerText)

	// Kirim sebagai sticker tanpa watermark
	h.sendAsSticker(context.Background(), client, m, stickerText)
	return true
}

func (h *Handler) extractStickerText(text string) string {
	cleaned := h.triggerRegex.ReplaceAllString(text, " ")
	cleaned = reBrat.ReplaceAllString(cleaned, " ")
	cleaned = regexp.MustCompile(`\s+`).ReplaceAllString(strings.TrimSpace(cleaned), " ")
	
	// Jika hasil cleaning kosong, ambil text asli tanpa trigger word
	if strings.TrimSpace(cleaned) == "" {
		// Ambil text asli dan hapus hanya trigger word
		original := h.triggerRegex.ReplaceAllString(text, "")
		original = reBrat.ReplaceAllString(original, "")
		original = strings.TrimSpace(original)
		if original != "" {
			return original
		}
	}
	
	return cleaned
}

func (h *Handler) generateBratImage(text string) ([]byte, error) {
	size := 512 // Ukuran sticker standar
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	
	// Brat green background
	bratGreen := color.RGBA{142, 255, 142, 255}
	draw.Draw(img, img.Bounds(), &image.Uniform{bratGreen}, image.Point{}, draw.Src)
	
	// Setup font dengan dukungan unicode untuk emoji
	f, err := freetype.ParseFont(goregular.TTF)
	if err != nil {
		return nil, err
	}
	
	c := freetype.NewContext()
	c.SetDPI(72)
	c.SetFont(f)
	c.SetClip(img.Bounds())
	c.SetDst(img)
	c.SetSrc(image.NewUniform(color.RGBA{0, 0, 0, 255}))
	
	fontSize := h.calculateFontSize(text, size)
	c.SetFontSize(fontSize)
	
	// Draw text
	h.drawText(c, strings.ToLower(text), size, fontSize)
	
	// Generate PNG
	var pngBuf bytes.Buffer
	err = png.Encode(&pngBuf, img)
	if err != nil {
		return nil, err
	}
	
	return pngBuf.Bytes(), nil
}

func (h *Handler) calculateFontSize(text string, imageSize int) float64 {
	textLen := len(text)
	baseFontSize := 48.0
	
	switch {
	case textLen <= 5:
		baseFontSize = 60.0
	case textLen <= 10:
		baseFontSize = 48.0
	case textLen <= 20:
		baseFontSize = 36.0
	case textLen <= 40:
		baseFontSize = 28.0
	default:
		baseFontSize = 20.0
	}
	
	return baseFontSize * float64(imageSize) / 400.0
}

func (h *Handler) drawText(c *freetype.Context, text string, imageSize int, fontSize float64) {
	words := strings.Fields(text)
	lines := h.wrapTextToLines(words, fontSize, imageSize)
	
	if len(lines) == 1 && len(text) <= 12 {
		h.drawCenteredLine(c, lines[0], imageSize, fontSize)
	} else {
		h.drawMultipleLines(c, lines, imageSize, fontSize)
	}
}

func (h *Handler) wrapTextToLines(words []string, fontSize float64, imageSize int) []string {
	var lines []string
	var currentLine string
	
	charWidth := fontSize * 0.55
	maxWidth := float64(imageSize) * 0.9
	maxChars := int(maxWidth / charWidth)
	
	if maxChars < 3 {
		maxChars = 3
	}
	
	for _, word := range words {
		testLine := currentLine
		if testLine != "" {
			testLine += " "
		}
		testLine += word
		
		if len(testLine) <= maxChars {
			currentLine = testLine
		} else {
			if currentLine != "" {
				lines = append(lines, currentLine)
			}
			currentLine = word
		}
	}
	
	if currentLine != "" {
		lines = append(lines, currentLine)
	}
	
	return lines
}

func (h *Handler) drawCenteredLine(c *freetype.Context, text string, imageSize int, fontSize float64) {
	textWidth := int(float64(len(text)) * fontSize * 0.55)
	x := (imageSize - textWidth) / 2
	y := imageSize/2 + int(fontSize/4)
	
	if x < 20 {
		x = 20
	}
	
	c.DrawString(text, freetype.Pt(x, y))
}

func (h *Handler) drawMultipleLines(c *freetype.Context, lines []string, imageSize int, fontSize float64) {
	lineHeight := int(fontSize * 1.2)
	totalHeight := len(lines) * lineHeight
	startY := (imageSize-totalHeight)/2 + int(fontSize)
	startX := 20
	
	for i, line := range lines {
		y := startY + (i * lineHeight)
		if y > imageSize-20 {
			break
		}
		c.DrawString(line, freetype.Pt(startX, y))
	}
}

// Konversi PNG ke WebP tanpa watermark (diambil dari sticker.go)
func (h *Handler) toWebP(ctx context.Context, input []byte) ([]byte, error) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return nil, fmt.Errorf("ffmpeg tidak ditemukan di PATH")
	}
	
	tmpDir, _ := os.MkdirTemp("", "brat-sticker-*")
	defer os.RemoveAll(tmpDir)

	in := filepath.Join(tmpDir, "in.png")
	out := filepath.Join(tmpDir, "out.webp")
	
	if err := os.WriteFile(in, input, 0o600); err != nil {
		return nil, err
	}

	// Konversi ke WebP tanpa watermark
	args := []string{
		"-y", "-i", in,
		"-vf", "scale=512:512:force_original_aspect_ratio=decrease:flags=lanczos,pad=512:512:(ow-iw)/2:(oh-ih)/2:color=0x00000000",
		"-vcodec", "libwebp", "-lossless", "1", "-q:v", "75", "-preset", "default",
		out,
	}

	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	cmd.Stderr = &stderr
	
	if err := cmd.Run(); err != nil {
		tail := stderr.String()
		if len(tail) > 400 {
			tail = tail[len(tail)-400:]
		}
		return nil, fmt.Errorf("ffmpeg error: %s", tail)
	}

	return os.ReadFile(out)
}

// Kirim sebagai sticker (diambil dari sticker.go)
func (h *Handler) sendAsSticker(ctx context.Context, client *whatsmeow.Client, m *events.Message, text string) {
	// Timeout untuk proses
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// Generate brat image
	imageData, err := h.generateBratImage(text)
	if err != nil {
		log.Printf("[BRAT] Failed to generate image: %v", err)
		return
	}

	// Konversi ke WebP
	webpData, err := h.toWebP(ctx, imageData)
	if err != nil {
		log.Printf("[BRAT] Failed to convert to WebP: %v", err)
		return
	}

	// Upload WebP
	uploaded, err := client.Upload(ctx, webpData, whatsmeow.MediaImage)
	if err != nil {
		log.Printf("[BRAT] Upload failed: %v", err)
		return
	}

	// Context info untuk reply
	ci := &waProto.ContextInfo{
		StanzaID:      pbf.String(m.Info.ID),
		QuotedMessage: m.Message,
		Participant:   pbf.String(m.Info.Sender.String()),
		RemoteJID:     pbf.String(m.Info.Chat.String()),
	}

	// Kirim sebagai sticker
	sticker := &waProto.StickerMessage{
		URL:           pbf.String(uploaded.URL),
		Mimetype:      pbf.String("image/webp"),
		DirectPath:    pbf.String(uploaded.DirectPath),
		FileSHA256:    uploaded.FileSHA256,
		FileEncSHA256: uploaded.FileEncSHA256,
		FileLength:    pbf.Uint64(uint64(len(webpData))),
		MediaKey:      uploaded.MediaKey,
		ContextInfo:   ci,
	}

	_, err = client.SendMessage(ctx, m.Info.Chat, &waProto.Message{StickerMessage: sticker})
	if err != nil {
		log.Printf("[BRAT] Send sticker failed: %v", err)
	} else {
		log.Printf("[BRAT] Sticker sent successfully!")
	}
}