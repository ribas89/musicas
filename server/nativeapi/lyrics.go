package nativeapi

import (
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/Masterminds/squirrel"
	"github.com/go-chi/chi/v5"
	lyricssvc "github.com/navidrome/navidrome/core/lyrics"
	"github.com/navidrome/navidrome/model"
)

type lyricsFolder struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type lyricsSong struct {
	ID       string  `json:"id"`
	Title    string  `json:"title"`
	Artist   string  `json:"artist"`
	Album    string  `json:"album"`
	Duration float32 `json:"duration"`
}

func (api *Router) addLyricsRoute(r chi.Router) {
	r.Route("/lyrics", func(r chi.Router) {
		r.Get("/status", api.lyricsStatus)
		r.Get("/folders", api.lyricsFolders)
		r.Post("/scan", api.lyricsScan)
		r.Get("/songs", api.lyricsSongs)
		r.Get("/search", api.lyricsGlobalSearch)
		r.Post("/apply", api.lyricsApplySearchResult)
		r.Get("/song/{id}/search", api.lyricsSearch)
		r.Post("/song/{id}/apply", api.lyricsApply)
	})
}

func (api *Router) lyricsStatus(w http.ResponseWriter, r *http.Request) {
	root, libraryID, err := api.musicaUserRoot(r)
	if err != nil {
		musicaError(w, http.StatusForbidden, "Private library unavailable")
		return
	}
	_ = root.Close()
	status := api.lyricsFetcher().StatusForLibrary(libraryID)
	response := map[string]any{
		"scanning":  status.Running,
		"available": !status.Running,
	}
	if status.ProgressAvailable {
		response["progressAvailable"] = true
		response["total"] = status.Total
		response["processed"] = status.Processed
		response["remaining"] = status.Remaining
		response["percent"] = status.Percent
	}
	musicaJSON(w, http.StatusOK, response)
}

func (api *Router) lyricsFolders(w http.ResponseWriter, r *http.Request) {
	root, _, err := api.musicaUserRoot(r)
	if err != nil {
		musicaError(w, http.StatusForbidden, "Private library unavailable")
		return
	}
	defer root.Close()

	dir, err := root.Open(".")
	if err != nil {
		musicaError(w, http.StatusInternalServerError, "Cannot list private library folders")
		return
	}
	entries, err := dir.ReadDir(-1)
	_ = dir.Close()
	if err != nil {
		musicaError(w, http.StatusInternalServerError, "Cannot list private library folders")
		return
	}

	folders := make([]lyricsFolder, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || strings.Contains(entry.Name(), ":") || musicaSafeName(entry.Name()) != nil {
			continue
		}
		folder, err := root.Open(entry.Name())
		if err != nil {
			continue
		}
		info, statErr := folder.Stat()
		_ = folder.Close()
		if statErr != nil || !info.IsDir() {
			continue
		}
		folders = append(folders, lyricsFolder{Name: entry.Name(), Path: entry.Name()})
	}
	sort.Slice(folders, func(i, j int) bool {
		return strings.ToLower(folders[i].Name) < strings.ToLower(folders[j].Name)
	})
	musicaJSON(w, http.StatusOK, map[string]any{"folders": folders})
}

func (api *Router) lyricsScan(w http.ResponseWriter, r *http.Request) {
	root, libraryID, err := api.musicaUserRoot(r)
	if err != nil {
		musicaError(w, http.StatusForbidden, "Private library unavailable")
		return
	}
	defer root.Close()

	var body struct {
		Folder string `json:"folder"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
			musicaError(w, http.StatusBadRequest, "Invalid scan request")
			return
		}
	}

	folderPath := ""
	if strings.TrimSpace(body.Folder) != "" {
		folderPath, err = musicaCleanRelative(body.Folder, false)
		if err != nil {
			musicaError(w, http.StatusBadRequest, "Invalid library folder")
			return
		}
		folder, openErr := root.Open(folderPath)
		if openErr != nil {
			musicaError(w, http.StatusNotFound, "Library folder not found")
			return
		}
		info, statErr := folder.Stat()
		_ = folder.Close()
		if statErr != nil || !info.IsDir() {
			musicaError(w, http.StatusBadRequest, "Selected path is not a folder")
			return
		}
	}

	if !api.lyricsFetcher().StartScan(r.Context(), []model.ScanTarget{{LibraryID: libraryID, FolderPath: folderPath}}) {
		musicaError(w, http.StatusConflict, "A lyrics scan is already running")
		return
	}
	musicaJSON(w, http.StatusAccepted, map[string]bool{"started": true})
}

func (api *Router) lyricsGlobalSearch(w http.ResponseWriter, r *http.Request) {
	root, _, err := api.musicaUserRoot(r)
	if err != nil {
		musicaError(w, http.StatusForbidden, "Private library unavailable")
		return
	}
	_ = root.Close()

	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if len([]rune(query)) < 2 {
		musicaJSON(w, http.StatusOK, map[string]any{"results": []lyricssvc.LRCLIBTrack{}})
		return
	}
	if len([]rune(query)) > 160 {
		query = string([]rune(query)[:160])
	}
	results, err := api.lyricsFetcher().SearchLyrics(r.Context(), query)
	if err != nil {
		musicaError(w, http.StatusBadGateway, "Could not search the lyrics service")
		return
	}
	musicaJSON(w, http.StatusOK, map[string]any{"results": results})
}

func (api *Router) lyricsSongs(w http.ResponseWriter, r *http.Request) {
	root, libraryID, err := api.musicaUserRoot(r)
	if err != nil {
		musicaError(w, http.StatusForbidden, "Private library unavailable")
		return
	}
	_ = root.Close()

	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(query) < 2 {
		musicaJSON(w, http.StatusOK, map[string]any{"songs": []lyricsSong{}})
		return
	}
	if len([]rune(query)) > 160 {
		query = string([]rune(query)[:160])
	}
	like := "%" + query + "%"
	files, err := api.ds.MediaFile().GetAll(r.Context(), model.QueryOptions{
		Filters: squirrel.And{
			squirrel.Eq{"media_file.library_id": libraryID},
			squirrel.Eq{"media_file.missing": false},
			squirrel.Or{
				squirrel.Like{"media_file.title": like},
				squirrel.Like{"media_file.artist": like},
				squirrel.Like{"media_file.album": like},
				squirrel.Like{"media_file.path": like},
			},
		},
		Sort:  "title",
		Order: "asc",
		Max:   40,
	})
	if err != nil {
		musicaError(w, http.StatusInternalServerError, "Cannot search your music library")
		return
	}

	songs := make([]lyricsSong, 0, len(files))
	for _, file := range files {
		if file.LibraryID != libraryID || file.Missing {
			continue
		}
		artist := file.Artist
		if artist == "" {
			artist = file.AlbumArtist
		}
		songs = append(songs, lyricsSong{
			ID:       file.ID,
			Title:    file.Title,
			Artist:   artist,
			Album:    file.Album,
			Duration: file.Duration,
		})
	}
	musicaJSON(w, http.StatusOK, map[string]any{"songs": songs})
}

func (api *Router) lyricsApplySearchResult(w http.ResponseWriter, r *http.Request) {
	root, libraryID, err := api.musicaUserRoot(r)
	if err != nil {
		musicaError(w, http.StatusForbidden, "Private library unavailable")
		return
	}
	_ = root.Close()

	var body struct {
		SongID   string `json:"songId"`
		Query    string `json:"query"`
		ResultID int64  `json:"resultId"`
		Format   string `json:"format"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		musicaError(w, http.StatusBadRequest, "Invalid lyrics selection")
		return
	}
	body.Query = strings.TrimSpace(body.Query)
	if body.SongID == "" || len([]rune(body.Query)) < 2 || len([]rune(body.Query)) > 160 || body.ResultID <= 0 || (body.Format != "synced" && body.Format != "plain") {
		musicaError(w, http.StatusBadRequest, "Invalid lyrics selection")
		return
	}
	mediaFile, err := api.ds.MediaFile().GetWithParticipants(r.Context(), body.SongID)
	if err != nil || mediaFile == nil || mediaFile.LibraryID != libraryID || mediaFile.Missing || mediaFile.Path == "" {
		musicaError(w, http.StatusNotFound, "Song not found in your private library")
		return
	}
	if err := api.lyricsFetcher().AssociateSearchResult(r.Context(), mediaFile, body.Query, body.ResultID, body.Format); err != nil {
		if err == lyricssvc.ErrLyricsCandidate {
			musicaError(w, http.StatusConflict, "Selected lyrics result is no longer available. Search again.")
			return
		}
		musicaError(w, http.StatusBadGateway, "Could not save the selected lyrics")
		return
	}
	musicaJSON(w, http.StatusOK, map[string]any{"saved": true, "format": body.Format})
}

func (api *Router) lyricsSearch(w http.ResponseWriter, r *http.Request) {
	mediaFile, ok := api.lyricsUserSong(w, r)
	if !ok {
		return
	}
	results, err := api.lyricsFetcher().SearchForSong(r.Context(), mediaFile)
	if err != nil {
		musicaError(w, http.StatusBadGateway, "Could not search the lyrics service")
		return
	}
	musicaJSON(w, http.StatusOK, map[string]any{"results": results})
}

func (api *Router) lyricsApply(w http.ResponseWriter, r *http.Request) {
	mediaFile, ok := api.lyricsUserSong(w, r)
	if !ok {
		return
	}
	var body struct {
		ResultID int64  `json:"resultId"`
		Format   string `json:"format"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		musicaError(w, http.StatusBadRequest, "Invalid lyrics selection")
		return
	}
	if body.ResultID <= 0 || (body.Format != "synced" && body.Format != "plain") {
		musicaError(w, http.StatusBadRequest, "Invalid lyrics selection")
		return
	}
	if err := api.lyricsFetcher().Associate(r.Context(), mediaFile, body.ResultID, body.Format); err != nil {
		if err == lyricssvc.ErrLyricsCandidate {
			musicaError(w, http.StatusConflict, "Selected lyrics result is no longer available. Search again.")
			return
		}
		musicaError(w, http.StatusBadGateway, "Could not save the selected lyrics")
		return
	}
	musicaJSON(w, http.StatusOK, map[string]any{"saved": true, "format": body.Format})
}

func (api *Router) lyricsUserSong(w http.ResponseWriter, r *http.Request) (*model.MediaFile, bool) {
	root, libraryID, err := api.musicaUserRoot(r)
	if err != nil {
		musicaError(w, http.StatusForbidden, "Private library unavailable")
		return nil, false
	}
	_ = root.Close()

	id := chi.URLParam(r, "id")
	if id == "" {
		musicaError(w, http.StatusBadRequest, "Song ID is required")
		return nil, false
	}
	mediaFile, err := api.ds.MediaFile().GetWithParticipants(r.Context(), id)
	if err != nil || mediaFile == nil || mediaFile.LibraryID != libraryID || mediaFile.Missing {
		musicaError(w, http.StatusNotFound, "Song not found in your private library")
		return nil, false
	}
	return mediaFile, true
}

func (api *Router) lyricsFetcher() *lyricssvc.Fetcher {
	api.lyricsOnce.Do(func() {
		api.lyrics = lyricssvc.NewFetcher(api.ds)
	})
	return api.lyrics
}
