package animekita

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const defaultBaseURL = "https://apps.animekita.org/api/v1.1.9"

// Client wraps HTTP access to the AnimeKita API.
type Client struct {
	baseURL string
	http    *http.Client
}

// New creates a fresh API client. When httpClient is nil a sane default is used.
func New(httpClient *http.Client) *Client {
	client := httpClient
	if client == nil {
		client = &http.Client{
			Timeout: 20 * time.Second,
		}
	}
	return &Client{
		baseURL: defaultBaseURL,
		http:    client,
	}
}

// SimpleEntry captures common lightweight anime metadata returned by multiple endpoints.
type SimpleEntry struct {
	ID       int    `json:"id"`
	URL      string `json:"url"`
	Title    string `json:"judul"`
	Cover    string `json:"cover"`
	LastCh   string `json:"lastch"`
	LastUp   string `json:"lastup"`
	Episode  string `json:"episode"`      // genres endpoint
	Link     string `json:"link"`         // genres endpoint
	Thumb    string `json:"thumb"`        // genres endpoint
	AirDate  string `json:"release_date"` // genres endpoint
	AnimeKey string `json:"anime_name"`   // genres endpoint
}

// ScheduleDay holds airing schedule grouped by day.
type ScheduleDay struct {
	Day  string           `json:"day"`
	List []ScheduleSeries `json:"animeList"`
}

// ScheduleSeries describes an anime entry inside schedule section.
type ScheduleSeries struct {
	ID    string `json:"id"`
	Name  string `json:"anime_name"`
	Link  string `json:"link"`
	Cover string `json:"cover"`
}

// Detail describes a single anime with chapter list.
type Detail struct {
	ID        int            `json:"id"`
	SeriesID  string         `json:"series_id"`
	Cover     string         `json:"cover"`
	Title     string         `json:"judul"`
	Type      string         `json:"type"`
	Status    string         `json:"status"`
	Rating    string         `json:"rating"`
	Published string         `json:"published"`
	Author    string         `json:"author"`
	Genres    []string       `json:"genre"`
	Synopsis  string         `json:"sinopsis"`
	Chapters  []DetailStream `json:"chapter"`
}

// DetailStream contains metadata per episode/chapter.
type DetailStream struct {
	ID    int    `json:"id"`
	Name  string `json:"ch"`
	URL   string `json:"url"`
	Date  string `json:"date"`
	Notes string `json:"history"`
}

// Episode describes a single episode stream payload.
type Episode struct {
	EpisodeID     int             `json:"episode_id"`
	LikeCount     int             `json:"likeCount"`
	DislikeCount  int             `json:"dislikeCount"`
	UserLikeState int             `json:"userLikeStatus"`
	Resolutions   []string        `json:"reso"`
	Streams       []EpisodeStream `json:"stream"`
}

// EpisodeStream holds stream links grouped by resolution/provider.
type EpisodeStream struct {
	Resolution string `json:"reso"`
	Link       string `json:"link"`
	Provider   int    `json:"provide"`
}

// SearchResult groups search results including count.
type SearchResult struct {
	Count int           `json:"jumlah"`
	Items []SearchEntry `json:"result"`
}

// SearchEntry carries a single search hit.
type SearchEntry struct {
	ID     int    `json:"id"`
	URL    string `json:"url"`
	Title  string `json:"judul"`
	Cover  string `json:"cover"`
	LastCh string `json:"lastch"`
}

// ListEntry is returned by anime-list endpoint grouped alphabetically.
type ListEntry struct {
	ID    string `json:"id"`
	Title string `json:"judul"`
	URL   string `json:"url"`
	Cover string `json:"cover"`
}

// AlphabeticalList groups anime list by first-letter key.
type AlphabeticalList map[string][]ListEntry

// NewUploads returns latest uploads by page (1-based).
func (c *Client) NewUploads(ctx context.Context, page int) ([]SimpleEntry, error) {
	if page < 1 {
		page = 1
	}
	raw, err := c.getBytes(ctx, fmt.Sprintf("/baruupload.php?page=%d", page))
	if err != nil {
		return nil, err
	}
	list, err := parseSimpleEntries(raw)
	if err != nil {
		return nil, err
	}
	return list, nil
}

// Movies returns movie list.
func (c *Client) Movies(ctx context.Context) ([]SimpleEntry, error) {
	raw, err := c.getBytes(ctx, "/movie.php")
	if err != nil {
		return nil, err
	}
	list, err := parseSimpleEntries(raw)
	if err != nil {
		return nil, err
	}
	return list, nil
}

// Schedule returns airing schedule grouped by day.
func (c *Client) Schedule(ctx context.Context) ([]ScheduleDay, error) {
	var payload struct {
		Data []ScheduleDay `json:"data"`
	}
	if err := c.get(ctx, "/jadwal.php", &payload); err != nil {
		return nil, err
	}
	return payload.Data, nil
}

// AnimeList returns complete anime list grouped alphabetically.
func (c *Client) AnimeList(ctx context.Context) (AlphabeticalList, error) {
	var payload AlphabeticalList
	if err := c.get(ctx, "/anime-list.php", &payload); err != nil {
		return nil, err
	}
	return payload, nil
}

// Genre fetches anime by genre slug and page (1-based).
func (c *Client) Genre(ctx context.Context, genre string, page int) ([]SimpleEntry, error) {
	if page < 1 {
		page = 1
	}
	slug := strings.TrimSpace(genre)
	slug = strings.ToLower(slug)
	v := url.Values{}
	v.Set("url", slug+"/")
	v.Set("page", fmt.Sprintf("%d", page))
	path := "/genreseries.php?" + v.Encode()
	raw, err := c.getBytes(ctx, path)
	if err != nil {
		return nil, err
	}
	list, err := parseSimpleEntries(raw)
	if err != nil {
		return nil, err
	}
	return list, nil
}

// Search finds anime by keyword.
func (c *Client) Search(ctx context.Context, keyword string) ([]SearchResult, error) {
	q := strings.TrimSpace(keyword)
	if q == "" {
		return nil, fmt.Errorf("keyword is required")
	}
	v := url.Values{}
	v.Set("keyword", q)
	var payload struct {
		Data []SearchResult `json:"data"`
	}
	if err := c.get(ctx, "/search.php?"+v.Encode(), &payload); err != nil {
		return nil, err
	}
	return payload.Data, nil
}

// Detail fetches detail info and chapters for given slug.
func (c *Client) Detail(ctx context.Context, slug string) (*Detail, error) {
	s := strings.TrimSpace(slug)
	if s == "" {
		return nil, fmt.Errorf("url is required")
	}
	v := url.Values{}
	v.Set("url", s)
	var payload struct {
		Data []Detail `json:"data"`
	}
	if err := c.get(ctx, "/series.php?"+v.Encode(), &payload); err != nil {
		return nil, err
	}
	if len(payload.Data) == 0 {
		return nil, fmt.Errorf("series not found")
	}
	return &payload.Data[0], nil
}

// Episode fetches stream info for given episode slug (al-... etc) and resolution (defaults to api default).
func (c *Client) Episode(ctx context.Context, slug, resolution string) (*Episode, error) {
	s := strings.TrimSpace(slug)
	if s == "" {
		return nil, fmt.Errorf("url is required")
	}
	res := strings.TrimSpace(resolution)
	if res == "" {
		res = "720p"
	}
	v := url.Values{}
	v.Set("url", s)
	v.Set("reso", res)
	var payload struct {
		Data []Episode `json:"data"`
	}
	if err := c.get(ctx, "/chapter.php?"+v.Encode(), &payload); err != nil {
		return nil, err
	}
	if len(payload.Data) == 0 {
		return nil, fmt.Errorf("episode not found")
	}
	return &payload.Data[0], nil
}

func (c *Client) get(ctx context.Context, endpoint string, out any) error {
	raw, err := c.getBytes(ctx, endpoint)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	return nil
}

func (c *Client) getBytes(ctx context.Context, endpoint string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Dart/3.1 (dart:io)")
	req.Header.Set("Accept-Encoding", "gzip")

	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if res.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 4<<10))
		return nil, fmt.Errorf("animekita %d: %s", res.StatusCode, strings.TrimSpace(string(body)))
	}

	var raw []byte
	if strings.EqualFold(res.Header.Get("Content-Encoding"), "gzip") {
		gz, err := gzip.NewReader(res.Body)
		if err != nil {
			return nil, fmt.Errorf("inflate: %w", err)
		}
		defer gz.Close()
		raw, err = io.ReadAll(gz)
		if err != nil {
			return nil, fmt.Errorf("inflate read: %w", err)
		}
	} else {
		raw, err = io.ReadAll(res.Body)
		if err != nil {
			return nil, err
		}
	}

	trimmed := bytes.TrimSpace(raw)
	start := bytes.IndexAny(trimmed, "{[")
	if start > 0 {
		trimmed = trimmed[start:]
	}
	end := lastJSONEnd(trimmed)
	if end > 0 && end < len(trimmed) {
		trimmed = trimmed[:end]
	}

	if len(trimmed) == 0 {
		return nil, fmt.Errorf("empty response")
	}
	return trimmed, nil
}

func parseSimpleEntries(raw []byte) ([]SimpleEntry, error) {
	var arr []SimpleEntry
	if err := json.Unmarshal(raw, &arr); err == nil {
		return arr, nil
	}
	var wrapper struct {
		Value []SimpleEntry `json:"value"`
	}
	if err := json.Unmarshal(raw, &wrapper); err == nil {
		return wrapper.Value, nil
	}
	return nil, fmt.Errorf("decode simple entries: unexpected format")
}

func lastJSONEnd(buf []byte) int {
	// search backwards for final '}' or ']'
	for i := len(buf) - 1; i >= 0; i-- {
		switch buf[i] {
		case '}':
			return i + 1
		case ']':
			return i + 1
		}
	}
	return len(buf)
}
