package lyrics

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"time"

	"github.com/navidrome/navidrome/core/storage"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/utils/ioutils"
)

func fromEmbedded(ctx context.Context, mf *model.MediaFile) (model.LyricList, error) {
	if mf.Lyrics != "" {
		log.Debug(ctx, "Lyrics: embedded lyrics field present", "emptyArray", mf.Lyrics == "[]")
		return mf.StructuredLyrics()
	}

	log.Debug(ctx, "Lyrics: no embedded lyrics")

	return nil, nil
}

func fromExternalFile(ctx context.Context, mf *model.MediaFile, suffix string) (model.LyricList, error) {
	ext := path.Ext(mf.Path)
	sidecarRelPath := mf.Path[0:len(mf.Path)-len(ext)] + suffix
	ctx = log.NewContext(ctx, "file", sidecarRelPath)

	log.Debug(ctx, "Lyrics: checking sidecar file", "suffix", suffix)
	store, err := storage.For(mf.LibraryPath)
	if err != nil {
		log.Debug(ctx, "Lyrics: storage lookup failed", "error", err)
		return nil, fmt.Errorf("getting storage for library: %w", err)
	}
	fsys, err := store.FS()
	if err != nil {
		log.Debug(ctx, "Lyrics: opening library filesystem failed", "error", err)
		return nil, fmt.Errorf("opening library filesystem: %w", err)
	}

	f, err := fsys.Open(sidecarRelPath)
	if errors.Is(err, fs.ErrNotExist) {
		log.Debug(ctx, "Lyrics: sidecar file does not exist")
		return nil, nil
	} else if err != nil {
		log.Debug(ctx, "Lyrics: sidecar file could not be opened", "error", err)
		return nil, err
	}
	defer f.Close()
	log.Debug(ctx, "Lyrics: sidecar file found")

	contents, err := io.ReadAll(ioutils.UTF8Reader(f))
	if err != nil {
		log.Debug(ctx, "Lyrics: sidecar read failed", "error", err)
		return nil, err
	}

	list, err := model.ParseLyrics(ctx, suffix, "xxx", contents)
	if err != nil {
		log.Error(ctx, "error parsing external lyric file", err)
		return nil, err
	}

	if len(list) == 0 {
		log.Debug(ctx, "Lyrics: sidecar parsed but contained no lyrics", "bytes", len(contents))
		return nil, nil
	}

	log.Debug(ctx, "Lyrics: sidecar parsed", "bytes", len(contents), "entries", len(list), "lines", len(list[0].Line), "synced", list[0].Synced)
	return list, nil
}

// fromPlugin attempts to load lyrics from a plugin with the given name. Results are cached,
// misses included: clients ask for every track in a queue, and plugin lookups are slow.
func (l *lyricsService) fromPlugin(ctx context.Context, mf *model.MediaFile, pluginName string) (model.LyricList, error) {
	if l.pluginLoader == nil {
		log.Debug(ctx, "Invalid lyric source", "source", pluginName)
		return nil, nil
	}

	provider, ok := l.pluginLoader.LoadLyricsProvider(pluginName)
	if !ok {
		log.Warn(ctx, "Lyrics plugin not found", "plugin", pluginName)
		return nil, nil
	}

	key := pluginKey{plugin: pluginName, mediaFileID: mf.ID}
	log.Debug(ctx, "Lyrics: checking plugin source", "plugin", pluginName)
	return l.pluginCache.GetWithLoader(key, func(pluginKey) (model.LyricList, time.Duration, error) {
		// Concurrent callers share one load, so one cancelled request must not abort it for the others.
		loadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lyricsLoadTimeout)
		defer cancel()
		lyricsList, err := provider.GetLyrics(loadCtx, mf)
		if err != nil {
			// Cached as a miss: some plugins report "no lyrics" as an error.
			log.Error(ctx, "Error getting lyrics from plugin", "plugin", pluginName, err)
			return nil, 0, nil
		}
		if len(lyricsList) > 0 {
			log.Trace(ctx, "Retrieved lyrics from plugin", "plugin", pluginName, "count", len(lyricsList))
		}
		return lyricsList, 0, nil
	})
}
