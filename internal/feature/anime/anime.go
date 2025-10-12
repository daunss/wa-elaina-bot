package anime

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	pbf "google.golang.org/protobuf/proto"

	"wa-elaina/internal/animekita"
	"wa-elaina/internal/wa"
)

const (
	maxSimpleItems   = 10
	maxScheduleItems = 8
	maxChapterItems  = 6
	maxStreamItems   = 6
	httpUA           = "wa-elaina-bot/1.0"
)

type Handler struct {
	api         *animekita.Client
	reTrigger   *regexp.Regexp
	genreAllow  map[string]struct{}
	genreString string
	sender      *wa.Sender
	httpc       *http.Client
}

func New(reTrigger *regexp.Regexp, sender *wa.Sender) *Handler {
	genres := []string{
		"action", "adventure", "comedy", "demons", "drama", "ecchi", "fantasy", "game",
		"harem", "historical", "horror", "josei", "magic", "martial-arts", "mecha", "military",
		"music", "mystery", "psychological", "parody", "police", "romance", "samurai", "school",
		"sci-fi", "seinen", "shoujo", "shoujo-ai", "shounen", "slice-of-life", "sports", "space",
		"super-power", "supernatural", "thriller", "vampire", "yaoi", "yuri",
	}
	set := make(map[string]struct{}, len(genres))
	for _, g := range genres {
		set[g] = struct{}{}
	}

	return &Handler{
		api:         animekita.New(nil),
		reTrigger:   reTrigger,
		genreAllow:  set,
		genreString: strings.Join(genres, ", "),
		sender:      sender,
		httpc:       &http.Client{Timeout: 90 * time.Second},
	}
}

// TryHandle inspects incoming text for anime command variations.
func (h *Handler) TryHandle(client *whatsmeow.Client, m *events.Message, text string) bool {
	args, ok := h.extractCommand(text)
	if !ok {
		return false
	}

	if len(args) == 0 {
		h.replyText(context.Background(), client, m, h.helpMessage())
		return true
	}

	sub := strings.ToLower(args[0])
	rest := args[1:]

	switch sub {
	case "help", "menu":
		h.replyText(context.Background(), client, m, h.helpMessage())
		return true

	case "new", "baru", "latest":
		page := h.parsePage(rest)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		data, err := h.api.NewUploads(ctx, page)
		if err != nil {
			h.replyText(ctx, client, m, fmt.Sprintf("Gagal ambil data rilis terbaru: %v", err))
			return true
		}
		h.replyText(ctx, client, m, formatSimpleList(fmt.Sprintf("Rilisan terbaru (page %d)", page), data))
		return true

	case "movie", "movies":
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		data, err := h.api.Movies(ctx)
		if err != nil {
			h.replyText(ctx, client, m, fmt.Sprintf("Gagal ambil daftar movie: %v", err))
			return true
		}
		h.replyText(ctx, client, m, formatSimpleList("Daftar movie", data))
		return true

	case "schedule", "jadwal":
		var day string
		if len(rest) > 0 {
			day = rest[0]
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		data, err := h.api.Schedule(ctx)
		if err != nil {
			h.replyText(ctx, client, m, fmt.Sprintf("Gagal ambil jadwal: %v", err))
			return true
		}
		h.replyText(ctx, client, m, formatSchedule(day, data))
		return true

	case "list":
		if len(rest) == 0 {
			h.replyText(context.Background(), client, m, "Format: anime list <huruf>. Contoh: anime list a")
			return true
		}
		letter := strings.ToUpper(rest[0])
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		data, err := h.api.AnimeList(ctx)
		if err != nil {
			h.replyText(ctx, client, m, fmt.Sprintf("Gagal ambil daftar lengkap: %v", err))
			return true
		}
		h.replyText(ctx, client, m, formatAlphabetList(letter, data))
		return true

	case "genre":
		if len(rest) == 0 {
			h.replyText(context.Background(), client, m, "Format: anime genre <nama> [page].\nGenre tersedia: "+h.genreString)
			return true
		}
		genre := strings.ToLower(rest[0])
		if _, ok := h.genreAllow[genre]; !ok {
			h.replyText(context.Background(), client, m, "Genre tidak dikenal. Pilih salah satu dari:\n"+h.genreString)
			return true
		}
		page := 1
		if len(rest) > 1 {
			page = h.parsePage(rest[1:])
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		data, err := h.api.Genre(ctx, genre, page)
		if err != nil {
			h.replyText(ctx, client, m, fmt.Sprintf("Gagal ambil daftar genre %s: %v", genre, err))
			return true
		}
		title := fmt.Sprintf("Genre %s (page %d)", genre, page)
		h.replyText(ctx, client, m, formatSimpleList(title, data))
		return true

	case "search", "cari", "find":
		if len(rest) == 0 {
			h.replyText(context.Background(), client, m, "Format: anime search <kata kunci>")
			return true
		}
		query := strings.Join(rest, " ")
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		results, err := h.api.Search(ctx, query)
		if err != nil {
			h.replyText(ctx, client, m, fmt.Sprintf("Gagal mencari \"%s\": %v", query, err))
			return true
		}
		h.replyText(ctx, client, m, formatSearchResults(query, results))
		return true

	case "detail":
		if len(rest) == 0 {
			h.replyText(context.Background(), client, m, "Format: anime detail <slug>")
			return true
		}
		slug := rest[0]
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		detail, err := h.api.Detail(ctx, slug)
		if err != nil {
			h.replyText(ctx, client, m, fmt.Sprintf("Gagal ambil detail untuk %s: %v", slug, err))
			return true
		}
		h.replyText(ctx, client, m, formatDetail(detail))
		return true

	case "episode", "stream":
		if len(rest) == 0 {
			h.replyText(context.Background(), client, m, "Format: anime episode <slug> [reso]. Gunakan slug dari daftar chapter detail.")
			return true
		}
		slug := rest[0]
		reso := "720p"
		if len(rest) > 1 {
			reso = rest[1]
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		episode, err := h.api.Episode(ctx, slug, reso)
		if err != nil {
			h.replyText(ctx, client, m, fmt.Sprintf("Gagal ambil stream untuk %s (%s): %v", slug, reso, err))
			return true
		}
		note := ""
		if h.sender != nil && h.hasPixeldrain(episode.Streams) {
			note = fmt.Sprintf("\n%s akan mencoba unduh tautan Pixeldrain otomatis.", botName())
		}
		h.replyText(ctx, client, m, formatEpisode(slug, episode, note))

		if h.sender != nil && h.httpc != nil {
			go h.deliverPixeldrain(m.Info.Chat, episode.Streams)
		}
		return true
	}

	h.replyText(context.Background(), client, m, h.helpMessage())
	return true
}

func (h *Handler) extractCommand(text string) ([]string, bool) {
	t := strings.TrimSpace(text)
	if t == "" {
		return nil, false
	}

	if strings.HasPrefix(t, "!") {
		parts := strings.Fields(strings.TrimPrefix(t, "!"))
		if len(parts) == 0 {
			return nil, false
		}
		if strings.EqualFold(parts[0], "anime") {
			return parts[1:], true
		}
		return nil, false
	}

	if !h.reTrigger.MatchString(t) {
		return nil, false
	}
	clean := strings.TrimSpace(h.reTrigger.ReplaceAllString(t, ""))
	if clean == "" {
		return nil, false
	}
	parts := strings.Fields(clean)
	if len(parts) == 0 {
		return nil, false
	}
	if !strings.EqualFold(parts[0], "anime") {
		return nil, false
	}
	return parts[1:], true
}

func (h *Handler) parsePage(args []string) int {
	if len(args) == 0 {
		return 1
	}
	p, err := strconv.Atoi(args[0])
	if err != nil || p < 1 {
		return 1
	}
	return p
}

func (h *Handler) replyText(ctx context.Context, client *whatsmeow.Client, m *events.Message, msg string) {
	ci := &waProto.ContextInfo{
		StanzaID:      pbf.String(m.Info.ID),
		QuotedMessage: m.Message,
		Participant:   pbf.String(m.Info.Sender.String()),
		RemoteJID:     pbf.String(m.Info.Chat.String()),
	}
	_, _ = client.SendMessage(ctx, m.Info.Chat, &waProto.Message{
		ExtendedTextMessage: &waProto.ExtendedTextMessage{
			Text:        pbf.String(msg),
			ContextInfo: ci,
		},
	})
}

func (h *Handler) helpMessage() string {
	return strings.Join([]string{
		"*Menu AnimeKita*",
		"anime new [page]       - rilisan baru",
		"anime movie            - daftar movie",
		"anime schedule [hari]  - jadwal rilis (contoh: anime schedule selasa)",
		"anime list <huruf>     - daftar anime per awal huruf",
		"anime genre <nama> [page] - daftar berdasarkan genre",
		"anime search <keyword> - cari anime",
		"anime detail <slug>    - info detail + daftar chapter",
		"anime episode <slug> [reso] - link streaming per episode",
		"",
		"Gunakan slug/link dari hasil detail untuk mengambil episode.",
	}, "\n")
}

func formatSimpleList(title string, entries []animekita.SimpleEntry) string {
	if len(entries) == 0 {
		return fmt.Sprintf("%s kosong.", title)
	}
	max := len(entries)
	if max > maxSimpleItems {
		max = maxSimpleItems
	}
	var sb strings.Builder
	sb.WriteString("*")
	sb.WriteString(title)
	sb.WriteString("*\n")
	for i := 0; i < max; i++ {
		e := entries[i]
		name := fallback(e.Title, e.AnimeKey, "Tanpa judul")
		slug := fallback(e.URL, e.Link)
		update := fallback(e.LastUp, e.AirDate, e.Episode)
		fmt.Fprintf(&sb, "%d. %s\n", i+1, name)
		if slug != "" {
			sb.WriteString("   slug: ")
			sb.WriteString(slug)
			sb.WriteString("\n")
		}
		if update != "" {
			sb.WriteString("   info: ")
			sb.WriteString(update)
			sb.WriteString("\n")
		}
		if e.Cover != "" && i < 4 {
			sb.WriteString("   cover: ")
			sb.WriteString(e.Cover)
			sb.WriteString("\n")
		}
	}
	if len(entries) > max {
		fmt.Fprintf(&sb, "... %d entri lainnya.\n", len(entries)-max)
	}
	sb.WriteString("\nDetail: anime detail <slug>")
	return sb.String()
}

func formatSchedule(day string, days []animekita.ScheduleDay) string {
	if len(days) == 0 {
		return "Jadwal kosong."
	}
	wantDay := strings.TrimSpace(strings.ToLower(day))
	var sb strings.Builder
	sb.WriteString("*Jadwal Rilis*\n")
	var found bool
	for _, d := range days {
		if wantDay != "" && !strings.EqualFold(wantDay, d.Day) {
			continue
		}
		found = true
		sb.WriteString(d.Day)
		sb.WriteString(":\n")
		limit := len(d.List)
		if limit > maxScheduleItems {
			limit = maxScheduleItems
		}
		for i := 0; i < limit; i++ {
			item := d.List[i]
			name := fallback(item.Name, "Tanpa judul")
			fmt.Fprintf(&sb, "- %s (slug: %s)\n", name, item.Link)
		}
		if len(d.List) > limit {
			fmt.Fprintf(&sb, "  ... %d lainnya\n", len(d.List)-limit)
		}
		sb.WriteString("\n")
	}
	if !found {
		return fmt.Sprintf("Tidak ada jadwal untuk \"%s\".", day)
	}
	sb.WriteString("Ambil info lengkap: anime detail <slug>")
	return strings.TrimSpace(sb.String())
}

func formatAlphabetList(letter string, data animekita.AlphabeticalList) string {
	entries := data[letter]
	if len(entries) == 0 {
		if letter == "#" {
			return "Tidak ada daftar untuk simbol #."
		}
		return fmt.Sprintf("Tidak ada anime yang diawali huruf %s.", letter)
	}
	max := len(entries)
	if max > maxSimpleItems {
		max = maxSimpleItems
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "*Daftar huruf %s*\n", letter)
	for i := 0; i < max; i++ {
		e := entries[i]
		name := fallback(e.Title, "Tanpa judul")
		fmt.Fprintf(&sb, "%d. %s\n", i+1, name)
		sb.WriteString("   slug: ")
		sb.WriteString(e.URL)
		sb.WriteString("\n")
	}
	if len(entries) > max {
		fmt.Fprintf(&sb, "... %d entri lainnya.\n", len(entries)-max)
	}
	sb.WriteString("Gunakan anime detail <slug> untuk info.")
	return sb.String()
}

func formatSearchResults(query string, results []animekita.SearchResult) string {
	var all []animekita.SearchEntry
	for _, block := range results {
		all = append(all, block.Items...)
	}
	if len(all) == 0 {
		return fmt.Sprintf("Tidak ada hasil untuk \"%s\".", query)
	}
	max := len(all)
	if max > maxSimpleItems {
		max = maxSimpleItems
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "*Hasil pencarian \"%s\"*\n", query)
	for i := 0; i < max; i++ {
		e := all[i]
		name := fallback(e.Title, "Tanpa judul")
		fmt.Fprintf(&sb, "%d. %s\n", i+1, name)
		if e.URL != "" {
			sb.WriteString("   slug: ")
			sb.WriteString(e.URL)
			sb.WriteString("\n")
		}
	}
	if len(all) > max {
		fmt.Fprintf(&sb, "... %d hasil lainnya.\n", len(all)-max)
	}
	sb.WriteString("Detail: anime detail <slug>")
	return sb.String()
}

func formatDetail(d *animekita.Detail) string {
	if d == nil {
		return "Data detail kosong."
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "*%s*\n", fallback(d.Title, "Tanpa judul"))
	if d.Type != "" || d.Status != "" {
		fmt.Fprintf(&sb, "%s | %s\n", fallback(d.Type, "?"), fallback(d.Status, "?"))
	}
	if d.Rating != "" {
		fmt.Fprintf(&sb, "Rating: %s\n", d.Rating)
	}
	if d.Published != "" {
		fmt.Fprintf(&sb, "Rilis: %s\n", d.Published)
	}
	if d.Author != "" {
		fmt.Fprintf(&sb, "Studio/Author: %s\n", d.Author)
	}
	if len(d.Genres) > 0 {
		sb.WriteString("Genre: ")
		sb.WriteString(strings.Join(d.Genres, ", "))
		sb.WriteString("\n")
	}
	if d.Cover != "" {
		sb.WriteString("Cover: ")
		sb.WriteString(d.Cover)
		sb.WriteString("\n")
	}
	if strings.TrimSpace(d.Synopsis) != "" {
		sb.WriteString("\nSinopsis:\n")
		sb.WriteString(strings.TrimSpace(d.Synopsis))
		sb.WriteString("\n")
	}
	sb.WriteString("\nChapter terbaru:\n")
	if len(d.Chapters) == 0 {
		sb.WriteString("- Belum ada data.\n")
	} else {
		limit := len(d.Chapters)
		if limit > maxChapterItems {
			limit = maxChapterItems
		}
		for i := 0; i < limit; i++ {
			ch := d.Chapters[i]
			fmt.Fprintf(&sb, "- %s (%s) -> %s\n", fallback(ch.Name, "ep"), fallback(ch.Date, "?"), ch.URL)
		}
		if len(d.Chapters) > limit {
			fmt.Fprintf(&sb, "... %d episode lainnya di API.\n", len(d.Chapters)-limit)
		}
	}
	sb.WriteString("\nAmbil stream: anime episode <slug> [reso]")
	return sb.String()
}

func formatEpisode(slug string, ep *animekita.Episode, note string) string {
	if ep == nil {
		return fmt.Sprintf("Tidak ada data untuk %s.", slug)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "*Episode %s*\n", slug)
	if len(ep.Resolutions) > 0 {
		sb.WriteString("Resolusi tersedia: ")
		sb.WriteString(strings.Join(ep.Resolutions, ", "))
		sb.WriteString("\n")
	}
	sb.WriteString("Link streaming:\n")
	if len(ep.Streams) == 0 {
		sb.WriteString("- Belum ada tautan.\n")
	} else {
		limit := len(ep.Streams)
		if limit > maxStreamItems {
			limit = maxStreamItems
		}
		for i := 0; i < limit; i++ {
			s := ep.Streams[i]
			fmt.Fprintf(&sb, "- %s -> %s\n", fallback(s.Resolution, "?"), s.Link)
		}
		if len(ep.Streams) > limit {
			fmt.Fprintf(&sb, "... %d link lainnya di API.\n", len(ep.Streams)-limit)
		}
	}
	if strings.TrimSpace(note) != "" {
		sb.WriteString(note)
		sb.WriteString("\n")
	}
	return sb.String()
}

func fallback(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func isPixeldrainLink(link string) bool {
	return strings.Contains(strings.ToLower(link), "pixeldrain.com/api/file/")
}

func (h *Handler) hasPixeldrain(streams []animekita.EpisodeStream) bool {
	for _, st := range streams {
		if isPixeldrainLink(st.Link) {
			return true
		}
	}
	return false
}

func (h *Handler) deliverPixeldrain(chat types.JID, streams []animekita.EpisodeStream) {
	if h.sender == nil || h.httpc == nil {
		return
	}
	dest := wa.DestJID(chat)
	for _, st := range streams {
		if !isPixeldrainLink(st.Link) {
			continue
		}
		if err := h.fetchAndSendPixeldrain(dest, st); err != nil {
			_ = h.sender.Text(dest, fmt.Sprintf("Gagal unduh Pixeldrain %s: %v", fallback(st.Resolution, st.Link), err))
		}
	}
}

func (h *Handler) fetchAndSendPixeldrain(chat types.JID, st animekita.EpisodeStream) error {
	link := strings.TrimSpace(st.Link)
	if link == "" {
		return fmt.Errorf("tautan kosong")
	}
	data, mimeType, filename, err := h.downloadPixeldrain(link)
	if err != nil {
		return err
	}
	captionBase := fallback(st.Resolution, filename)
	caption := strings.TrimSpace(fmt.Sprintf("Pixeldrain %s", captionBase))
	return h.sender.Document(chat, data, mimeType, filename, caption)
}

func (h *Handler) downloadPixeldrain(link string) ([]byte, string, string, error) {
	req, err := http.NewRequest(http.MethodGet, link, nil)
	if err != nil {
		return nil, "", "", err
	}
	req.Header.Set("User-Agent", httpUA)

	resp, err := h.httpc.Do(req)
	if err != nil {
		return nil, "", "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return nil, "", "", fmt.Errorf("http %s", resp.Status)
	}

	filename := filenameFromHeaders(link, resp.Header.Get("Content-Disposition"))
	mimeType := strings.TrimSpace(resp.Header.Get("Content-Type"))

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", "", err
	}

	if mimeType == "" {
		mimeType = http.DetectContentType(data)
	}
	return data, mimeType, filename, nil
}

func filenameFromHeaders(link, header string) string {
	fallbackName := fallback(extractPathName(link), "pixeldrain.bin")
	if header == "" {
		return fallbackName
	}
	if _, params, err := mime.ParseMediaType(header); err == nil {
		if raw := params["filename*"]; raw != "" {
			if decoded, err := decodeRFC5987(raw); err == nil && decoded != "" {
				return decoded
			}
		}
		if name := params["filename"]; strings.TrimSpace(name) != "" {
			return name
		}
	}
	return fallbackName
}

func decodeRFC5987(raw string) (string, error) {
	parts := strings.SplitN(raw, "''", 2)
	if len(parts) != 2 {
		return "", fmt.Errorf("invalid extended filename")
	}
	enc := strings.ToLower(parts[0])
	data := parts[1]
	if enc != "utf-8" && enc != "us-ascii" {
		return "", fmt.Errorf("unsupported encoding %s", enc)
	}
	decoded, err := url.QueryUnescape(data)
	if err != nil {
		return "", err
	}
	return decoded, nil
}

func extractPathName(link string) string {
	u, err := url.Parse(link)
	if err != nil {
		return ""
	}
	name := path.Base(u.Path)
	name = strings.TrimSpace(name)
	name = strings.TrimSuffix(name, "?download")
	return name
}

func botName() string {
	if v := strings.TrimSpace(os.Getenv("BOT_NAME")); v != "" {
		return v
	}
	return "Bot"
}
