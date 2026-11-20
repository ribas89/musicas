package nativeapi

// Musica's private file manager. All filesystem operations are confined to
// the authenticated user's own Navidrome library using os.Root. Never resolve
// the filesystem root from a query parameter or trust a library ID supplied
// by the browser.

import (
	"archive/zip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Masterminds/squirrel"
	"github.com/go-chi/chi/v5"
	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/model/request"
	"github.com/navidrome/navidrome/server"
	"github.com/navidrome/navidrome/utils/str"
	"unicode/utf8"
)

const (
	musicaMaxRequest    = 2 << 30 // 2 GiB HTTP request limit
	musicaMaxExtracted  = 4 << 30 // 4 GiB extracted ZIP limit
	musicaMaxZipEntries = 10000
	musicaMaxFiles      = 500
)

var musicaAudioExtensions = []string{
	".mp3", ".flac", ".m4a", ".mp4", ".aac", ".ogg", ".opus", ".wav", ".aiff", ".aif", ".alac", ".wma", ".ape", ".wv", ".dsf", ".dff", ".mka",
}
var musicaCoverExtensions = []string{".jpg", ".jpeg", ".png", ".webp"}

func musicaAllowed(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return slices.Contains(musicaAudioExtensions, ext) || slices.Contains(musicaCoverExtensions, ext)
}

func musicaCleanRelative(value string, allowRoot bool) (string, error) {
	if len(value) > 1024 || strings.Count(value, "/") > 25 || strings.ContainsAny(value, "\\\x00") || strings.HasPrefix(value, "/") || strings.Contains(value, ":") {
		return "", errors.New("invalid path")
	}
	value = strings.TrimSuffix(value, "/")
	cleaned := path.Clean(value)
	if cleaned == "." && allowRoot {
		return ".", nil
	}
	if cleaned == "musicasyt.json" || cleaned == "." || !filepath.IsLocal(cleaned) || cleaned != value || strings.HasPrefix(cleaned, ".musica-") || strings.Contains(cleaned, "/.musica-") {
		return "", errors.New("invalid path")
	}
	return cleaned, nil
}

func musicaSafeName(value string) error {
	if value == "musicasyt.json" || value == "" || value == "." || value == ".." || strings.ContainsAny(value, "/\\\x00") || strings.HasPrefix(value, ".musica-") || len(value) > 240 {
		return errors.New("invalid filename")
	}
	return nil
}

// musicaUserRoot grants file access to *only* the native private library
// assigned to the authenticated user. Shared/public and other users' libraries
// can never be selected, including by administrator users.
func (api *Router) musicaUserRoot(r *http.Request) (*os.Root, int, error) {
	user, ok := request.UserFrom(r.Context())
	if !ok || user.UserName == "" || musicaSafeName(user.UserName) != nil {
		return nil, 0, errors.New("no private user library")
	}
	usersRoot := filepath.Clean(os.Getenv("MUSIC_USERS_ROOT"))
	if !filepath.IsAbs(usersRoot) {
		return nil, 0, errors.New("invalid user library configuration")
	}
	expected := filepath.Join(usersRoot, user.UserName)
	libs, err := api.ds.User().GetUserLibraries(r.Context(), user.ID)
	if err != nil {
		return nil, 0, err
	}
	for _, lib := range libs {
		if lib.Name == "OIDC: "+user.UserName && filepath.Clean(lib.Path) == expected {
			root, err := os.OpenRoot(lib.Path)
			return root, lib.ID, err
		}
	}
	return nil, 0, errors.New("private library not found")
}

func musicaJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func musicaError(w http.ResponseWriter, status int, message string) {
	musicaJSON(w, status, map[string]string{"error": message})
}

func (api *Router) addMusicaFilesRoute(r chi.Router) {
	r.Route("/files", func(r chi.Router) {
		r.Get("/", api.musicaListFiles)
		r.Post("/folder", api.musicaCreateFolder)
		r.Post("/upload", api.musicaUpload)
		r.Post("/download-session", api.musicaDownloadSession)
		r.Get("/download", api.musicaDownloadFolder)
		r.Delete("/", api.musicaDelete)
		r.Post("/scan", api.musicaScan)
		r.Get("/scan", api.musicaScanStatus)
	})
}

type musicaFileEntry struct {
	Name string `json:"name"`
	Dir  bool   `json:"dir"`
	Size int64  `json:"size"`
}

func (api *Router) musicaListFiles(w http.ResponseWriter, r *http.Request) {
	root, _, err := api.musicaUserRoot(r)
	if err != nil {
		musicaError(w, 403, "Private library unavailable")
		return
	}
	defer root.Close()
	rel, err := musicaCleanRelative(r.URL.Query().Get("path"), true)
	if err != nil {
		musicaError(w, 400, err.Error())
		return
	}
	dir, err := root.Open(rel)
	if err != nil {
		musicaError(w, 404, "Folder not found")
		return
	}
	defer dir.Close()
	info, err := dir.Stat()
	if err != nil || !info.IsDir() {
		musicaError(w, 400, "Not a folder")
		return
	}
	entries, err := dir.ReadDir(-1)
	if err != nil {
		musicaError(w, 500, "Cannot list folder")
		return
	}
	result := make([]musicaFileEntry, 0, len(entries))
	for _, e := range entries {
		if (rel == "." && e.Name() == "musicasyt.json") || strings.HasPrefix(e.Name(), ".musica-") || e.Type()&os.ModeSymlink != 0 {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			continue
		}
		result = append(result, musicaFileEntry{Name: e.Name(), Dir: info.IsDir(), Size: info.Size()})
	}
	musicaJSON(w, 200, map[string]any{"path": rel, "entries": result})
}

func (api *Router) musicaCreateFolder(w http.ResponseWriter, r *http.Request) {
	root, _, err := api.musicaUserRoot(r)
	if err != nil {
		musicaError(w, 403, "Private library unavailable")
		return
	}
	defer root.Close()
	var body struct {
		Path string `json:"path"`
		Name string `json:"name"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&body) != nil || musicaSafeName(body.Name) != nil {
		musicaError(w, 400, "Invalid folder name")
		return
	}
	rel, err := musicaCleanRelative(body.Path, true)
	if err != nil {
		musicaError(w, 400, err.Error())
		return
	}
	api.filesMu.Lock()
	defer api.filesMu.Unlock()
	if err := root.Mkdir(path.Join(rel, body.Name), 0750); err != nil {
		musicaError(w, 409, "Cannot create folder; name may already exist")
		return
	}
	musicaJSON(w, 201, map[string]string{"created": body.Name})
}

func (api *Router) musicaDelete(w http.ResponseWriter, r *http.Request) {
	root, _, err := api.musicaUserRoot(r)
	if err != nil {
		musicaError(w, 403, "Private library unavailable")
		return
	}
	defer root.Close()
	rel, err := musicaCleanRelative(r.URL.Query().Get("path"), false)
	if err != nil {
		musicaError(w, 400, "Cannot delete library root or invalid path")
		return
	}
	api.filesMu.Lock()
	defer api.filesMu.Unlock()
	if err := root.RemoveAll(rel); err != nil {
		musicaError(w, 500, "Cannot delete item")
		return
	}
	musicaJSON(w, 200, map[string]bool{"deleted": true})
}

func musicaRandomSuffix() string {
	var b [10]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func musicaZipExtract(root *os.Root, target string, archive multipart.File, size int64) (string, error) {
	zr, err := zip.NewReader(archive, size)
	if err != nil {
		return "", errors.New("invalid ZIP archive")
	}
	if len(zr.File) > musicaMaxZipEntries {
		return "", errors.New("too many ZIP entries")
	}
	var name string
	// The ZIP becomes its own first-level folder (a native playlist).
	// For nested uploads, it becomes a folder inside the current selection.
	name = target
	staging := path.Join(path.Dir(target), ".musica-"+musicaRandomSuffix())
	if err := root.Mkdir(staging, 0750); err != nil {
		return "", err
	}
	defer root.RemoveAll(staging)
	var total int64
	for _, entry := range zr.File {
		raw := strings.TrimSuffix(strings.ReplaceAll(entry.Name, "\\", "/"), "/")
		sub, err := musicaCleanRelative(raw, false)
		if err != nil {
			return "", errors.New("ZIP contains an unsafe path")
		}
		if entry.Mode()&os.ModeSymlink != 0 || (!entry.FileInfo().IsDir() && !entry.Mode().IsRegular()) {
			return "", errors.New("ZIP contains a link or unsupported file type")
		}
		dest := path.Join(staging, sub)
		if entry.FileInfo().IsDir() {
			if err := root.MkdirAll(dest, 0750); err != nil {
				return "", err
			}
			continue
		}
		if !musicaAllowed(sub) {
			continue
		}
		if entry.UncompressedSize64 > uint64(musicaMaxExtracted) || total+int64(entry.UncompressedSize64) > musicaMaxExtracted {
			return "", errors.New("ZIP exceeds extraction limit")
		}
		if err := root.MkdirAll(path.Dir(dest), 0750); err != nil {
			return "", err
		}
		in, err := entry.Open()
		if err != nil {
			return "", err
		}
		out, err := root.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0640)
		if err != nil {
			in.Close()
			return "", err
		}
		n, copyErr := io.Copy(out, io.LimitReader(in, musicaMaxExtracted-total+1))
		closeErr := out.Close()
		in.Close()
		total += n
		if copyErr != nil || closeErr != nil || total > musicaMaxExtracted {
			return "", errors.New("ZIP extraction failed or exceeded limit")
		}
	}
	if total == 0 {
		return "", errors.New("ZIP contains no supported music files")
	}
	if _, err := root.Lstat(name); err == nil {
		return "", errors.New("folder already exists")
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := root.Rename(staging, name); err != nil {
		return "", err
	}
	return path.Base(name), nil
}

func (api *Router) musicaUpload(w http.ResponseWriter, r *http.Request) {
	root, _, err := api.musicaUserRoot(r)
	if err != nil {
		musicaError(w, 403, "Private library unavailable")
		return
	}
	defer root.Close()
	r.Body = http.MaxBytesReader(w, r.Body, musicaMaxRequest)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		musicaError(w, 413, "Upload exceeds size limit or invalid multipart data")
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	rel, err := musicaCleanRelative(r.FormValue("path"), true)
	if err != nil {
		musicaError(w, 400, err.Error())
		return
	}
	folder, err := root.Open(rel)
	if err != nil {
		musicaError(w, 404, "Target folder not found")
		return
	}
	folderInfo, err := folder.Stat()
	folder.Close()
	if err != nil || !folderInfo.IsDir() {
		musicaError(w, 400, "Target is not a folder")
		return
	}
	headers := r.MultipartForm.File["files"]
	if len(headers) > musicaMaxFiles {
		musicaError(w, 400, "Maximum 500 files per upload")
		return
	}
	// Relative paths are sent separately from file names because browsers strip
	// directory information from multipart filenames. Validate the entire batch
	// BEFORE creating anything so a malformed directory drop cannot partially
	// modify a user's library.
	relativePaths := r.MultipartForm.Value["relativePaths"]
	if len(relativePaths) != 0 && len(relativePaths) != len(headers) {
		musicaError(w, 400, "Invalid folder upload: file path count does not match")
		return
	}
	directories := r.MultipartForm.Value["directories"]
	if len(directories) == 0 && len(headers) == 0 {
		musicaError(w, 400, "No files or directories selected")
		return
	}
	if len(directories) > 2000 || len(directories)+len(headers) > 2500 {
		musicaError(w, 400, "Too many folders or files")
		return
	}
	for _, dir := range directories {
		if _, err := musicaCleanRelative(dir, false); err != nil {
			musicaError(w, 400, "Unsafe uploaded folder path: "+err.Error())
			return
		}
	}
	dests := make([]string, len(headers))
	for i, header := range headers {
		if musicaSafeName(header.Filename) != nil {
			musicaError(w, 400, "Unsafe filename: "+header.Filename)
			return
		}
		fileRel := header.Filename
		if len(relativePaths) > 0 {
			fileRel = relativePaths[i]
		}
		safeRel, err := musicaCleanRelative(fileRel, false)
		if err != nil || path.Base(safeRel) != header.Filename {
			musicaError(w, 400, "Invalid uploaded file path: "+fileRel)
			return
		}
		dests[i] = path.Join(rel, safeRel)
	}
	api.filesMu.Lock()
	defer api.filesMu.Unlock()
	for _, dir := range directories {
		if err := root.MkdirAll(path.Join(rel, dir), 0750); err != nil {
			musicaError(w, 409, "Cannot create uploaded folder: "+dir)
			return
		}
	}
	uploaded := make([]string, 0, len(headers))
	for i, header := range headers {
		name := header.Filename
		dst := dests[i]
		if err := root.MkdirAll(path.Dir(dst), 0750); err != nil {
			musicaError(w, 409, "Cannot create destination folder")
			return
		}
		in, err := header.Open()
		if err != nil {
			musicaError(w, 400, "Cannot open uploaded file")
			return
		}
		if strings.EqualFold(filepath.Ext(name), ".zip") {
			base := strings.TrimSuffix(name, filepath.Ext(name))
			if musicaSafeName(base) != nil {
				in.Close()
				musicaError(w, 400, "Invalid ZIP folder name")
				return
			}
			created, err := musicaZipExtract(root, path.Join(path.Dir(dst), base), in, header.Size)
			in.Close()
			if err != nil {
				musicaError(w, 400, err.Error())
				return
			}
			uploaded = append(uploaded, created)
			continue
		}
		if !musicaAllowed(name) {
			in.Close()
			musicaError(w, 415, "Only music/audio files and cover images are allowed")
			return
		}
		out, err := root.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0640)
		if err != nil {
			in.Close()
			musicaError(w, 409, "File already exists or cannot be created")
			return
		}
		copied, copyErr := io.Copy(out, io.LimitReader(in, musicaMaxRequest+1))
		closeErr := out.Close()
		in.Close()
		if copyErr != nil || closeErr != nil || copied > musicaMaxRequest {
			_ = root.Remove(dst)
			musicaError(w, 500, "Upload failed")
			return
		}
		uploaded = append(uploaded, name)
	}
	musicaJSON(w, 201, map[string]any{"uploaded": uploaded, "folders": len(directories)})
}

// The UI already authenticates with a Navidrome JWT in X-ND-Authorization.
// Give the browser a short-lived, HttpOnly cookie scoped to this one download
// URL so ZIP downloads can stream directly to disk without loading the whole
// archive into browser RAM or putting credentials into the URL.
func (api *Router) musicaDownloadSession(w http.ResponseWriter, r *http.Request) {
	root, _, err := api.musicaUserRoot(r)
	if err != nil {
		musicaError(w, 403, "Private library unavailable")
		return
	}
	root.Close()
	tokenHeader := r.Header.Get("X-ND-Authorization")
	if !strings.HasPrefix(tokenHeader, "Bearer ") || len(tokenHeader) <= len("Bearer ") {
		musicaError(w, 401, "No authenticated token")
		return
	}
	// Verify middleware has already matched the token's username against this
	// authenticated user; don't mint a download session from an unrelated JWT.
	user, ok := request.UserFrom(r.Context())
	if !ok || !strings.EqualFold(server.UsernameFromToken(r), user.UserName) {
		musicaError(w, 401, "Invalid authenticated token")
		return
	}
	cookiePath := path.Join(conf.Server().BasePath, "api/files/download")
	if !strings.HasPrefix(cookiePath, "/") {
		cookiePath = "/" + cookiePath
	}
	http.SetCookie(w, &http.Cookie{
		Name: "jwt", Value: strings.TrimPrefix(tokenHeader, "Bearer "),
		Path: cookiePath, MaxAge: 90, HttpOnly: true, Secure: true,
		SameSite: http.SameSiteStrictMode,
	})
	musicaJSON(w, 200, map[string]bool{"ready": true})
}

func (api *Router) musicaDownloadFolder(w http.ResponseWriter, r *http.Request) {
	root, libraryID, err := api.musicaUserRoot(r)
	if err != nil {
		musicaError(w, 403, "Private library unavailable")
		return
	}
	defer root.Close()
	rel, err := musicaCleanRelative(r.URL.Query().Get("path"), false)
	if err != nil {
		musicaError(w, 400, "Choose a folder to download")
		return
	}
	dir, err := root.Open(rel)
	if err != nil {
		musicaError(w, 404, "Folder not found")
		return
	}
	info, statErr := dir.Stat()
	dir.Close()
	if statErr != nil || !info.IsDir() {
		musicaError(w, 400, "Not a folder")
		return
	}
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "raw"
	}
	bitrate := 0
	if format != "raw" {
		_, err = api.ds.Transcoding().FindByFormat(r.Context(), format)
		if err != nil {
			musicaError(w, 400, "Unknown transcoding format")
			return
		}
		bitrate, err = strconv.Atoi(r.URL.Query().Get("bitrate"))
		if err != nil || bitrate < 32 || bitrate > 320 {
			musicaError(w, 400, "Invalid bitrate")
			return
		}
	}
	if !conf.Server().EnableDownloads {
		musicaError(w, 403, "Downloads are disabled")
		return
	}
	// Don't serve HTTP 200 with an empty ZIP when the folder is new and
	// no scan has indexed its music yet.
	prefix := rel + "/"
	found, err := api.ds.MediaFile().GetAll(r.Context(), model.QueryOptions{
		Filters: squirrel.And{
			squirrel.Eq{"media_file.library_id": libraryID, "media_file.missing": false},
			squirrel.Expr("substr(media_file.path,1,?) = ?", utf8.RuneCountInString(prefix), prefix),
		}, Max: 1,
	})
	if err != nil {
		musicaError(w, 500, "Cannot check scanned songs")
		return
	}
	if len(found) == 0 {
		musicaError(w, 409, "No scanned songs found in this folder. Run Scan first.")
		return
	}
	// Reuse Navidrome's own streamer and transcoder through the core archiver.
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", str.ContentDispositionAttachment(path.Base(rel)+".zip"))
	w.Header().Set("Cache-Control", "no-store")
	if err := api.archiver.ZipFolder(r.Context(), libraryID, rel, format, bitrate, w); err != nil {
		// Once streaming starts it is too late to send a different status.
		// Log the underlying failure; premature EOF signals an interrupted ZIP.
		log.Error(r.Context(), "Musica folder download failed", "folder", rel, err)
	}
}

func (api *Router) musicaScan(w http.ResponseWriter, r *http.Request) {
	root, libID, err := api.musicaUserRoot(r)
	if err != nil {
		musicaError(w, 403, "Private library unavailable")
		return
	}
	root.Close()
	current, statusErr := api.scanner.Status(r.Context())
	if statusErr != nil {
		musicaError(w, 500, "Cannot check scanner status")
		return
	}
	if current.Scanning {
		musicaError(w, 409, "Navidrome is already scanning")
		return
	}
	if !api.filesScanning.CompareAndSwap(false, true) {
		musicaError(w, 409, "A user scan is already requested")
		return
	}
	// Run asynchronously, but only for the authenticated user's library ID.
	// A global Navidrome scan may already be in progress, in which case the
	// scanner returns ErrAlreadyScanning and we report it in scan status.
	api.filesScanError.Store("")
	user, _ := request.UserFrom(r.Context())
	api.filesScanOwner.Store(user.ID)
	ctx := r.Context()
	go func() {
		defer api.filesScanning.Store(false)
		scanCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Minute)
		defer cancel()
		_, scanErr := api.scanner.ScanFolders(scanCtx, false, []model.ScanTarget{{LibraryID: libID}})
		if scanErr != nil {
			api.filesScanError.Store(scanErr.Error())
		}
	}()
	musicaJSON(w, 202, map[string]bool{"started": true})
}

func (api *Router) musicaScanStatus(w http.ResponseWriter, r *http.Request) {
	root, _, err := api.musicaUserRoot(r)
	if err != nil {
		musicaError(w, 403, "Private library unavailable")
		return
	}
	root.Close()
	user, _ := request.UserFrom(r.Context())
	owner, _ := api.filesScanOwner.Load().(string)
	if owner != user.ID {
		musicaJSON(w, 200, map[string]any{"scanning": false, "error": ""})
		return
	}
	musicaJSON(w, 200, map[string]any{"scanning": api.filesScanning.Load(), "error": api.filesScanError.Load()})
}
