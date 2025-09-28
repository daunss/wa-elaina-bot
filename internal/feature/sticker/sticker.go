package sticker

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	pbf "google.golang.org/protobuf/proto"

	"wa-elaina/internal/util"
)

type Handler struct {
	reCmd *regexp.Regexp
	reNat *regexp.Regexp
	reEla *regexp.Regexp
}

func New() *Handler {
	return &Handler{
		reCmd: regexp.MustCompile(`(?i)\b(!s|!sgif|!stiker|!sticker|stiker|sticker)\b`),
		reNat: regexp.MustCompile(`(?i)\b(stiker|sticker)\b`),
		reEla: regexp.MustCompile(`(?i)\belaina\b`),
	}
}

func (h *Handler) TryHandleTo(client *whatsmeow.Client, to types.JID, msg *waProto.Message, rawText string) bool {
	text := getText(rawText, msg)
	low := strings.ToLower(strings.TrimSpace(text))
	hasMedia := hasImageOrVideo(msg)

	if h.reCmd.MatchString(low) {
		return h.handleStickerTo(client, to, msg, low)
	}
	
	if h.reNat.MatchString(low) && hasMedia {
		return h.handleStickerTo(client, to, msg, low)
	}
	
	if hasMedia && h.reEla.MatchString(low) && (strings.Contains(low, "stiker") || strings.Contains(low, "sticker")) {
		return h.handleStickerTo(client, to, msg, low)
	}
	
	return false
}

func (h *Handler) TryHandle(client *whatsmeow.Client, msg *waProto.Message, rawText string) bool {
	text := getText(rawText, msg)
	low := strings.ToLower(strings.TrimSpace(text))
	hasMedia := hasImageOrVideo(msg)

	if h.reCmd.MatchString(low) {
		return h.handleStickerTo(client, types.JID{}, msg, low)
	}
	
	if h.reNat.MatchString(low) && hasMedia {
		return h.handleStickerTo(client, types.JID{}, msg, low)
	}
	
	if hasMedia && h.reEla.MatchString(low) && (strings.Contains(low, "stiker") || strings.Contains(low, "sticker")) {
		return h.handleStickerTo(client, types.JID{}, msg, low)
	}
	
	return false
}

func (h *Handler) handleStickerTo(client *whatsmeow.Client, to types.JID, msg *waProto.Message, low string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()

	isAnimated := strings.Contains(low, "!sgif")
	url := firstURL(low)

	var data []byte
	var err error

	if url != "" {
		data, _, err = util.DownloadBytes(nil, url, 50<<20)
		if err != nil {
			_ = sendText(ctx, client, to, msg, "Gagal unduh: "+err.Error())
			return true
		}
		if !isAnimated && strings.HasSuffix(strings.ToLower(url), ".gif") {
			isAnimated = true
		}
	} else {
		switch {
		case msg.GetImageMessage() != nil:
			data, err = client.Download(ctx, msg.GetImageMessage())
		case msg.GetVideoMessage() != nil:
			data, err = client.Download(ctx, msg.GetVideoMessage())
			if !isAnimated {
				isAnimated = true
			}
		default:
			if et := msg.GetExtendedTextMessage(); et != nil && et.GetContextInfo() != nil {
				if qm := et.GetContextInfo().GetQuotedMessage(); qm != nil {
					switch {
					case qm.GetImageMessage() != nil:
						data, err = client.Download(ctx, qm.GetImageMessage())
					case qm.GetVideoMessage() != nil:
						data, err = client.Download(ctx, qm.GetVideoMessage())
						if !isAnimated {
							isAnimated = true
						}
					}
				}
			}
		}
		if err != nil || len(data) == 0 {
			_ = sendText(ctx, client, to, msg, "Tidak menemukan media untuk dijadikan sticker. Sertakan URL atau reply gambar/video.")
			return true
		}
	}

	outWebP, err := toWebP(ctx, data, isAnimated)
	if err != nil {
		_ = sendText(ctx, client, to, msg, "Konversi WebP gagal: "+err.Error())
		return true
	}

	if err := sendStickerBytes(ctx, client, to, msg, outWebP, isAnimated); err != nil {
		_ = sendText(ctx, client, to, msg, "Kirim sticker gagal: "+err.Error())
	}
	return true
}

func hasImageOrVideo(m *waProto.Message) bool {
	if m.GetImageMessage() != nil || m.GetVideoMessage() != nil {
		return true
	}
	if et := m.GetExtendedTextMessage(); et != nil && et.GetContextInfo() != nil {
		if qm := et.GetContextInfo().GetQuotedMessage(); qm != nil {
			return qm.GetImageMessage() != nil || qm.GetVideoMessage() != nil
		}
	}
	return false
}

func firstURL(s string) string {
	rx := regexp.MustCompile(`https?://\S+`)
	return rx.FindString(s)
}

func getText(fallback string, m *waProto.Message) string {
	if fallback != "" {
		return fallback
	}
	if m.GetConversation() != "" {
		return m.GetConversation()
	}
	if et := m.GetExtendedTextMessage(); et != nil && et.GetText() != "" {
		return et.GetText()
	}
	if im := m.GetImageMessage(); im != nil && im.GetCaption() != "" {
		return im.GetCaption()
	}
	if vm := m.GetVideoMessage(); vm != nil && vm.GetCaption() != "" {
		return vm.GetCaption()
	}
	return ""
}

func sendText(ctx context.Context, client *whatsmeow.Client, to types.JID, in *waProto.Message, text string) error {
	var jid types.JID
	if to.User != "" {
		jid = to
	} else {
		ci := &waProto.ContextInfo{}
		if xt := in.GetExtendedTextMessage(); xt != nil && xt.GetContextInfo() != nil {
			ci = xt.GetContextInfo()
		}
		jidStr := ci.GetRemoteJID()
		if jidStr == "" {
			return fmt.Errorf("remote JID kosong: tidak bisa mengirim balasan")
		}
		var err error
		jid, err = types.ParseJID(jidStr)
		if err != nil {
			return fmt.Errorf("parse JID gagal: %w", err)
		}
	}
	_, err := client.SendMessage(ctx, jid, &waProto.Message{
		ExtendedTextMessage: &waProto.ExtendedTextMessage{
			Text: pbf.String(text),
		},
	})
	return err
}

func getenv(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func atoiDef(s string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func atofDef(s string, def float64) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || f < 0 || f > 1 {
		return def
	}
	return f
}

func fileExists(p string) bool {
	if p == "" {
		return false
	}
	_, err := os.Stat(p)
	return err == nil
}

func getAbsolutePath(path string) string {
	if path == "" {
		return ""
	}
	
	if filepath.IsAbs(path) {
		return path
	}
	
	absPath, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	
	return absPath
}

func findSystemFont() string {
	var possibleFonts []string
	
	switch runtime.GOOS {
	case "windows":
		possibleFonts = []string{
			"C:\\Windows\\Fonts\\arial.ttf",
			"C:\\Windows\\Fonts\\calibri.ttf", 
			"C:\\Windows\\Fonts\\tahoma.ttf",
			"C:\\Windows\\Fonts\\segoeui.ttf",
		}
	case "darwin":
		possibleFonts = []string{
			"/System/Library/Fonts/Arial.ttf",
			"/System/Library/Fonts/Helvetica.ttc",
			"/Library/Fonts/Arial.ttf",
		}
	default:
		possibleFonts = []string{
			"/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf",
			"/usr/share/fonts/truetype/liberation/LiberationSans-Regular.ttf",
			"/usr/share/fonts/TTF/arial.ttf",
			"/usr/share/fonts/truetype/noto/NotoSans-Regular.ttf",
			"/System/Library/Fonts/Arial.ttf",
		}
	}
	
	for _, font := range possibleFonts {
		if fileExists(font) {
			return font
		}
	}
	
	return ""
}

func defaultFontFile() string {
	if ff := strings.TrimSpace(os.Getenv("STICKER_WM_FONT")); ff != "" {
		absPath := getAbsolutePath(ff)
		if fileExists(absPath) {
			return absPath
		}
	}
	
	systemFont := findSystemFont()
	if systemFont != "" {
		return systemFont
	}
	
	if runtime.GOOS == "windows" {
		return "arial"
	}
	
	return "DejaVu Sans"
}

func toWebP(ctx context.Context, input []byte, animated bool) ([]byte, error) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return nil, fmt.Errorf("ffmpeg tidak ditemukan di PATH")
	}
	
	tmpDir, _ := os.MkdirTemp("", "sticker-*")
	defer os.RemoveAll(tmpDir)

	in := filepath.Join(tmpDir, "in.bin")
	out := filepath.Join(tmpDir, "out.webp")
	if err := os.WriteFile(in, input, 0o600); err != nil {
		return nil, err
	}

	wmText := getenv("STICKER_WM_TEXT", "@Elaina")
	wmImg := getenv("STICKER_WM_IMG", "")
	wmSize := atoiDef(getenv("STICKER_WM_SIZE", "18"), 18)
	wmMargin := atoiDef(getenv("STICKER_WM_MARGIN", "12"), 12)
	wmAlpha := atofDef(getenv("STICKER_WM_ALPHA", "0.8"), 0.8)
	wmImgScale := atoiDef(getenv("STICKER_WM_IMG_SCALE", "128"), 128)

	if wmImg != "" {
		wmImg = getAbsolutePath(wmImg)
	}
	
	useImg := fileExists(wmImg)

	base := ""
	if animated {
		base = "fps=15,scale=512:-1:force_original_aspect_ratio=decrease:flags=lanczos," +
			"pad=512:512:(ow-iw)/2:(oh-ih)/2:color=0x00000000"
	} else {
		base = "scale=512:512:force_original_aspect_ratio=decrease:flags=lanczos," +
			"pad=512:512:(ow-iw)/2:(oh-ih)/2:color=0x00000000"
	}

	var args []string
	var codec []string

	if animated {
		codec = []string{"-vcodec", "libwebp", "-loop", "0", "-lossless", "0", "-q:v", "60", "-preset", "default"}
	} else {
		codec = []string{"-vcodec", "libwebp", "-lossless", "1", "-q:v", "75", "-preset", "default"}
	}

	if useImg {
		wmImgPath := strings.ReplaceAll(wmImg, "\\", "/")
		
		args = []string{
			"-y",
			"-i", in,
			"-i", wmImgPath,
			"-filter_complex",
			fmt.Sprintf("[0:v]%s[bg];[1:v]scale=%d:-1,format=rgba,colorchannelmixer=aa=%0.2f[wm];[bg][wm]overlay=main_w-overlay_w-%d:main_h-overlay_h-%d",
				base, wmImgScale, wmAlpha, wmMargin, wmMargin),
		}
		args = append(args, codec...)
		args = append(args, out)
	} else {
		font := defaultFontFile()
		
		escaped := strings.ReplaceAll(wmText, ":", "\\:")
		escaped = strings.ReplaceAll(escaped, "'", "\\'")
		escaped = strings.ReplaceAll(escaped, "\\", "\\\\")
		
		var vf string
		if fileExists(font) {
			fontPath := strings.ReplaceAll(font, "\\", "/")
			fontPath = strings.ReplaceAll(fontPath, ":", "\\:")
			vf = fmt.Sprintf(`%s,drawtext=fontfile='%s':text='%s':fontcolor=white@%0.2f:fontsize=%d:box=1:boxcolor=0x00000088:boxborderw=6:x=w-tw-%d:y=h-th-%d`,
				base, fontPath, escaped, wmAlpha, wmSize, wmMargin, wmMargin)
		} else {
			vf = fmt.Sprintf(`%s,drawtext=font='%s':text='%s':fontcolor=white@%0.2f:fontsize=%d:box=1:boxcolor=0x00000088:boxborderw=6:x=w-tw-%d:y=h-th-%d`,
				base, font, escaped, wmAlpha, wmSize, wmMargin, wmMargin)
		}

		args = []string{"-y", "-i", in, "-vf", vf}
		args = append(args, codec...)
		args = append(args, out)
	}

	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	cmd.Stderr = &stderr
	
	cmd.Env = append(os.Environ(), 
		"FONTCONFIG_FILE=/dev/null",
		"FC_CONFIG_FILE=/dev/null",
	)
	
	if err := cmd.Run(); err != nil {
		tail := stderr.String()
		if len(tail) > 400 {
			tail = tail[len(tail)-400:]
		}
		return nil, fmt.Errorf("ffmpeg error: %s", tail)
	}

	return os.ReadFile(out)
}

func sendStickerBytes(ctx context.Context, client *whatsmeow.Client, to types.JID, in *waProto.Message, webp []byte, animated bool) error {
	_ = animated

	uploaded, err := client.Upload(ctx, webp, whatsmeow.MediaImage)
	if err != nil {
		return err
	}

	var jid types.JID
	if to.User != "" {
		jid = to
	} else {
		ci := &waProto.ContextInfo{}
		if xt := in.GetExtendedTextMessage(); xt != nil && xt.GetContextInfo() != nil {
			ci = xt.GetContextInfo()
		}
		jidStr := ci.GetRemoteJID()
		if jidStr == "" {
			return fmt.Errorf("remote JID kosong: tidak bisa kirim sticker")
		}
		jid, err = types.ParseJID(jidStr)
		if err != nil {
			return fmt.Errorf("parse JID gagal: %w", err)
		}
	}

	var ci *waProto.ContextInfo
	if xt := in.GetExtendedTextMessage(); xt != nil && xt.GetContextInfo() != nil {
		ci = xt.GetContextInfo()
	}

	sticker := &waProto.StickerMessage{
		URL:           pbf.String(uploaded.URL),
		Mimetype:      pbf.String("image/webp"),
		DirectPath:    pbf.String(uploaded.DirectPath),
		FileSHA256:    uploaded.FileSHA256,
		FileEncSHA256: uploaded.FileEncSHA256,
		FileLength:    pbf.Uint64(uint64(len(webp))),
		MediaKey:      uploaded.MediaKey,
		ContextInfo:   ci,
	}

	_, err = client.SendMessage(ctx, jid, &waProto.Message{StickerMessage: sticker})
	return err
}