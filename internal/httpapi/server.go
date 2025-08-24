package httpapi

import (
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.mau.fi/whatsmeow/types"

	"wa-elaina/internal/config"
	"wa-elaina/internal/wa"
)

type Server struct {
	cfg         config.Config
	sender      *wa.Sender
	ready       *atomic.Bool
	rateCap     int
	mu          sync.Mutex
	tokenBucket map[string]*bucket
}

type bucket struct {
	tokens     float64
	lastRefill time.Time
}

func New(cfg config.Config, sender *wa.Sender, ready *atomic.Bool) *Server {
	return &Server{
		cfg:         cfg,
		sender:      sender,
		ready:       ready,
		rateCap:     cfg.SendRatePerMin,
		tokenBucket: make(map[string]*bucket),
	}
}

func (s *Server) RegisterHandlers(mux *http.ServeMux) {
	if mux == nil {
		mux = http.DefaultServeMux
	}
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/help", s.handleHelp)
	mux.HandleFunc("/send", s.handleSend)
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) handleHelp(w http.ResponseWriter, r *http.Request) {
	_, _ = io.WriteString(w, "Endpoints:\n"+
		"GET /healthz -> ok\n"+
		"GET /help -> bantuan ini\n"+
		"POST/GET /send?to=62xxxx&text=... (Header: X-API-Key)\n")
}

func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	// Auth via X-API-Key
	if s.cfg.SendAPIKey != "" && r.Header.Get("X-API-Key") != s.cfg.SendAPIKey {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// Rate limit per-IP
	ip := clientIP(r)
	if !s.allow(ip) {
		http.Error(w, "rate limit", http.StatusTooManyRequests)
		return
	}
	to := r.URL.Query().Get("to")
	text := r.URL.Query().Get("text")
	if to == "" || text == "" {
		http.Error(w, "need 'to' & 'text'", http.StatusBadRequest)
		return
	}
	if !s.ready.Load() {
		http.Error(w, "WA not ready", http.StatusServiceUnavailable)
		return
	}
	j := types.NewJID(to, types.DefaultUserServer)
	if err := s.sender.Text(wa.DestJID(j), text); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_, _ = w.Write([]byte("sent"))
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		ip := strings.TrimSpace(parts[0])
		if ip != "" {
			return "ip:" + ip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return "ip:" + r.RemoteAddr
	}
	return "ip:" + host
}

// allow: token bucket per menit (refill proporsional; clamp ke kapasitas)
func (s *Server) allow(key string) bool {
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	b, ok := s.tokenBucket[key]
	if !ok {
		s.tokenBucket[key] = &bucket{
			tokens:     float64(s.rateCap),
			lastRefill: now,
		}
		return true
	}

	elapsedMin := now.Sub(b.lastRefill).Minutes()
	if elapsedMin > 0 {
		b.tokens += elapsedMin * float64(s.rateCap)
		if b.tokens > float64(s.rateCap) {
			b.tokens = float64(s.rateCap)
		}
		b.lastRefill = now
	}

	if b.tokens < 1 {
		return false
	}
	b.tokens -= 1
	return true
}
