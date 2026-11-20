package scanner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/model/criteria"
	"github.com/navidrome/navidrome/model/request"
)

// An OIDC private library has one and only one location: an immediate child
// of MUSIC_USERS_ROOT. The public library (a sibling of users/) never matches.
func oidcManagedUser(lib model.Library) string {
	root := os.Getenv("MUSIC_USERS_ROOT")
	if !filepath.IsAbs(root) || filepath.Dir(filepath.Clean(lib.Path)) != filepath.Clean(root) {
		return ""
	}
	username := filepath.Base(lib.Path)
	if lib.Name != "OIDC: "+username {
		return ""
	}
	return username
}

// Store the source-folder identity in the playlist's Comment so the scanner
// never mistakes a user-created playlist for one it owns. This field is human
// readable, requires no new schema or sidecar files, and is unique within the
// corresponding owner's private library.
const musicaFolderMarker = "Musica managed folder: "

func folderPlaylistComment(libraryID int, folder string) string {
	return musicaFolderMarker + strconv.Itoa(libraryID) + "/" + folder
}

// nativeFolderRules are the same native smart-playlist criteria used by the
// Navidrome UI. The combination of library_id and a path prefix prevents
// matches from Public, another user's library, or sibling folders with a
// similar name (e.g. Rock vs Rock Classics).
func nativeFolderRules(libraryID int, folder string) *criteria.Criteria {
	return &criteria.Criteria{
		Expression: criteria.All{
			criteria.Is{"library_id": libraryID},
			criteria.StartsWith{"filepath": filepath.ToSlash(folder) + "/"},
		},
		Sort:         "filepath",
		Order:        "asc",
		RefreshDelay: time.Minute,
	}
}

// syncOIDCFolderPlaylists creates/updates/deletes true Navidrome smart
// playlists in the native repository. It does not create NSP/M3U files or
// modify audio metadata. The normal scanner must finish before this runs.
// Changes to tracks under a folder are automatically reflected by the native
// smart playlist evaluator on the next read after its short refresh delay.
func syncOIDCFolderPlaylists(ctx context.Context, ds model.DataStore, libs model.Libraries, failedLibs map[int]bool) error {
	admin, err := ds.User().FindFirstAdmin(ctx)
	if err != nil {
		return fmt.Errorf("finding administrator for native folder playlists: %w", err)
	}
	if admin == nil || admin.ID == "" {
		return fmt.Errorf("cannot synchronize native folder playlists without an administrator")
	}
	adminCtx := request.WithUser(ctx, *admin)
	allPlaylists, err := ds.Playlist().GetAll(adminCtx)
	if err != nil {
		return fmt.Errorf("listing native playlists: %w", err)
	}
	// Index once: scans stay linear in the number of playlists and folders
	// even for installations with many separate users.
	byManagedKey := make(map[string]*model.Playlist)
	duplicates := make(map[string]bool)
	for i := range allPlaylists {
		pl := &allPlaylists[i]
		if !strings.HasPrefix(pl.Comment, musicaFolderMarker) {
			continue
		}
		key := pl.OwnerID + "\x00" + pl.Comment
		if _, exists := byManagedKey[key]; exists {
			duplicates[key] = true
		} else {
			byManagedKey[key] = pl
		}
	}

	for _, lib := range libs {
		username := oidcManagedUser(lib)
		if username == "" || failedLibs[lib.ID] {
			continue
		}
		owner, err := ds.User().FindByUsername(adminCtx, username)
		if err != nil {
			return fmt.Errorf("looking up owner of private library %q: %w", lib.Path, err)
		}
		if owner == nil || owner.IsAdmin || !owner.HasLibraryAccess(lib.ID) {
			return fmt.Errorf("private library %q has no valid owner or access grant", lib.Path)
		}
		children, err := os.ReadDir(lib.Path)
		if err != nil {
			return fmt.Errorf("listing private music library %q: %w", lib.Path, err)
		}
		ownerCtx := request.WithUser(ctx, *owner)
		markerPrefix := musicaFolderMarker + strconv.Itoa(lib.ID) + "/"
		wanted := make(map[string]bool, len(children))
		for _, child := range children {
			if !child.IsDir() || child.Type()&os.ModeSymlink != 0 || strings.HasPrefix(child.Name(), ".") {
				continue
			}
			folder := child.Name()
			comment := folderPlaylistComment(lib.ID, folder)
			wanted[comment] = true
			rules := nativeFolderRules(lib.ID, folder)
			key := owner.ID + "\x00" + comment
			if duplicates[key] {
				return fmt.Errorf("duplicate Musica-managed playlist for library %d folder %q", lib.ID, folder)
			}
			existing := byManagedKey[key]
			if existing == nil {
				pl := model.Playlist{Name: folder, Comment: comment, OwnerID: owner.ID,
					Public: false, Rules: rules}
				if err := ds.Playlist().Put(ownerCtx, &pl); err != nil {
					return fmt.Errorf("creating native playlist for %s/%s: %w", username, folder, err)
				}
				log.Info(ctx, "Created native private folder playlist", "user", username, "folder", folder, "playlistId", pl.ID)
				continue
			}
			if existing.Name != folder || existing.Public || !reflect.DeepEqual(existing.Rules, rules) {
				existing.Name = folder
				existing.Public = false
				existing.Rules = rules
				existing.EvaluatedAt = nil
				if err := ds.Playlist().Put(ownerCtx, existing); err != nil {
					return fmt.Errorf("updating native playlist for %s/%s: %w", username, folder, err)
				}
			}
		}

		// Only delete playlists with our exact owner and marker. Deleting a
		// folder removes its generated playlist, never a user's own playlists.
		for _, pl := range allPlaylists {
			if pl.OwnerID != owner.ID || !strings.HasPrefix(pl.Comment, markerPrefix) || wanted[pl.Comment] {
				continue
			}
			if err := ds.Playlist().Delete(ownerCtx, pl.ID); err != nil {
				return fmt.Errorf("removing playlist for deleted folder %q: %w", pl.Name, err)
			}
			log.Info(ctx, "Removed native playlist for deleted folder", "user", username, "playlist", pl.Name)
		}
	}
	return nil
}
