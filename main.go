package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxPlaylistTracks = 100
	maxSearchResults  = 6
	maxJobAge         = 45 * time.Minute
	maxJobsInQueue    = 80
)

var (
	playlistRE = regexp.MustCompile(`(?i)^https?://(?:open\\.)?spotify\\.com/playlist/([A-Za-z0-9]+)`)
	nextDataRE = regexp.MustCompile(`(?s)<script[^>]*\\bid=["']__NEXT_DATA__["'][^>]*>(.*?)</script>`)
	wsRE       = regexp.MustCompile(`\\s+`)
	httpClient = &http.Client{Timeout: 90 * time.Second}

	jobMu  sync.Mutex
	jobs   = map[string]*DownloadJob{}
	jobSem = make(chan struct{}, 1) // keep the prototype gentle on a small server
)

type Track struct {
	Title      string `json:"title"`
	Artist     string `json:"artist"`
	DurationMs int64  `json:"durationMs"`
	SpotifyURL string `json:"spotifyUrl"`
	Index      int    `json:"index"`
}

type Playlist struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Owner      string  `json:"owner"`
	URL        string  `json:"url"`
	Tracks     []Track `json:"tracks"`
	TrackCount int     `json:"trackCount"`
}

type YTResult struct {
	ID       string  `json:"id"`
	Title    string  `json:"title"`
	Duration float64 `json:"duration"`
}

type DownloadJob struct {
	ID        string    `json:"id"`
	Index     int       `json:"index"`
	Title     string    `json:"title"`
	Artist    string    `json:"artist"`
	Status    string    `json:"status"`
	Message   string    `json:"message"`
	Candidate string    `json:"candidate"`
	File      string    `json:"file,omitempty"`
	Done      bool      `json:"done"`
	ExpiresAt time.Time `json:"-"`
}

type entity struct {
	Name      string
	Subtitle  string
	TrackList []map[string]any
}

func main() {
	go cleanupLoop()

	mux := http.NewServeMux()
	mux.HandleFunc("/", serveIndex)
	mux.HandleFunc("/api/health", healthAPI)
	mux.HandleFunc("/api/playlist", playlistAPI)
	mux.HandleFunc("/api/download", downloadAPI)
	mux.HandleFunc("/api/job", jobAPI)
	mux.HandleFunc("/api/file", fileAPI)

	port := strings.TrimSpace(os.Getenv("PORT"))
	if port == "" {
		port = "10000"
	}

	server := &http.Server{
		Addr:              "0.0.0.0:" + port,
		Handler:           requestLogger(mux),
		ReadHeaderTimeout: 15 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      0, // downloads may take time
		IdleTimeout:       60 * time.Second,
	}

	log.Printf("Nightcache listening on %s", server.Addr)
	log.Fatal(server.ListenAndServe())
}

func serveIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	b, err := os.ReadFile("index.html")
	if err != nil {
		http.Error(w, "index.html not found", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(b)
}

func healthAPI(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "service": "nightcache"})
}

func playlistAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := parsePlaylistID(strings.TrimSpace(r.URL.Query().Get("url")))
	if id == "" {
		jsonError(w, http.StatusBadRequest, "Нужна публичная ссылка Spotify-плейлиста.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 75*time.Second)
	defer cancel()
	p, err := fetchPlaylist(ctx, id)
	if err != nil {
		jsonError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func fetchPlaylist(ctx context.Context, id string) (Playlist, error) {
	publicURL := "https://open.spotify.com/playlist/" + id
	targets := []string{
		"https://open.spotify.com/embed/playlist/" + id + "?utm_source=generator&theme=0",
		"https://open.spotify.com/embed/playlist/" + id,
	}
	var lastErr error
	for _, target := range targets {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/154 Safari/537.36")
		req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
		resp, err := httpClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			lastErr = fmt.Errorf("Spotify вернул HTTP %d", resp.StatusCode)
			continue
		}
		if p, err := parseSpotifyHTML(id, publicURL, body); err == nil {
			return p, nil
		} else {
			lastErr = err
		}
	}
	if lastErr == nil {
		lastErr = errors.New("Не удалось получить плейлист Spotify.")
	}
	return Playlist{}, lastErr
}

func parseSpotifyHTML(id, publicURL string, body []byte) (Playlist, error) {
	m := nextDataRE.FindSubmatch(body)
	if len(m) >= 2 {
		var root any
		if json.Unmarshal(m[1], &root) == nil {
			if e := findEntity(root); e != nil && len(e.TrackList) > 0 {
				return buildPlaylist(id, publicURL, e), nil
			}
		}
	}

	// Fallback: locate an embedded JSON object containing trackList.
	text := string(body)
	idx := strings.Index(text, `"trackList"`)
	if idx >= 0 {
		for start := strings.LastIndex(text[:idx], "{"); start >= 0; start = strings.LastIndex(text[:start], "{") {
			candidate := text[start:]
			if end := strings.Index(candidate, "</script>"); end > 0 {
				candidate = candidate[:end]
			}
			candidate = strings.TrimSpace(strings.TrimSuffix(candidate, ";"))
			var root any
			if json.Unmarshal([]byte(candidate), &root) == nil {
				if e := findEntity(root); e != nil && len(e.TrackList) > 0 {
					return buildPlaylist(id, publicURL, e), nil
				}
			}
			if idx-start > 250000 {
				break
			}
		}
	}
	return Playlist{}, errors.New("Spotify открыл страницу, но список треков не найден. Проверь, что плейлист публичный.")
}

func findEntity(v any) *entity {
	switch x := v.(type) {
	case map[string]any:
		if raw, ok := x["trackList"]; ok {
			if list, ok := raw.([]any); ok && len(list) > 0 {
				e := &entity{Name: str(x, "name"), Subtitle: firstNonEmpty(str(x, "subtitle"), str(x, "owner"))}
				for _, z := range list {
					if o, ok := z.(map[string]any); ok {
						e.TrackList = append(e.TrackList, o)
					}
				}
				if len(e.TrackList) > 0 {
					return e
				}
			}
		}
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if e := findEntity(x[k]); e != nil {
				return e
			}
		}
	case []any:
		for _, z := range x {
			if e := findEntity(z); e != nil {
				return e
			}
		}
	}
	return nil
}

func buildPlaylist(id, publicURL string, e *entity) Playlist {
	p := Playlist{ID: id, Name: e.Name, Owner: e.Subtitle, URL: publicURL}
	for i, item := range e.TrackList {
		title := firstNonEmpty(str(item, "title"), str(item, "name"))
		artist := firstNonEmpty(str(item, "subtitle"), str(item, "artist"))
		if title == "" {
			continue
		}
		var duration int64
		if n, ok := item["duration"].(float64); ok {
			duration = int64(n)
		}
		trackURL := publicURL
		if uri := str(item, "uri"); strings.HasPrefix(uri, "spotify:track:") {
			trackURL = "https://open.spotify.com/track/" + strings.TrimPrefix(uri, "spotify:track:")
		}
		p.Tracks = append(p.Tracks, Track{Title: title, Artist: artist, DurationMs: duration, SpotifyURL: trackURL, Index: i})
		if len(p.Tracks) >= maxPlaylistTracks {
			break
		}
	}
	p.TrackCount = len(p.Tracks)
	if p.TrackCount == 0 {
		return Playlist{}
	}
	return p
}

func str(m map[string]any, key string) string {
	if s, ok := m[key].(string); ok {
		return cleanText(s)
	}
	return ""
}

func cleanText(s string) string {
	return strings.TrimSpace(wsRE.ReplaceAllString(html.UnescapeString(strings.TrimSpace(s)), " "))
}

func firstNonEmpty(xs ...string) string {
	for _, s := range xs {
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func parsePlaylistID(raw string) string {
	raw = strings.TrimSpace(raw)
	if m := playlistRE.FindStringSubmatch(raw); len(m) == 2 {
		return m[1]
	}
	if strings.HasPrefix(raw, "spotify:playlist:") {
		return strings.TrimPrefix(raw, "spotify:playlist:")
	}
	return ""
}

func downloadAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	jobMu.Lock()
	active := 0
	for _, j := range jobs {
		if !j.Done {
			active++
		}
	}
	if active >= maxJobsInQueue {
		jobMu.Unlock()
		jsonError(w, http.StatusTooManyRequests, "Сервер сейчас занят. Попробуй ещё раз через минуту.")
		return
	}
	jobMu.Unlock()

	var req struct {
		Track Track `json:"track"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "Неверный запрос.")
		return
	}
	if strings.TrimSpace(req.Track.Title) == "" {
		jsonError(w, http.StatusBadRequest, "У трека нет названия.")
		return
	}

	id := randomID()
	job := &DownloadJob{
		ID:        id,
		Index:     req.Track.Index,
		Title:     req.Track.Title,
		Artist:    req.Track.Artist,
		Status:    "queued",
		Message:   "В очереди…",
		ExpiresAt: time.Now().Add(maxJobAge),
	}
	jobMu.Lock()
	jobs[id] = job
	jobMu.Unlock()

	go runDownload(job, req.Track)
	writeJSON(w, http.StatusOK, map[string]any{"jobId": id})
}

func runDownload(job *DownloadJob, t Track) {
	jobUpdate(job, func(j *DownloadJob) {
		j.Status = "searching"
		j.Message = "Ищу подходящий результат…"
	})

	// One worker at a time keeps a small public demo from creating a pile of yt-dlp processes.
	jobSem <- struct{}{}
	defer func() { <-jobSem }()

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	results, err := searchYouTube(ctx, t)
	if err != nil {
		finishJob(job, "error", err.Error(), "")
		return
	}
	if len(results) == 0 {
		finishJob(job, "error", "YouTube не вернул подходящих результатов.", "")
		return
	}

	sort.SliceStable(results, func(i, j int) bool { return scoreResult(t, results[i]) > scoreResult(t, results[j]) })

	dir, err := os.MkdirTemp("", "nightcache-")
	if err != nil {
		finishJob(job, "error", "Не удалось создать временную папку.", "")
		return
	}
	defer os.RemoveAll(dir)

	baseName := sanitizeFilename(fmt.Sprintf("%s - %s", t.Artist, t.Title))
	outputTemplate := filepath.Join(dir, baseName+".%(ext)s")
	clients := []string{"web_embedded", "android", "tv_simply", "web_safari"}

	for ri, result := range results {
		for ci, client := range clients {
			jobUpdate(job, func(j *DownloadJob) {
				j.Status = "downloading"
				j.Message = fmt.Sprintf("Скачиваю… вариант %d/%d · попытка %d/%d", ri+1, len(results), ci+1, len(clients))
				j.Candidate = result.Title
			})

			args := []string{
				"--no-warnings", "--no-playlist", "--newline",
				"--retries", "5", "--fragment-retries", "5",
				"--retry-sleep", "exp=1:6", "--sleep-requests", "1",
				"--sleep-interval", "1", "--max-sleep-interval", "3",
				"--socket-timeout", "45", "--force-ipv4",
				"--js-runtimes", "deno", "--remote-components", "ejs:github",
				"-f", "bestaudio/best", "-x", "--audio-format", "mp3", "--audio-quality", "0",
				"-o", outputTemplate,
				"--extractor-args", "youtube:player_client=" + client,
				"https://www.youtube.com/watch?v=" + result.ID,
			}
			out, cmdErr := runCmd(ctx, "yt-dlp", args...)
			if cmdErr == nil {
				file := findMP3(dir, baseName)
				if file != "" {
					jobMu.Lock()
					if j, ok := jobs[job.ID]; ok {
						j.Status = "done"
						j.Message = "Готово"
						j.File = file
						j.Done = true
					}
					jobMu.Unlock()
					return
				}
			}
			if strings.Contains(out, "Sign in to confirm you’re not a bot") || strings.Contains(out, "Sign in to confirm you're not a bot") {
				jobUpdate(job, func(j *DownloadJob) {
					j.Message = "YouTube потребовал дополнительную проверку. Пробую другой результат…"
				})
			}
			select {
			case <-ctx.Done():
				finishJob(job, "error", "Время ожидания загрузки истекло.", "")
				return
			case <-time.After(time.Duration(ci+1) * 1200 * time.Millisecond):
			}
		}
	}

	finishJob(job, "error", "Не удалось скачать этот трек после нескольких вариантов YouTube.", "")
}

func searchYouTube(ctx context.Context, t Track) ([]YTResult, error) {
	q := strings.TrimSpace(t.Artist + " - " + t.Title)
	clients := []string{"web_embedded", "android", "tv_simply", "web_safari"}
	var last string
	for _, client := range clients {
		args := []string{
			"--no-warnings", "--skip-download", "--flat-playlist",
			"--print", "%(id)s\\t%(title)s\\t%(duration)s", "--playlist-end", strconv.Itoa(maxSearchResults),
			"--js-runtimes", "deno", "--remote-components", "ejs:github",
			"--extractor-args", "youtube:player_client=" + client,
			"ytsearch" + strconv.Itoa(maxSearchResults) + ":" + q,
		}
		out, err := runCmd(ctx, "yt-dlp", args...)
		if err == nil {
			var res []YTResult
			sc := bufio.NewScanner(strings.NewReader(out))
			for sc.Scan() {
				parts := strings.SplitN(sc.Text(), "\\t", 3)
				if len(parts) < 2 || strings.TrimSpace(parts[0]) == "" {
					continue
				}
				var dur float64
				if len(parts) == 3 {
					dur, _ = strconv.ParseFloat(strings.TrimSpace(parts[2]), 64)
				}
				res = append(res, YTResult{ID: strings.TrimSpace(parts[0]), Title: strings.TrimSpace(parts[1]), Duration: dur})
			}
			if len(res) > 0 {
				return res, nil
			}
		}
		last = errString(err, out)
	}
	return nil, fmt.Errorf("YouTube не дал результаты поиска. %s", last)
}

func scoreResult(t Track, r YTResult) float64 {
	titleWords := tokenize(t.Title)
	artistWords := tokenize(t.Artist)
	got := strings.ToLower(r.Title)
	var score float64
	for _, word := range titleWords {
		if strings.Contains(got, word) {
			score += 4
		}
	}
	for _, word := range artistWords {
		if len(word) >= 3 && strings.Contains(got, word) {
			score += 2
		}
	}
	for _, bad := range []string{"karaoke", "cover", "sped up", "slowed", "nightcore", "8d audio", "instrumental", "reaction", "tribute"} {
		if strings.Contains(got, bad) {
			score -= 3.5
		}
	}
	for _, good := range []string{"official audio", "official video", "audio", "topic", "music video"} {
		if strings.Contains(got, good) {
			score += 1.5
		}
	}
	if r.Duration > 0 && t.DurationMs > 0 {
		d := abs(r.Duration - float64(t.DurationMs)/1000)
		switch {
		case d < 5:
			score += 4
		case d < 15:
			score += 2
		case d < 30:
			score += 0.5
		}
	}
	return score
}

func tokenize(s string) []string {
	s = strings.ToLower(s)
	s = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'а' && r <= 'я') || (r >= '0' && r <= '9') {
			return r
		}
		return ' '
	}, s)
	fields := strings.Fields(s)
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if len([]rune(f)) >= 2 {
			out = append(out, f)
		}
	}
	return out
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func runCmd(ctx context.Context, exe string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, exe, args...)
	env := os.Environ()
	env = append(env, "HOME=/tmp")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func findMP3(dir, base string) string {
	p := filepath.Join(dir, base+".mp3")
	st, err := os.Stat(p)
	if err == nil && !st.IsDir() && st.Size() > 10*1024 {
		return p
	}
	return ""
}

func fileAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := r.URL.Query().Get("id")
	jobMu.Lock()
	j, ok := jobs[id]
	if !ok || !j.Done || j.File == "" || time.Now().After(j.ExpiresAt) {
		jobMu.Unlock()
		http.Error(w, "file expired or not found", http.StatusNotFound)
		return
	}
	file := j.File
	name := sanitizeFilename(fmt.Sprintf("%s - %s.mp3", j.Artist, j.Title))
	jobMu.Unlock()

	f, err := os.Open(file)
	if err != nil {
		http.Error(w, "file unavailable", http.StatusNotFound)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		http.Error(w, "file unavailable", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	w.Header().Set("Content-Disposition", `attachment; filename="`+escapeHeaderFilename(name)+`"`)
	http.ServeContent(w, r, name, st.ModTime(), f)
}

func jobAPI(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	jobMu.Lock()
	j, ok := jobs[id]
	if !ok {
		jobMu.Unlock()
		jsonError(w, http.StatusNotFound, "Неизвестная загрузка.")
		return
	}
	cp := *j
	jobMu.Unlock()
	writeJSON(w, http.StatusOK, cp)
}

func jobUpdate(job *DownloadJob, fn func(*DownloadJob)) {
	jobMu.Lock()
	defer jobMu.Unlock()
	if j, ok := jobs[job.ID]; ok {
		fn(j)
	}
}

func finishJob(job *DownloadJob, status, message, file string) {
	jobUpdate(job, func(j *DownloadJob) {
		j.Status = status
		j.Message = message
		j.File = file
		j.Done = true
	})
}

func cleanupLoop() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		jobMu.Lock()
		for id, j := range jobs {
			if now.After(j.ExpiresAt) {
				if j.File != "" {
					_ = os.Remove(j.File)
					_ = os.Remove(filepath.Dir(j.File))
				}
				delete(jobs, id)
			}
		}
		jobMu.Unlock()
	}
}

func randomID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	return hex.EncodeToString(b)
}

func sanitizeFilename(s string) string {
	bad := `<>:"/\\|?*`
	s = strings.Map(func(r rune) rune {
		if strings.ContainsRune(bad, r) || r < 32 {
			return '_'
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if len(s) > 120 {
		s = s[:120]
	}
	if s == "" {
		s = "nightcache-track"
	}
	return s
}

func escapeHeaderFilename(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, `\\`, `_`), `"`, `_`)
}

func errString(err error, out string) string {
	out = strings.TrimSpace(out)
	if len(out) > 1800 {
		out = out[len(out)-1800:]
	}
	if err == nil {
		return out
	}
	if out == "" {
		return err.Error()
	}
	return err.Error() + " — " + out
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}
