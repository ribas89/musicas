package nativeapi

// MusicasYT bridge. The browser talks only to authenticated Navidrome routes;
// the Go server supplies the authenticated username and the worker API key.
// No worker secrets or user-selected filesystem paths reach the browser.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/navidrome/navidrome/model"

	"github.com/go-chi/chi/v5"
	"github.com/navidrome/navidrome/model/request"
)

var musicaYoutubePlaylistID = regexp.MustCompile(`^[A-Za-z0-9_-]{5,128}$`)

const musicaYTMaxBody = 1 << 20 // Never forward more than 1 MiB of worker JSON.

// Match the accepted playlist URL shape in MusicasYT's app/common.py.
func musicaPlaylistIDFromURL(raw string) (string, error) {
	if len(raw) > 2048 {
		return "", errors.New("playlist URL is too long")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Fragment != "" {
		return "", errors.New("expected an HTTPS YouTube playlist URL")
	}
	switch strings.ToLower(parsed.Hostname()) {
	case "youtube.com", "www.youtube.com", "music.youtube.com", "m.youtube.com":
	default:
		return "", errors.New("expected a YouTube playlist URL")
	}
	if parsed.Port() != "" && parsed.Port() != "443" {
		return "", errors.New("invalid YouTube URL port")
	}
	ids := parsed.Query()["list"]
	if len(ids) != 1 || !musicaYoutubePlaylistID.MatchString(ids[0]) {
		return "", errors.New("YouTube playlist ID is missing or invalid")
	}
	return ids[0], nil
}

func musicaYTSettings() (*url.URL, string, error) {
	rawURL := strings.TrimSpace(os.Getenv("MUSICASYT_URL"))
	key := os.Getenv("MUSICASYT_API_KEY")
	if rawURL == "" || key == "" {
		return nil, "", errors.New("configure MUSICASYT_URL and MUSICASYT_API_KEY in Musicas")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return nil, "", errors.New("MUSICASYT_URL must be an HTTP(S) origin without a path")
	}
	return parsed, key, nil
}

// No automatic retries on POST here: the client retries only when it knows the
// worker is warming up. Worker job IDs deduplicate repeated POST requests.
func musicaYTCall(method, endpoint string, body any) (int, json.RawMessage, error) {
	origin, key, err := musicaYTSettings()
	if err != nil {
		return http.StatusServiceUnavailable, nil, err
	}
	var input io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 500, nil, errors.New("could not encode request")
		}
		input = bytes.NewReader(encoded)
	}
	// endpoint is an internal route; components are already PathEscaped by the
	// caller. Concatenate with the validated origin to avoid double-encoding %.
	req, err := http.NewRequest(method, origin.String()+endpoint, input)
	if err != nil {
		return 500, nil, errors.New("could not create worker request")
	}
	if endpoint != "/status" {
		req.Header.Set("X-API-Key", key)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{
		Timeout: 90 * time.Second, // Account for Sablier waking a sleeping worker.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// Never forward the shared API key to another origin.
			if len(via) >= 3 || req.URL.Scheme != origin.Scheme || !strings.EqualFold(req.URL.Host, origin.Host) {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
	response, err := client.Do(req)
	if err != nil {
		return http.StatusServiceUnavailable, nil, errors.New("MusicasYT is unavailable or waking up; try again shortly")
	}
	defer response.Body.Close()
	contents, err := io.ReadAll(io.LimitReader(response.Body, musicaYTMaxBody+1))
	if err != nil || len(contents) > musicaYTMaxBody || !json.Valid(contents) {
		return http.StatusServiceUnavailable, nil, errors.New("MusicasYT did not return valid JSON; it may be starting")
	}
	return response.StatusCode, contents, nil
}

func (api *Router) musicaYTAuthorizedUser(w http.ResponseWriter, r *http.Request) (string, int, bool) {
	user, ok := request.UserFrom(r.Context())
	if !ok || user.UserName == "" {
		musicaError(w, http.StatusUnauthorized, "Sign in to manage YouTube playlists")
		return "", 0, false
	}
	// Require a native personal library, as with the Files manager. No ability
	// to send jobs for other usernames, administrators, or /music/public.
	root, libID, err := api.musicaUserRoot(r)
	if err != nil {
		musicaError(w, http.StatusForbidden, "Your private library is not available")
		return "", 0, false
	}
	_ = root.Close()
	return user.UserName, libID, true
}

func musicaYTForward(w http.ResponseWriter, status int, contents json.RawMessage, err error) {
	if err != nil {
		musicaError(w, status, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(contents)
}

func (api *Router) addMusicaYouTubeRoute(r chi.Router) {
	r.Route("/youtube", func(r chi.Router) {
		r.Get("/status", api.musicaYTStatus)
		r.Get("/playlists", api.musicaYTPlaylists)
		r.Delete("/playlists/{playlistID}", api.musicaYTDeletePlaylist)
		r.Post("/sync", api.musicaYTSync)
		r.Get("/jobs/{playlistID}", api.musicaYTJob)
	})
}

// Snapshot data, separate from volatile jobs. The worker writes an atomic JSON
// manifest to the authenticated user's own /music/users/<username> directory.
// Reading it never wakes the sleeping worker or visits YouTube.
type musicaYTStoredPlaylist struct {
	PlaylistID        string            `json:"playlist_id"`
	PlaylistURL       string            `json:"playlist_url,omitempty"`
	Title             string            `json:"title"`
	FolderName        string            `json:"folder_name,omitempty"`
	Status            string            `json:"status,omitempty"`
	Total             *int              `json:"total,omitempty"`
	Downloaded        int               `json:"downloaded"`
	Existing          int               `json:"existing"`
	Failed            int               `json:"failed"`
	Unavailable       int               `json:"unavailable"`
	Remaining         *int              `json:"remaining,omitempty"`
	Percent           *float64          `json:"percent,omitempty"`
	Error             string            `json:"error,omitempty"`
	CurrentTrack      string            `json:"current_track,omitempty"`
	UpdatedAt         string            `json:"updated_at,omitempty"`
	LastAttemptAt     string            `json:"last_attempt_at,omitempty"`
	LastSyncedAt      string            `json:"last_synced_at,omitempty"`
	Folder            string            `json:"folder,omitempty"`
	TracksUnavailable map[string]string `json:"tracks_unavailable,omitempty"`
	TracksError       map[string]string `json:"tracks_error,omitempty"`
}

func (api *Router) musicaYTPlaylists(w http.ResponseWriter, r *http.Request) {
	username, _, ok := api.musicaYTAuthorizedUser(w, r)
	if !ok {
		return
	}
	root, _, err := api.musicaUserRoot(r)
	if err != nil {
		musicaError(w, 403, "Private library unavailable")
		return
	}
	defer root.Close()
	playlists := map[string]musicaYTStoredPlaylist{}
	file, err := root.Open("musicasyt.json")
	if err == nil {
		defer file.Close()
		stat, statErr := file.Stat()
		if statErr != nil || !stat.Mode().IsRegular() || stat.Size() > 16<<20 {
			musicaError(w, 500, "Invalid YouTube playlist metadata")
			return
		}
		contents, readErr := io.ReadAll(io.LimitReader(file, 16<<20+1))
		if readErr != nil || len(contents) > 16<<20 {
			musicaError(w, 500, "Cannot read YouTube playlist metadata")
			return
		}
		var document struct {
			Playlists map[string]musicaYTStoredPlaylist `json:"playlists"`
		}
		if json.Unmarshal(contents, &document) != nil {
			musicaError(w, 500, "Cannot parse YouTube playlist metadata")
			return
		}
		for id, record := range document.Playlists {
			if !musicaYoutubePlaylistID.MatchString(id) {
				continue
			}
			record.PlaylistID = id
			if record.PlaylistURL == "" {
				record.PlaylistURL = "https://music.youtube.com/playlist?list=" + id
			}
			if record.FolderName != "" && filepath.Base(record.FolderName) == record.FolderName && record.FolderName != "." && record.FolderName != ".." {
				record.Folder = "/music/users/" + username + "/" + record.FolderName
			}
			playlists[id] = record
		}
	} else if !os.IsNotExist(err) {
		musicaError(w, 500, "Cannot access YouTube playlist metadata")
		return
	}
	// Upgrade old worker installs transparently when only folder-local
	// .musicasyt.json markers exist. The worker migrates these on next Sync.
	dir, err := root.Open(".")
	if err == nil {
		entries, _ := dir.ReadDir(-1)
		_ = dir.Close()
		for _, entry := range entries {
			if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
				continue
			}
			marker, err := root.Open(entry.Name() + "/.musicasyt.json")
			if err != nil {
				continue
			}
			var old struct {
				PlaylistID string `json:"playlist_id"`
				Title      string `json:"title"`
			}
			info, statErr := marker.Stat()
			if statErr == nil && info.Mode().IsRegular() && info.Size() < 4096 {
				data, readErr := io.ReadAll(io.LimitReader(marker, 4096))
				if readErr == nil && json.Unmarshal(data, &old) == nil && musicaYoutubePlaylistID.MatchString(old.PlaylistID) {
					if _, found := playlists[old.PlaylistID]; !found {
						playlists[old.PlaylistID] = musicaYTStoredPlaylist{
							PlaylistID:  old.PlaylistID,
							PlaylistURL: "https://music.youtube.com/playlist?list=" + old.PlaylistID,
							Title:       old.Title, FolderName: entry.Name(),
							Folder: "/music/users/" + username + "/" + entry.Name(),
							Status: "previously_synced",
						}
					}
				}
			}
			_ = marker.Close()
		}
	}
	result := make([]musicaYTStoredPlaylist, 0, len(playlists))
	for _, record := range playlists {
		result = append(result, record)
	}
	sort.Slice(result, func(i, j int) bool { return strings.ToLower(result[i].Title) < strings.ToLower(result[j].Title) })
	musicaJSON(w, 200, map[string]any{"playlists": result})
}

func (api *Router) musicaYTDeletePlaylist(w http.ResponseWriter, r *http.Request) {
	username, libraryID, ok := api.musicaYTAuthorizedUser(w, r)
	if !ok {
		return
	}
	playlistID := chi.URLParam(r, "playlistID")
	if !musicaYoutubePlaylistID.MatchString(playlistID) {
		musicaError(w, http.StatusBadRequest, "Invalid playlist ID")
		return
	}
	endpoint := "/playlists/" + url.PathEscape(username) + "/" + url.PathEscape(playlistID)
	status, result, err := musicaYTCall(http.MethodDelete, endpoint, nil)
	if err == nil && status >= 200 && status < 300 {
		// The directory was actually removed; refresh native folder playlists.
		go api.musicaYTScanLibrary(libraryID)
	}
	musicaYTForward(w, status, result, err)
}

func (api *Router) musicaYTStatus(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := api.musicaYTAuthorizedUser(w, r); !ok {
		return
	}
	status, result, err := musicaYTCall(http.MethodGet, "/status", nil)
	musicaYTForward(w, status, result, err)
}

func (api *Router) musicaYTSync(w http.ResponseWriter, r *http.Request) {
	username, libID, ok := api.musicaYTAuthorizedUser(w, r)
	if !ok {
		return
	}
	var payload struct {
		URL     string `json:"playlist_url"`
		Restart bool   `json:"restart"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&payload); err != nil {
		musicaError(w, http.StatusBadRequest, "Invalid synchronization request")
		return
	}
	if _, err := musicaPlaylistIDFromURL(payload.URL); err != nil {
		musicaError(w, http.StatusBadRequest, err.Error())
		return
	}
	status, result, err := musicaYTCall(http.MethodPost, "/sync", map[string]any{
		"username":     username,
		"playlist_url": payload.URL,
		"restart":      payload.Restart,
	})
	// A memory-only watcher keeps Sablier's worker session active while a job
	// runs, and scans the correct Navidrome library even if the browser closes.
	if err == nil && status >= 200 && status < 300 {
		var job musicaYTJobState
		if json.Unmarshal(result, &job) == nil && job.JobID == username+":"+job.PlaylistID && job.CreatedAt != "" {
			api.musicaYTWatch(job, libID)
		}
	}
	musicaYTForward(w, status, result, err)
}

func (api *Router) musicaYTJob(w http.ResponseWriter, r *http.Request) {
	username, _, ok := api.musicaYTAuthorizedUser(w, r)
	if !ok {
		return
	}
	playlistID := chi.URLParam(r, "playlistID")
	if !musicaYoutubePlaylistID.MatchString(playlistID) {
		musicaError(w, http.StatusBadRequest, "Invalid playlist ID")
		return
	}
	endpoint := "/jobs/" + url.PathEscape(username) + "/" + url.PathEscape(playlistID)
	status, result, err := musicaYTCall(http.MethodGet, endpoint, nil)
	musicaYTForward(w, status, result, err)
}

// In-memory only. Restarting Musicas forgets watchers just as restarting
// MusicasYT forgets active jobs; the scheduled scanner remains the fallback.
type musicaYTJobState struct {
	JobID      string `json:"job_id"`
	PlaylistID string `json:"playlist_id"`
	Status     string `json:"status"`
	CreatedAt  string `json:"created_at"`
}

func musicaYTTerminal(status string) bool {
	return status == "completed" || status == "completed_with_errors" || status == "error"
}

func (api *Router) musicaYTWatch(job musicaYTJobState, libraryID int) {
	if libraryID <= 0 || musicaYTTerminal(job.Status) {
		// A completed job can still be returned by an existing-job request.
		if job.Status == "completed" || job.Status == "completed_with_errors" {
			// If the worker completed before the first polling tick, scan too.
			key := job.JobID + ":" + job.CreatedAt
			if _, loaded := api.ytWatching.LoadOrStore(key, true); !loaded {
				go func() { defer api.ytWatching.Delete(key); api.musicaYTScanLibrary(libraryID) }()
			}
		}
		return
	}
	key := job.JobID + ":" + job.CreatedAt
	if _, loaded := api.ytWatching.LoadOrStore(key, true); loaded {
		return
	}
	go func() {
		defer api.ytWatching.Delete(key)
		ticker := time.NewTicker(12 * time.Second)
		defer ticker.Stop()
		deadline := time.NewTimer(12 * time.Hour)
		defer deadline.Stop()
		for {
			select {
			case <-deadline.C:
				return
			case <-ticker.C:
				status, result, err := musicaYTCall(http.MethodGet, "/jobs/"+url.PathEscape(strings.SplitN(job.JobID, ":", 2)[0])+"/"+url.PathEscape(job.PlaylistID), nil)
				if err != nil {
					continue // Sablier might be waking or upgrading upstream dependencies.
				}
				if status == http.StatusNotFound {
					return // Worker restarted, job lost by design.
				}
				if status != http.StatusOK {
					continue
				}
				var latest musicaYTJobState
				if json.Unmarshal(result, &latest) != nil || latest.JobID != job.JobID || latest.CreatedAt != job.CreatedAt {
					return // A new job replaced this one.
				}
				if musicaYTTerminal(latest.Status) {
					if latest.Status != "error" {
						api.musicaYTScanLibrary(libraryID)
					}
					return
				}
			}
		}
	}()
}

func (api *Router) musicaYTScanLibrary(libraryID int) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	// Wait for a concurrent Files or scheduled scan to finish, rather than
	// aborting a newly completed YouTube playlist without a private scan.
	for {
		status, err := api.scanner.Status(ctx)
		if err == nil && !status.Scanning && api.filesScanning.CompareAndSwap(false, true) {
			defer api.filesScanning.Store(false)
			_, _ = api.scanner.ScanFolders(ctx, false, []model.ScanTarget{{LibraryID: libraryID}})
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(15 * time.Second):
		}
	}
}
