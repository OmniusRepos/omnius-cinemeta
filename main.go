// omnius-cinemeta is a small self-hosted metadata service for the Omnius server.
//
// It replaces the dead api.imdbapi.dev by serving that exact contract
// (/titles/{id}, /search/titles) so the Omnius app only needs its base URL
// changed. Data comes from Stremio's Cinemeta (imdb-id native, free, no key,
// posters included); every upstream response is cached in memory so a Cinemeta
// hiccup doesn't take the app down, and the upstream is swappable via env.
//
// Stdlib only — builds to a tiny static binary.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

var (
	cinemetaBase = strings.TrimRight(env("CINEMETA_BASE", "https://v3-cinemeta.strem.io"), "/")
	httpClient   = &http.Client{Timeout: 15 * time.Second}
)

// ---- the imdbapi.dev contract the Omnius app expects ----------------------

type rating struct {
	AggregateRating float64 `json:"aggregateRating"`
	VoteCount       int     `json:"voteCount"`
}
type image struct {
	URL    string `json:"url"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}
type imdbTitle struct {
	ID             string   `json:"id"`
	Type           string   `json:"type"`
	PrimaryTitle   string   `json:"primaryTitle"`
	OriginalTitle  string   `json:"originalTitle"`
	StartYear      int      `json:"startYear"`
	EndYear        *int     `json:"endYear,omitempty"`
	RuntimeSeconds int      `json:"runtimeSeconds"`
	Genres         []string `json:"genres"`
	Plot           string   `json:"plot"`
	Rating         *rating  `json:"rating,omitempty"`
	PrimaryImage   *image   `json:"primaryImage,omitempty"`
	ContentRating  string   `json:"contentRating"`
	IsAdult        bool     `json:"isAdult"`
}
type searchResponse struct {
	Titles []imdbTitle `json:"titles"`
}

// ---- Cinemeta shapes (only the fields we use) -----------------------------

type cinemetaMeta struct {
	ID          string          `json:"id"`
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Year        string          `json:"year"`
	ReleaseInfo string          `json:"releaseInfo"`
	Runtime     string          `json:"runtime"`
	Description string          `json:"description"`
	ImdbRating  string          `json:"imdbRating"`
	Genres      []string        `json:"genres"`
	Genre       []string        `json:"genre"`
	Poster      string          `json:"poster"`
	Videos      []cinemetaVideo `json:"videos"` // series episodes
}

type cinemetaVideo struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Name      string `json:"name"`
	Season    int    `json:"season"`
	Episode   int    `json:"episode"`
	Number    int    `json:"number"`
	Released  string `json:"released"`
	Thumbnail string `json:"thumbnail"`
	Overview  string `json:"overview"`
}

// The /titles/{id}/episodes contract the Omnius app expects.
type imdbEpisode struct {
	ID            string `json:"id"`
	Title         string `json:"title"`
	Season        string `json:"season"`
	EpisodeNumber int    `json:"episodeNumber"`
	Plot          string `json:"plot"`
	PrimaryImage  *image `json:"primaryImage,omitempty"`
}
type episodesResponse struct {
	Episodes      []imdbEpisode `json:"episodes"`
	TotalCount    int           `json:"totalCount"`
	NextPageToken string        `json:"nextPageToken"`
}

// ---- small TTL cache ------------------------------------------------------

type cacheEntry struct {
	val any
	exp time.Time
}
type cache struct {
	mu sync.RWMutex
	m  map[string]cacheEntry
}

func newCache() *cache { return &cache{m: make(map[string]cacheEntry)} }
func (c *cache) get(k string) (any, bool) {
	c.mu.RLock()
	e, ok := c.m[k]
	c.mu.RUnlock()
	if !ok || time.Now().After(e.exp) {
		return nil, false
	}
	return e.val, true
}
func (c *cache) set(k string, v any, ttl time.Duration) {
	c.mu.Lock()
	c.m[k] = cacheEntry{val: v, exp: time.Now().Add(ttl)}
	c.mu.Unlock()
}

var (
	titleCache  = newCache()
	searchCache = newCache()
	epCache     = newCache()
)

// ---- upstream helpers -----------------------------------------------------

func fetchMeta(kind, id string) (*cinemetaMeta, error) {
	u := fmt.Sprintf("%s/meta/%s/%s.json", cinemetaBase, kind, url.PathEscape(id))
	resp, err := httpClient.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cinemeta meta %s/%s -> %d", kind, id, resp.StatusCode)
	}
	var wrap struct {
		Meta cinemetaMeta `json:"meta"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&wrap); err != nil {
		return nil, err
	}
	if wrap.Meta.ID == "" {
		return nil, fmt.Errorf("cinemeta meta %s/%s: empty", kind, id)
	}
	return &wrap.Meta, nil
}

func fetchCatalog(kind, query string) ([]cinemetaMeta, error) {
	u := fmt.Sprintf("%s/catalog/%s/top/search=%s.json", cinemetaBase, kind, url.PathEscape(query))
	resp, err := httpClient.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cinemeta catalog %s -> %d", kind, resp.StatusCode)
	}
	var wrap struct {
		Metas []cinemetaMeta `json:"metas"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&wrap); err != nil {
		return nil, err
	}
	return wrap.Metas, nil
}

// ---- reshaping ------------------------------------------------------------

func imdbType(t string) string {
	if t == "series" {
		return "tvSeries"
	}
	return "movie"
}

func parseYear(m *cinemetaMeta) int {
	s := m.Year
	if s == "" {
		s = m.ReleaseInfo
	}
	if len(s) >= 4 {
		if y, err := strconv.Atoi(s[:4]); err == nil {
			return y
		}
	}
	return 0
}

// "136 min" -> 8160 seconds
func parseRuntimeSeconds(s string) int {
	f := strings.Fields(s)
	if len(f) == 0 {
		return 0
	}
	if n, err := strconv.Atoi(f[0]); err == nil {
		return n * 60
	}
	return 0
}

func toTitle(m *cinemetaMeta) imdbTitle {
	g := m.Genres
	if len(g) == 0 {
		g = m.Genre
	}
	t := imdbTitle{
		ID:             m.ID,
		Type:           imdbType(m.Type),
		PrimaryTitle:   m.Name,
		OriginalTitle:  m.Name,
		StartYear:      parseYear(m),
		RuntimeSeconds: parseRuntimeSeconds(m.Runtime),
		Genres:         g,
		Plot:           m.Description,
	}
	if r, err := strconv.ParseFloat(strings.TrimSpace(m.ImdbRating), 64); err == nil && r > 0 {
		t.Rating = &rating{AggregateRating: r}
	}
	poster := m.Poster
	if poster == "" && strings.HasPrefix(m.ID, "tt") {
		poster = "https://images.metahub.space/poster/medium/" + m.ID + "/img"
	}
	if poster != "" {
		t.PrimaryImage = &image{URL: poster}
	}
	return t
}

// ---- handlers -------------------------------------------------------------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// GET /titles/{id} and /titles/{id}/episodes (other suffixes return the title).
func handleTitle(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/titles/")
	parts := strings.SplitN(rest, "/", 2)
	id := parts[0]
	if id == "" {
		writeJSON(w, 400, map[string]string{"error": "id required"})
		return
	}
	if len(parts) == 2 && strings.HasPrefix(parts[1], "episodes") {
		handleEpisodes(w, id)
		return
	}
	if v, ok := titleCache.get(id); ok {
		writeJSON(w, 200, v)
		return
	}
	// Cinemeta needs the type in the path. Try SERIES first: Cinemeta's movie
	// endpoint returns a wrong-but-id-matching entry for series ids (an internal
	// collision — e.g. Euphoria's id yields "Burial Boys"), whereas its series
	// endpoint returns nothing for real movies. So series-first disambiguates
	// correctly for both.
	var meta *cinemetaMeta
	var err error
	for _, kind := range []string{"series", "movie"} {
		meta, err = fetchMeta(kind, id)
		if err == nil {
			break
		}
	}
	if meta == nil {
		writeJSON(w, 404, map[string]string{"error": "title not found: " + id})
		return
	}
	title := toTitle(meta)
	titleCache.set(id, title, 24*time.Hour)
	writeJSON(w, 200, title)
}

// GET /search/titles?query=&type=
func handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("query"))
	if q == "" {
		writeJSON(w, 400, map[string]string{"error": "query required"})
		return
	}
	typeFilter := r.URL.Query().Get("type") // "movie" | "tvSeries" | ""
	ck := typeFilter + "|" + strings.ToLower(q)
	if v, ok := searchCache.get(ck); ok {
		writeJSON(w, 200, v)
		return
	}

	kinds := []string{"movie", "series"}
	switch typeFilter {
	case "movie":
		kinds = []string{"movie"}
	case "tvSeries", "series", "tvMiniSeries":
		kinds = []string{"series"}
	}

	seen := map[string]bool{}
	out := searchResponse{Titles: []imdbTitle{}}
	for _, kind := range kinds {
		metas, err := fetchCatalog(kind, q)
		if err != nil {
			log.Printf("search %s %q: %v", kind, q, err)
			continue
		}
		for i := range metas {
			if seen[metas[i].ID] {
				continue
			}
			seen[metas[i].ID] = true
			out.Titles = append(out.Titles, toTitle(&metas[i]))
		}
	}
	searchCache.set(ck, out, 6*time.Hour)
	writeJSON(w, 200, out)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
func firstNonZero(a, b int) int {
	if a != 0 {
		return a
	}
	return b
}

// GET /titles/{id}/episodes — series episodes from Cinemeta's videos[], in the
// shape the Omnius app expects. All episodes come in one page (nextPageToken empty).
func handleEpisodes(w http.ResponseWriter, id string) {
	if v, ok := epCache.get(id); ok {
		writeJSON(w, 200, v)
		return
	}
	meta, err := fetchMeta("series", id)
	if err != nil || meta == nil {
		writeJSON(w, 200, episodesResponse{Episodes: []imdbEpisode{}})
		return
	}
	out := episodesResponse{Episodes: []imdbEpisode{}}
	for _, v := range meta.Videos {
		if v.Season <= 0 {
			continue // skip specials (season 0)
		}
		ep := imdbEpisode{
			ID:            v.ID,
			Title:         firstNonEmpty(v.Title, v.Name),
			Season:        strconv.Itoa(v.Season),
			EpisodeNumber: firstNonZero(v.Episode, v.Number),
			Plot:          v.Overview,
		}
		if v.Thumbnail != "" {
			ep.PrimaryImage = &image{URL: v.Thumbnail}
		}
		out.Episodes = append(out.Episodes, ep)
	}
	out.TotalCount = len(out.Episodes)
	epCache.set(id, out, 12*time.Hour)
	writeJSON(w, 200, out)
}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/titles/", handleTitle) // /titles/{id} (+ ignored suffixes)
	mux.HandleFunc("/search/titles", handleSearch)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			writeJSON(w, 200, map[string]string{"service": "omnius-cinemeta", "upstream": cinemetaBase})
			return
		}
		http.NotFound(w, r)
	})

	addr := ":" + env("PORT", "8080")
	log.Printf("omnius-cinemeta on %s (upstream %s)", addr, cinemetaBase)
	log.Fatal(http.ListenAndServe(addr, mux))
}
