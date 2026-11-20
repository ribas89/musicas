package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/model"
)

// validateMusicLayout requires public and users to be sibling directories.
// This prevents a recursive scan of the shared library from indexing another
// user's private files, even when an administrator misconfigures the .env.
func validateMusicLayout(publicRoot, usersRoot string) error {
	if publicRoot == "" || usersRoot == "" || !filepath.IsAbs(publicRoot) || !filepath.IsAbs(usersRoot) {
		return errors.New("ND_MUSICFOLDER and MUSIC_USERS_ROOT must both be absolute directories")
	}
	shared := filepath.Clean(publicRoot)
	users := filepath.Clean(usersRoot)
	if filepath.Base(shared) != "public" || filepath.Base(users) != "users" || filepath.Dir(shared) != filepath.Dir(users) {
		return fmt.Errorf("expected sibling folders such as ND_MUSICFOLDER=/music/public and MUSIC_USERS_ROOT=/music/users (got %q and %q)", shared, users)
	}
	if shared == users {
		return errors.New("public and user libraries cannot share a path")
	}
	return nil
}

func initializeMusicLayout() error {
	shared := conf.Server().MusicFolder
	users := strings.TrimSpace(os.Getenv("MUSIC_USERS_ROOT"))
	if err := validateMusicLayout(shared, users); err != nil {
		return err
	}
	for _, dir := range []struct {
		path string
		mode os.FileMode
	}{
		{shared, 0755},
		{users, 0750},
	} {
		if err := os.MkdirAll(dir.path, dir.mode); err != nil {
			return fmt.Errorf("creating %s: %w", dir.path, err)
		}
		info, err := os.Lstat(dir.path)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("unsafe music directory: %s", dir.path)
		}
	}
	return nil
}

// configureSharedLibrary names the built-in library "Public" and makes it
// a default grant for newly registered accounts. User-specific grants are
// additionally set explicitly in ensureUserLibrary.
func configureSharedLibrary(ctx context.Context, ds model.DataStore) error {
	lib, err := ds.Library().Get(ctx, model.DefaultLibraryID)
	if err != nil {
		return fmt.Errorf("loading default public library: %w", err)
	}
	if filepath.Clean(lib.Path) != filepath.Clean(conf.Server().MusicFolder) {
		return fmt.Errorf("default library path %q does not match public music folder %q", lib.Path, conf.Server().MusicFolder)
	}
	if lib.Name == "Public" && lib.DefaultNewUsers {
		return nil
	}
	lib.Name = "Public"
	lib.DefaultNewUsers = true
	if err := ds.Library().Put(ctx, lib); err != nil {
		return fmt.Errorf("configuring shared public library: %w", err)
	}
	return nil
}
