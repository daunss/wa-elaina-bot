package tkwrap

import (
	"net/http"
	"regexp"

	"go.mau.fi/whatsmeow/types"

	"wa-elaina/internal/config"
	"wa-elaina/internal/tiktok"
	"wa-elaina/internal/wa"
)

type Handler struct {
	re *regexp.Regexp
	tk *tiktok.Handler
}

func New(cfg config.Config, s *wa.Sender) *Handler {
	h := &tiktok.Handler{
		Client: http.DefaultClient,
		Send:   s,
		L: tiktok.Limits{
			Video:  cfg.TTMaxVideo,
			Image:  cfg.TTMaxImage,
			Doc:    cfg.TTMaxDoc,
			Slides: cfg.TTMaxSlides,
		},
	}

	// Perluas cakupan: vt.tiktok.com, vm.tiktok.com, m.tiktok.com, www.tiktok.com
	re := regexp.MustCompile(`(?i)https?://(?:(?:vt|vm|m|www)\.)?tiktok\.com/`)

	return &Handler{
		re: re,
		tk: h,
	}
}

func (h *Handler) TryHandle(text string, to types.JID) bool {
	if !h.re.MatchString(text) {
		return false
	}
	return h.tk.TryHandle(text, to)
}
