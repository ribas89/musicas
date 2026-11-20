package lyrics

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Masterminds/squirrel"
	"github.com/navidrome/navidrome/core/storage"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"golang.org/x/text/unicode/norm"
)

const (
	lyricsRequestTimeout = 20 * time.Second
	lyricsMaxResponse    = 8 << 20
	lyricsDurationSlack  = 2.0
)

var (
	ErrScanBusy         = errors.New("lyrics scan already running")
	ErrNoMatchingLyrics = errors.New("no matching lyrics found")
	ErrLyricsCandidate  = errors.New("selected lyrics result is no longer available")
	lyricsScanRunning   atomic.Bool
	lyricsScanProgress  = struct {
		sync.RWMutex
		ownerLibraryID int
		total          int
		processed      int
		ready          bool
	}{}
	lyricsSidecarExts = []string{".ttml", ".yaml", ".yml", ".elrc", ".lrc", ".srt", ".txt"}
)

type Fetcher struct {
	ds      model.DataStore
	client  *http.Client
	baseURL string
}

// LRCLIBTrack is the subset of the LRCLIB API response used by Musicas.
type LRCLIBTrack struct {
	ID           int64   `json:"id"`
	TrackName    string  `json:"trackName"`
	ArtistName   string  `json:"artistName"`
	AlbumName    string  `json:"albumName"`
	Duration     float64 `json:"duration"`
	Instrumental bool    `json:"instrumental"`
	PlainLyrics  string  `json:"plainLyrics"`
	SyncedLyrics string  `json:"syncedLyrics"`
}

type LyricsLookup struct {
	Title  string
	Artist string
	Album  string
}

func NewFetcher(ds model.DataStore) *Fetcher {
	baseURL := strings.TrimSpace(os.Getenv("LYRICS_URL"))
	if baseURL == "" {
		baseURL = "https://lrclib.net"
	}
	return &Fetcher{
		ds:      ds,
		baseURL: baseURL,
		client:  &http.Client{Timeout: lyricsRequestTimeout},
	}
}

type ScanStatus struct {
	Running           bool
	ProgressAvailable bool
	Total             int
	Processed         int
	Remaining         int
	Percent           float64
}

func (f *Fetcher) IsScanning() bool {
	return lyricsScanRunning.Load()
}

// StatusForLibrary exposes progress only to the owner of a manual private-library scan.
// Scheduled global scans and scans started by another user expose only the busy state.
func (f *Fetcher) StatusForLibrary(libraryID int) ScanStatus {
	status := ScanStatus{Running: lyricsScanRunning.Load()}
	if !status.Running || libraryID <= 0 {
		return status
	}
	lyricsScanProgress.RLock()
	defer lyricsScanProgress.RUnlock()
	if lyricsScanProgress.ownerLibraryID != libraryID || !lyricsScanProgress.ready {
		return status
	}
	status.ProgressAvailable = true
	status.Total = lyricsScanProgress.total
	status.Processed = lyricsScanProgress.processed
	status.Remaining = status.Total - status.Processed
	if status.Remaining < 0 {
		status.Remaining = 0
	}
	if status.Total > 0 {
		status.Percent = float64(status.Processed) * 100 / float64(status.Total)
	}
	return status
}

func beginLyricsScanProgress(ownerLibraryID int) {
	lyricsScanProgress.Lock()
	lyricsScanProgress.ownerLibraryID = ownerLibraryID
	lyricsScanProgress.total = 0
	lyricsScanProgress.processed = 0
	lyricsScanProgress.ready = false
	lyricsScanProgress.Unlock()
}

func resetLyricsScanProgress() {
	lyricsScanProgress.Lock()
	lyricsScanProgress.ownerLibraryID = 0
	lyricsScanProgress.total = 0
	lyricsScanProgress.processed = 0
	lyricsScanProgress.ready = false
	lyricsScanProgress.Unlock()
}

func setLyricsScanTotal(libraryID, total int) {
	lyricsScanProgress.Lock()
	if libraryID > 0 && lyricsScanProgress.ownerLibraryID == libraryID {
		lyricsScanProgress.total = total
		lyricsScanProgress.processed = 0
		lyricsScanProgress.ready = true
	}
	lyricsScanProgress.Unlock()
}

func advanceLyricsScanProgress(libraryID int) {
	lyricsScanProgress.Lock()
	if libraryID > 0 && lyricsScanProgress.ownerLibraryID == libraryID && lyricsScanProgress.ready && lyricsScanProgress.processed < lyricsScanProgress.total {
		lyricsScanProgress.processed++
	}
	lyricsScanProgress.Unlock()
}

// StartScan starts a private-library scan in the background. The package-wide
// lock is shared with scheduled scans and every other manual scan.
func (f *Fetcher) StartScan(ctx context.Context, targets []model.ScanTarget) bool {
	if !lyricsScanRunning.CompareAndSwap(false, true) {
		return false
	}
	ownerLibraryID := 0
	if len(targets) == 1 {
		ownerLibraryID = targets[0].LibraryID
	}
	beginLyricsScanProgress(ownerLibraryID)
	go func() {
		defer func() {
			resetLyricsScanProgress()
			lyricsScanRunning.Store(false)
		}()
		scanCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 24*time.Hour)
		defer cancel()
		if err := f.scanTargets(scanCtx, targets); err != nil {
			log.Error(scanCtx, "Lyrics scan failed", err)
		}
	}()
	return true
}

// ScanAll is used by the scheduler. It shares the same lock as manual scans.
func (f *Fetcher) ScanAll(ctx context.Context) error {
	if !lyricsScanRunning.CompareAndSwap(false, true) {
		return ErrScanBusy
	}
	beginLyricsScanProgress(0)
	defer func() {
		resetLyricsScanProgress()
		lyricsScanRunning.Store(false)
	}()

	libraries, err := f.ds.Library().GetAll(ctx)
	if err != nil {
		return fmt.Errorf("listing music libraries: %w", err)
	}
	for _, library := range libraries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := f.scanLibrary(ctx, library, ""); err != nil {
			log.Error(ctx, "Lyrics scan failed for library", "libraryId", library.ID, err)
		}
	}
	return nil
}

func (f *Fetcher) scanTargets(ctx context.Context, targets []model.ScanTarget) error {
	for _, target := range targets {
		if err := ctx.Err(); err != nil {
			return err
		}
		library, err := f.ds.Library().Get(ctx, target.LibraryID)
		if err != nil {
			log.Error(ctx, "Cannot load library for lyrics scan", "libraryId", target.LibraryID, err)
			continue
		}
		if err := f.scanLibrary(ctx, *library, target.FolderPath); err != nil {
			log.Error(ctx, "Lyrics scan failed for library", "libraryId", target.LibraryID, err)
		}
	}
	return nil
}

func (f *Fetcher) scanLibrary(ctx context.Context, library model.Library, folderPath string) error {
	store, err := storage.For(library.Path)
	if err != nil {
		return fmt.Errorf("opening library storage: %w", err)
	}
	fsys, err := store.FS()
	if err != nil {
		return fmt.Errorf("opening library filesystem: %w", err)
	}
	root, err := os.OpenRoot(library.Path)
	if err != nil {
		return fmt.Errorf("opening writable library root: %w", err)
	}
	defer root.Close()

	filters := squirrel.And{
		squirrel.Eq{"media_file.library_id": library.ID},
		squirrel.Eq{"media_file.missing": false},
	}
	if folderPath != "" {
		clean := path.Clean(folderPath)
		if clean == "." || path.IsAbs(clean) || !filepath.IsLocal(clean) || strings.ContainsAny(clean, "\\:") {
			return errors.New("invalid lyrics scan folder")
		}
		prefix := strings.TrimSuffix(clean, "/") + "/"
		filters = append(filters, squirrel.Expr("substr(media_file.path,1,?) = ?", utf8.RuneCountInString(prefix), prefix))
	}

	files, err := f.ds.MediaFile().GetAll(ctx, model.QueryOptions{
		Filters: filters,
		Sort:    "path",
		Order:   "asc",
	})
	if err != nil {
		return fmt.Errorf("listing indexed music files: %w", err)
	}

	setLyricsScanTotal(library.ID, len(files))
	for i := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		mediaFile := &files[i]
		func() {
			defer advanceLyricsScanProgress(library.ID)
			if mediaFile.LibraryID != library.ID || mediaFile.Missing || mediaFile.Path == "" {
				return
			}
			if mediaFile.HasEmbeddedLyrics() {
				return
			}
			hasSidecar, err := hasLyricsSidecar(fsys, mediaFile.Path)
			if err != nil {
				log.Warn(ctx, "Cannot inspect existing lyrics sidecars", "path", mediaFile.Path, err)
				return
			}
			if hasSidecar {
				return
			}

			track, err := f.findAutomaticLyrics(ctx, mediaFile)
			if err != nil {
				if !errors.Is(err, ErrNoMatchingLyrics) {
					log.Debug(ctx, "Lyrics lookup failed", "path", mediaFile.Path, err)
				}
				return
			}
			format, contents := preferredLyrics(track)
			if len(contents) == 0 {
				return
			}
			hasSidecar, err = hasLyricsSidecar(fsys, mediaFile.Path)
			if err != nil || hasSidecar {
				return
			}
			if err := writeAutomaticLyrics(root, mediaFile.Path, format, contents); err != nil {
				if !errors.Is(err, os.ErrExist) {
					log.Warn(ctx, "Cannot save lyrics sidecar", "path", mediaFile.Path, err)
				}
			}
		}()
	}
	return nil
}

func hasLyricsSidecar(fsys fs.FS, audioPath string) (bool, error) {
	base, err := lyricsSidecarBase(audioPath)
	if err != nil {
		return true, err
	}
	for _, ext := range lyricsSidecarExts {
		file, err := fsys.Open(base + ext)
		if err == nil {
			_ = file.Close()
			return true, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return true, err
		}
	}
	return false, nil
}

func lyricsSidecarBase(audioPath string) (string, error) {
	audioPath = filepath.ToSlash(audioPath)
	if audioPath == "" || path.IsAbs(audioPath) || !filepath.IsLocal(audioPath) || strings.Contains(audioPath, "\\") || !fsValidPath(audioPath) {
		return "", errors.New("invalid indexed music path")
	}
	base := strings.TrimSuffix(audioPath, path.Ext(audioPath))
	if base == "" || base == "." {
		return "", errors.New("invalid indexed music basename")
	}
	return base, nil
}

func fsValidPath(value string) bool {
	return value != "." && path.Clean(value) == value && !strings.HasPrefix(value, "../") && value != ".."
}

func (f *Fetcher) findAutomaticLyrics(ctx context.Context, mediaFile *model.MediaFile) (LRCLIBTrack, error) {
	duration := float64(mediaFile.Duration)
	if math.IsNaN(duration) || math.IsInf(duration, 0) || duration <= 0 {
		return LRCLIBTrack{}, ErrNoMatchingLyrics
	}
	lookups := buildLyricsLookups(mediaFile)
	if len(lookups) == 0 {
		return LRCLIBTrack{}, ErrNoMatchingLyrics
	}

	var fallback *LRCLIBTrack
	for _, lookup := range lookups {
		if duration >= 1 && duration <= 3600 {
			track, err := f.getLyrics(ctx, lookup, duration)
			if err == nil {
				if matchesRecording(track, lookup, duration) {
					return track, nil
				}
				fallback = betterDurationFallback(fallback, &track, lookup, duration)
			}
			if err != nil && !errors.Is(err, ErrNoMatchingLyrics) && !isClientLookupError(err) {
				return LRCLIBTrack{}, err
			}
		}

		candidates, err := f.searchLookup(ctx, lookup, false)
		if err != nil {
			return LRCLIBTrack{}, err
		}
		if best := bestLyricsCandidate(candidates, lookup, duration); best != nil {
			return *best, nil
		}
		fallback = bestDurationFallback(candidates, lookup, duration, fallback)

		candidates, err = f.searchLookup(ctx, lookup, true)
		if err != nil {
			return LRCLIBTrack{}, err
		}
		if best := bestLyricsCandidate(candidates, lookup, duration); best != nil {
			return *best, nil
		}
		fallback = bestDurationFallback(candidates, lookup, duration, fallback)
	}
	if fallback != nil {
		// A duration fallback favors plain lyrics, which remain readable even if
		// the chosen recording has a different runtime from the local audio file.
		if strings.TrimSpace(fallback.PlainLyrics) != "" {
			fallback.SyncedLyrics = ""
		}
		return *fallback, nil
	}
	return LRCLIBTrack{}, ErrNoMatchingLyrics
}

func betterDurationFallback(current, candidate *LRCLIBTrack, lookup LyricsLookup, duration float64) *LRCLIBTrack {
	if candidate == nil || candidate.ID <= 0 || candidate.Instrumental || !hasUsableLyrics(candidate) || candidate.Duration <= 0 ||
		!artistMatches(candidate.ArtistName, lookup.Artist) || !titleMatches(candidate.TrackName, lookup.Title, lookup.Artist) {
		return current
	}
	if current == nil {
		copy := *candidate
		return &copy
	}
	newDelta := math.Abs(candidate.Duration - duration)
	oldDelta := math.Abs(current.Duration - duration)
	if newDelta < oldDelta {
		copy := *candidate
		return &copy
	}
	if newDelta > oldDelta {
		return current
	}
	newPlain := strings.TrimSpace(candidate.PlainLyrics) != ""
	oldPlain := strings.TrimSpace(current.PlainLyrics) != ""
	if newPlain && !oldPlain {
		copy := *candidate
		return &copy
	}
	newAlbum := lookup.Album != "" && normalizeLyricsMetadata(candidate.AlbumName) == normalizeLyricsMetadata(lookup.Album)
	oldAlbum := lookup.Album != "" && normalizeLyricsMetadata(current.AlbumName) == normalizeLyricsMetadata(lookup.Album)
	if newPlain == oldPlain && newAlbum && !oldAlbum {
		copy := *candidate
		return &copy
	}
	return current
}

func bestDurationFallback(candidates []LRCLIBTrack, lookup LyricsLookup, duration float64, current *LRCLIBTrack) *LRCLIBTrack {
	for i := range candidates {
		current = betterDurationFallback(current, &candidates[i], lookup, duration)
	}
	return current
}

func isClientLookupError(err error) bool {
	var statusError *lyricsHTTPError
	return errors.As(err, &statusError) && (statusError.status == http.StatusBadRequest || statusError.status == http.StatusNotFound)
}

type lyricsHTTPError struct {
	status int
	body   string
}

func (e *lyricsHTTPError) Error() string {
	if e.body == "" {
		return fmt.Sprintf("LRCLIB returned HTTP %d", e.status)
	}
	return fmt.Sprintf("LRCLIB returned HTTP %d: %s", e.status, e.body)
}

func (f *Fetcher) endpoint(name string) (*url.URL, error) {
	base, err := url.Parse(strings.TrimSpace(f.baseURL))
	if err != nil || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, errors.New("LYRICS_URL must be a valid HTTP(S) base URL")
	}
	base.Path = strings.TrimRight(base.Path, "/")
	switch {
	case strings.HasSuffix(base.Path, "/api/get"):
		base.Path = strings.TrimSuffix(base.Path, "/get")
		base.Path = path.Join(base.Path, name)
	case strings.HasSuffix(base.Path, "/api/search"):
		base.Path = strings.TrimSuffix(base.Path, "/search")
		base.Path = path.Join(base.Path, name)
	case strings.HasSuffix(base.Path, "/api"):
		base.Path = path.Join(base.Path, name)
	default:
		base.Path = path.Join(base.Path, "api", name)
	}
	if !strings.HasPrefix(base.Path, "/") {
		base.Path = "/" + base.Path
	}
	base.RawPath = ""
	return base, nil
}

func (f *Fetcher) getLyrics(ctx context.Context, lookup LyricsLookup, duration float64) (LRCLIBTrack, error) {
	endpoint, err := f.endpoint("get")
	if err != nil {
		return LRCLIBTrack{}, err
	}
	query := endpoint.Query()
	query.Set("artist_name", lookup.Artist)
	query.Set("track_name", lookup.Title)
	query.Set("album_name", lookup.Album)
	query.Set("duration", strconv.Itoa(int(math.Round(duration))))
	endpoint.RawQuery = query.Encode()

	var track LRCLIBTrack
	if err := f.requestJSON(ctx, endpoint, &track); err != nil {
		if isClientLookupError(err) {
			return LRCLIBTrack{}, ErrNoMatchingLyrics
		}
		return LRCLIBTrack{}, err
	}
	if !hasUsableLyrics(&track) || track.Instrumental {
		return LRCLIBTrack{}, ErrNoMatchingLyrics
	}
	return track, nil
}

func (f *Fetcher) searchLookup(ctx context.Context, lookup LyricsLookup, broad bool) ([]LRCLIBTrack, error) {
	endpoint, err := f.endpoint("search")
	if err != nil {
		return nil, err
	}
	query := endpoint.Query()
	if broad {
		query.Set("q", truncateLyricsQuery(lookup.Title))
	} else {
		query.Set("track_name", truncateLyricsQuery(lookup.Title))
		query.Set("artist_name", truncateLyricsQuery(lookup.Artist))
	}
	endpoint.RawQuery = query.Encode()

	var candidates []LRCLIBTrack
	if err := f.requestJSON(ctx, endpoint, &candidates); err != nil {
		if isClientLookupError(err) {
			return nil, nil
		}
		return nil, err
	}
	return candidates, nil
}

// SearchLyrics searches LRCLIB directly from a free-form query. It is used by
// manual search, so it deliberately does not require a library song or scan lock.
func (f *Fetcher) SearchLyrics(ctx context.Context, queryText string) ([]LRCLIBTrack, error) {
	queryText = strings.TrimSpace(queryText)
	if len([]rune(queryText)) < 2 {
		return nil, nil
	}
	if len([]rune(queryText)) > 160 {
		queryText = string([]rune(queryText)[:160])
	}

	endpoint, err := f.endpoint("search")
	if err != nil {
		return nil, err
	}
	query := endpoint.Query()
	query.Set("q", queryText)
	endpoint.RawQuery = query.Encode()

	var candidates []LRCLIBTrack
	if err := f.requestJSON(ctx, endpoint, &candidates); err != nil {
		if isClientLookupError(err) {
			return nil, nil
		}
		return nil, err
	}

	seen := make(map[int64]struct{}, len(candidates))
	results := make([]LRCLIBTrack, 0, min(len(candidates), 50))
	for _, candidate := range candidates {
		if candidate.ID <= 0 || candidate.Instrumental || !hasUsableLyrics(&candidate) {
			continue
		}
		if _, exists := seen[candidate.ID]; exists {
			continue
		}
		seen[candidate.ID] = struct{}{}
		results = append(results, candidate)
		if len(results) == 50 {
			break
		}
	}
	return results, nil
}

func (f *Fetcher) requestJSON(ctx context.Context, endpoint *url.URL, destination any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := f.client.Do(req)
	if err != nil {
		return fmt.Errorf("requesting LRCLIB: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return &lyricsHTTPError{status: resp.StatusCode, body: strings.TrimSpace(string(body))}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, lyricsMaxResponse+1))
	if err != nil {
		return fmt.Errorf("reading LRCLIB response: %w", err)
	}
	if len(body) > lyricsMaxResponse {
		return errors.New("LRCLIB response is too large")
	}
	if err := json.Unmarshal(body, destination); err != nil {
		return fmt.Errorf("decoding LRCLIB response: %w", err)
	}
	return nil
}

// SearchForSong performs a manual lookup without taking the background scan lock.
func (f *Fetcher) SearchForSong(ctx context.Context, mediaFile *model.MediaFile) ([]LRCLIBTrack, error) {
	lookups := buildLyricsLookups(mediaFile)
	if len(lookups) == 0 {
		return nil, nil
	}
	seen := make(map[int64]struct{})
	results := make([]LRCLIBTrack, 0, 20)
	for _, lookup := range lookups {
		for _, broad := range []bool{false, true} {
			candidates, err := f.searchLookup(ctx, lookup, broad)
			if err != nil {
				return nil, err
			}
			for _, candidate := range candidates {
				if candidate.ID == 0 || candidate.Instrumental || !hasUsableLyrics(&candidate) {
					continue
				}
				if _, exists := seen[candidate.ID]; exists {
					continue
				}
				seen[candidate.ID] = struct{}{}
				results = append(results, candidate)
			}
		}
	}

	duration := float64(mediaFile.Duration)
	primary := lookups[0]
	sort.SliceStable(results, func(i, j int) bool {
		return lyricsCandidateScore(results[i], primary, duration) > lyricsCandidateScore(results[j], primary, duration)
	})
	if len(results) > 30 {
		results = results[:30]
	}
	return results, nil
}

func lyricsCandidateScore(track LRCLIBTrack, lookup LyricsLookup, duration float64) int {
	score := 0
	if artistMatches(track.ArtistName, lookup.Artist) {
		score += 100
	}
	if titleMatches(track.TrackName, lookup.Title, lookup.Artist) {
		score += 100
	}
	if lookup.Album != "" && normalizeLyricsMetadata(track.AlbumName) == normalizeLyricsMetadata(lookup.Album) {
		score += 25
	}
	if track.SyncedLyrics != "" {
		score += 15
	}
	if duration > 0 && track.Duration > 0 {
		delta := math.Abs(track.Duration - duration)
		if delta <= lyricsDurationSlack {
			score += 50
		} else {
			score -= int(math.Min(delta, 100))
		}
	}
	return score
}

// Associate re-resolves a song-specific result against LRCLIB before writing it.
func (f *Fetcher) Associate(ctx context.Context, mediaFile *model.MediaFile, resultID int64, format string) error {
	if resultID <= 0 {
		return errors.New("invalid lyrics result")
	}
	results, err := f.SearchForSong(ctx, mediaFile)
	if err != nil {
		return err
	}
	selected := findLyricsResult(results, resultID)
	if selected == nil {
		return ErrLyricsCandidate
	}
	return f.saveSelectedLyrics(ctx, mediaFile, selected, format)
}

// AssociateSearchResult saves a result selected from a free-form LRCLIB search.
// The media file comes from the authenticated user's library repository; the
// browser never supplies a destination path.
func (f *Fetcher) AssociateSearchResult(ctx context.Context, mediaFile *model.MediaFile, query string, resultID int64, format string) error {
	if resultID <= 0 {
		return errors.New("invalid lyrics result")
	}
	results, err := f.SearchLyrics(ctx, query)
	if err != nil {
		return err
	}
	selected := findLyricsResult(results, resultID)
	if selected == nil {
		return ErrLyricsCandidate
	}
	return f.saveSelectedLyrics(ctx, mediaFile, selected, format)
}

func findLyricsResult(results []LRCLIBTrack, resultID int64) *LRCLIBTrack {
	for i := range results {
		if results[i].ID == resultID {
			return &results[i]
		}
	}
	return nil
}

func (f *Fetcher) saveSelectedLyrics(ctx context.Context, mediaFile *model.MediaFile, selected *LRCLIBTrack, format string) error {
	if mediaFile == nil || mediaFile.LibraryID <= 0 || mediaFile.Path == "" {
		return errors.New("invalid library song")
	}
	var contents []byte
	var extension string
	switch format {
	case "synced":
		if strings.TrimSpace(selected.SyncedLyrics) == "" {
			return errors.New("selected result has no synchronized lyrics")
		}
		contents, extension = []byte(selected.SyncedLyrics), ".lrc"
	case "plain":
		if strings.TrimSpace(selected.PlainLyrics) == "" {
			return errors.New("selected result has no plain lyrics")
		}
		contents, extension = []byte(selected.PlainLyrics), ".txt"
	default:
		return errors.New("invalid lyrics format")
	}

	library, err := f.ds.Library().Get(ctx, mediaFile.LibraryID)
	if err != nil {
		return fmt.Errorf("loading song library: %w", err)
	}
	root, err := os.OpenRoot(library.Path)
	if err != nil {
		return fmt.Errorf("opening song library: %w", err)
	}
	defer root.Close()
	return writeManualLyrics(root, mediaFile.Path, extension, contents)
}

func preferredLyrics(track LRCLIBTrack) (string, []byte) {
	if strings.TrimSpace(track.SyncedLyrics) != "" {
		return "synced", []byte(track.SyncedLyrics)
	}
	if strings.TrimSpace(track.PlainLyrics) != "" {
		return "plain", []byte(track.PlainLyrics)
	}
	return "", nil
}

func writeAutomaticLyrics(root *os.Root, audioPath, format string, contents []byte) error {
	base, err := lyricsSidecarBase(audioPath)
	if err != nil {
		return err
	}
	extension := ".txt"
	if format == "synced" {
		extension = ".lrc"
	}
	file, err := root.OpenFile(base+extension, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0640)
	if err != nil {
		return err
	}
	if _, err := file.Write(contents); err != nil {
		_ = file.Close()
		_ = root.Remove(base + extension)
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = root.Remove(base + extension)
		return err
	}
	return file.Close()
}

func writeManualLyrics(root *os.Root, audioPath, extension string, contents []byte) error {
	base, err := lyricsSidecarBase(audioPath)
	if err != nil {
		return err
	}
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	temp := base + ".lyrics-" + hex.EncodeToString(random[:]) + ".tmp"
	file, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0640)
	if err != nil {
		return err
	}
	if _, err := file.Write(contents); err != nil {
		_ = file.Close()
		_ = root.Remove(temp)
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = root.Remove(temp)
		return err
	}
	if err := file.Close(); err != nil {
		_ = root.Remove(temp)
		return err
	}
	if err := root.Rename(temp, base+extension); err != nil {
		_ = root.Remove(temp)
		return err
	}
	for _, oldExtension := range lyricsSidecarExts {
		if oldExtension == extension {
			continue
		}
		if err := root.Remove(base + oldExtension); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Warn("Could not remove obsolete lyrics sidecar", "path", base+oldExtension, err)
		}
	}
	return nil
}

func buildLyricsLookups(mediaFile *model.MediaFile) []LyricsLookup {
	var lookups []LyricsLookup
	var versionLookups []LyricsLookup
	add := func(title, artist, album string) {
		title, artist, album = strings.TrimSpace(title), strings.TrimSpace(artist), strings.TrimSpace(album)
		if title == "" || artist == "" {
			return
		}
		target := &lookups
		if lyricsTitleHasVersion(title) {
			target = &versionLookups
		}
		for _, existing := range *target {
			if normalizeLyricsMetadata(existing.Title) == normalizeLyricsMetadata(title) && artistKey(existing.Artist) == artistKey(artist) {
				return
			}
		}
		*target = append(*target, LyricsLookup{Title: title, Artist: artist, Album: album})
	}

	artist := strings.TrimSpace(mediaFile.Artist)
	if artist == "" {
		artist = mediaFile.Participants.First(model.RoleArtist).Name
	}
	if artist == "" {
		artist = strings.TrimSpace(mediaFile.AlbumArtist)
	}
	title := strings.TrimSpace(mediaFile.Title)
	if title == "" {
		title = mediaFile.Tags.First(model.TagTitle)
	}
	add(title, artist, mediaFile.Album)

	tagTitle := mediaFile.Tags.First(model.TagTitle)
	tagArtist := mediaFile.Tags.First(model.TagTrackArtist)
	if tagArtist == "" {
		tagArtist = mediaFile.Tags.First(model.TagAlbumArtist)
	}
	if tagArtist == "" {
		tagArtist = artist
	}
	add(tagTitle, tagArtist, mediaFile.Tags.First(model.TagAlbum))

	for _, lookup := range filenameLyricsLookups(mediaFile.Path, mediaFile.Album) {
		add(lookup.Title, lookup.Artist, lookup.Album)
	}
	if len(versionLookups) > 0 {
		first := versionLookups[0]
		for _, lookup := range versionLookups[1:] {
			if !artistMatches(lookup.Artist, first.Artist) || !titleMatches(lookup.Title, first.Title, first.Artist) {
				return nil
			}
		}
		return versionLookups
	}
	return lookups
}

func filenameLyricsLookups(filePath, album string) []LyricsLookup {
	stem := strings.TrimSpace(strings.TrimSuffix(path.Base(filePath), path.Ext(filePath)))
	stem = strings.ReplaceAll(stem, " — ", " - ")
	stem = strings.ReplaceAll(stem, " – ", " - ")
	stem = strings.ReplaceAll(stem, " | ", " - ")
	stem = stripTrailingWebLabel(stem)
	parts := strings.Split(stem, " - ")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	for len(parts) > 2 && isLyricsNoise(strings.Trim(parts[len(parts)-1], "()[] ")) {
		parts = parts[:len(parts)-1]
	}
	if len(parts) < 2 {
		return nil
	}

	var result []LyricsLookup
	last := len(parts) - 1
	if len(parts) >= 3 && isFilenameVersion(parts[last]) {
		result = append(result, LyricsLookup{
			Artist: parts[last-2],
			Title:  strings.TrimSpace(parts[last-1] + " (" + parts[last] + ")"),
			Album:  album,
		})
	} else {
		result = append(result, LyricsLookup{Artist: parts[last-1], Title: parts[last], Album: album})
	}
	if len(parts) >= 3 && !slices.ContainsFunc(parts[2:], isFilenameVersion) {
		result = append(result, LyricsLookup{Artist: parts[0], Title: parts[1], Album: album})
		if len(parts) >= 3 && !isLyricsNoise(parts[last]) && !isFilenameVersion(parts[last]) {
			result = append(result, LyricsLookup{Artist: parts[0], Title: parts[last], Album: album})
		}
	}
	filtered := result[:0]
	for i := range result {
		result[i].Title = stripTrackNumber(result[i].Title)
		result[i].Artist = stripTrackNumber(result[i].Artist)
		if isNumericTrackNumber(result[i].Artist) || result[i].Title == "" {
			continue
		}
		filtered = append(filtered, result[i])
	}
	return filtered
}

func isNumericTrackNumber(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return true
	}
	for _, char := range value {
		if !unicode.IsDigit(char) {
			return false
		}
	}
	return true
}

func stripTrackNumber(value string) string {
	value = strings.TrimSpace(value)
	count := 0
	for count < len(value) && value[count] >= '0' && value[count] <= '9' {
		count++
	}
	if count == 0 || count > 3 || count >= len(value) {
		return value
	}
	rest := strings.TrimLeft(value[count:], " \t")
	if rest == "" {
		return value
	}
	if !strings.ContainsRune(".)-_", rune(rest[0])) {
		return value
	}
	after := strings.TrimLeft(rest[1:], " \t")
	if after == "" || len(after) == len(rest[1:]) {
		return value
	}
	return after
}

func normalizeLyricsMetadata(value string) string {
	value = stripTrackNumber(value)
	decomposed := norm.NFD.String(value)
	var b strings.Builder
	space := false
	for _, char := range decomposed {
		if unicode.Is(unicode.Mn, char) {
			continue
		}
		if unicode.IsLetter(char) || unicode.IsNumber(char) {
			if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(strings.ToLower(string(char)))
			space = false
		} else if b.Len() > 0 {
			space = true
		}
	}
	return strings.TrimSpace(b.String())
}

func artistKey(value string) string {
	normalized := normalizeLyricsMetadata(value)
	words := strings.Fields(normalized)
	if len(words) < 3 {
		return normalized
	}
	filtered := make([]string, 0, len(words))
	for i, word := range words {
		if i > 0 && i < len(words)-1 && (word == "e" || word == "and" || word == "y") {
			continue
		}
		filtered = append(filtered, word)
	}
	return strings.Join(filtered, " ")
}

func lyricsTitleHasVersion(value string) bool {
	if len(value) > 2 {
		closing := value[len(value)-1]
		opening := byte(0)
		if closing == ')' {
			opening = '('
		} else if closing == ']' {
			opening = '['
		}
		if opening != 0 {
			if index := strings.LastIndexByte(value, opening); index >= 0 && isVersionQualifier(value[index+1:len(value)-1]) {
				return true
			}
		}
	}
	for _, separator := range []string{" - ", " — ", " – ", " | "} {
		if index := strings.LastIndex(value, separator); index >= 0 && isVersionQualifier(value[index+len(separator):]) {
			return true
		}
	}
	words := strings.Fields(normalizeLyricsMetadata(value))
	if len(words) == 0 {
		return false
	}
	if len(words) == 1 {
		return isVersionQualifier(value)
	}
	last := words[len(words)-1]
	switch last {
	case "live", "vivo", "remix", "mix", "acoustic", "acustico", "instrumental", "karaoke", "demo", "cover", "extended", "slowed", "sped", "nightcore", "piano", "orchestral", "edit", "part", "version", "versao", "original", "remaster", "remastered", "mono", "stereo":
		return true
	case "take", "recording", "session", "performance", "concert":
		start := len(words) - 3
		if start < 0 {
			start = 0
		}
		return isVersionQualifier(strings.Join(words[start:], " "))
	default:
		return false
	}
}

func isVersionQualifier(value string) bool {
	for _, word := range strings.Fields(normalizeLyricsMetadata(value)) {
		switch word {
		case "live", "vivo", "remix", "mix", "acoustic", "acustico", "instrumental", "karaoke", "demo", "cover", "extended", "slowed", "sped", "nightcore", "piano", "orchestral", "edit", "part", "version", "versao", "original", "remaster", "remastered", "mono", "stereo":
			return true
		}
	}
	return false
}

func isFilenameVersion(value string) bool {
	normalized := normalizeLyricsMetadata(value)
	switch normalized {
	case "live", "vivo", "ao vivo", "remix", "instrumental", "acoustic", "acustico", "demo", "karaoke", "radio edit", "extended mix", "original mix", "slowed", "sped up", "nightcore", "cover", "piano version", "orchestral version", "mono", "stereo":
		return true
	}
	words := strings.Fields(normalized)
	if len(words) < 2 {
		return false
	}
	first, last := words[0], words[len(words)-1]
	if (first == "live" || first == "vivo") && (words[1] == "at" || words[1] == "from" || words[1] == "in") {
		return true
	}
	if (first == "acoustic" || first == "acustico" || first == "original" || first == "extended" || first == "radio" || first == "remaster" || first == "remastered") && (last == "version" || last == "mix" || last == "edit" || last == "recording" || last == "session" || last == "take") {
		return true
	}
	if (first == "remaster" || first == "remastered") && len(words) == 2 {
		_, err := strconv.Atoi(words[1])
		return err == nil
	}
	return false
}

func isLyricsNoise(value string) bool {
	switch normalizeLyricsMetadata(value) {
	case "traducao", "traducao legendado", "legendado", "legendada", "lyrics", "lyric", "lyric video", "lyrics video", "official lyrics", "official video", "official music video", "music video", "official mv", "mv", "official audio", "audio", "video oficial", "audio oficial", "clipe oficial", "video clip", "video letra", "letra", "hd", "4k", "visualizer", "visualiser":
		return true
	default:
		return false
	}
}

func stripTrailingAnnotation(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if len(value) < 3 {
		return "", false
	}
	closing := value[len(value)-1]
	opening := byte(0)
	if closing == ')' {
		opening = '('
	} else if closing == ']' {
		opening = '['
	} else {
		return "", false
	}
	index := strings.LastIndexByte(value, opening)
	if index < 0 {
		return "", false
	}
	annotation := value[index+1 : len(value)-1]
	if annotation == "" || isVersionQualifier(annotation) {
		return "", false
	}
	prefix := strings.TrimRight(value[:index], " \t-–—_|")
	return prefix, prefix != ""
}

func stripArtistPrefix(title, artist string) (string, bool) {
	for _, separator := range []string{" - ", " / ", " — ", " – ", " | "} {
		if prefix, rest, ok := strings.Cut(title, separator); ok && strings.TrimSpace(rest) != "" && artistKey(prefix) == artistKey(artist) {
			return strings.TrimSpace(rest), true
		}
	}
	return "", false
}

func stripTrailingWebLabel(value string) string {
	for _, separator := range []string{" - ", " — ", " – ", " | "} {
		if prefix, suffix, ok := strings.Cut(value, separator); ok && isLyricsNoise(suffix) {
			return strings.TrimSpace(prefix)
		}
		if index := strings.LastIndex(value, separator); index >= 0 {
			prefix, suffix := value[:index], value[index+len(separator):]
			if isLyricsNoise(suffix) && strings.TrimSpace(prefix) != "" {
				return strings.TrimSpace(prefix)
			}
		}
	}
	return value
}

func titleKeys(title, artist string) []string {
	variants := []string{strings.TrimSpace(title)}
	keys := make([]string, 0, 4)
	for i := 0; i < len(variants) && i < 12; i++ {
		raw := variants[i]
		key := normalizeLyricsMetadata(raw)
		if key != "" && !slices.Contains(keys, key) {
			keys = append(keys, key)
		}
		if next, ok := stripTrailingAnnotation(raw); ok && !slices.Contains(variants, next) {
			variants = append(variants, next)
		}
		if next, ok := stripArtistPrefix(raw, artist); ok && !slices.Contains(variants, next) {
			variants = append(variants, next)
		}
		if next := stripTrailingWebLabel(raw); next != raw && !slices.Contains(variants, next) {
			variants = append(variants, next)
		}
	}
	return keys
}

func artistMatches(actual, expected string) bool {
	return actual != "" && expected != "" && artistKey(actual) == artistKey(expected)
}

func titleMatches(actual, expected, artist string) bool {
	actualKeys := titleKeys(actual, artist)
	expectedKeys := titleKeys(expected, artist)
	for _, key := range expectedKeys {
		if slices.Contains(actualKeys, key) {
			return true
		}
	}
	return false
}

func matchesRecording(track LRCLIBTrack, lookup LyricsLookup, duration float64) bool {
	return track.ID > 0 && !track.Instrumental && hasUsableLyrics(&track) && artistMatches(track.ArtistName, lookup.Artist) && titleMatches(track.TrackName, lookup.Title, lookup.Artist) && track.Duration > 0 && math.Abs(track.Duration-duration) <= lyricsDurationSlack
}

func bestLyricsCandidate(candidates []LRCLIBTrack, lookup LyricsLookup, duration float64) *LRCLIBTrack {
	var best *LRCLIBTrack
	for i := range candidates {
		candidate := &candidates[i]
		if !matchesRecording(*candidate, lookup, duration) {
			continue
		}
		if best == nil || compareLyricsCandidates(*candidate, *best, lookup, duration) > 0 {
			best = candidate
		}
	}
	return best
}

func compareLyricsCandidates(a, b LRCLIBTrack, lookup LyricsLookup, duration float64) int {
	score := func(track LRCLIBTrack) int {
		value := 0
		if track.SyncedLyrics != "" {
			value += 100
		}
		if lookup.Album != "" && normalizeLyricsMetadata(track.AlbumName) == normalizeLyricsMetadata(lookup.Album) {
			value += 25
		}
		if track.Duration > 0 {
			value -= int(math.Round(math.Abs(track.Duration-duration) * 10))
		}
		return value
	}
	return score(a) - score(b)
}

func hasUsableLyrics(track *LRCLIBTrack) bool {
	return strings.TrimSpace(track.SyncedLyrics) != "" || strings.TrimSpace(track.PlainLyrics) != ""
}

func truncateLyricsQuery(value string) string {
	var b strings.Builder
	for _, char := range value {
		if b.Len()+utf8.RuneLen(char) > 120 {
			break
		}
		b.WriteRune(char)
	}
	return strings.TrimSpace(b.String())
}
