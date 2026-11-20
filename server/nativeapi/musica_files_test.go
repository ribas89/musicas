package nativeapi

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMusicaCleanRelative(t *testing.T) {
	for _, tc := range []struct {
		path      string
		allowRoot bool
		ok        bool
	}{
		{"", true, true}, {"Favorites", false, true}, {"Favorites/Live", false, true},
		{"..", true, false}, {"../bob", true, false}, {"/music/public", false, false},
		{"Favorites/../bob", false, false}, {"Favorites//Live", false, false},
		{"Favorites\\Live", false, false}, {"C:/music", false, false},
		{".musica-upload", false, false}, {"Favorites/.musica-upload", false, false},
		{"", false, false},
	} {
		_, err := musicaCleanRelative(tc.path, tc.allowRoot)
		if (err == nil) != tc.ok {
			t.Errorf("path=%q: valid=%v want=%v", tc.path, err == nil, tc.ok)
		}
	}
}

func TestMusicaSafeNames(t *testing.T) {
	for _, bad := range []string{"", "..", "a/b", "a\\b", ".musica-temp", "\x00"} {
		if musicaSafeName(bad) == nil {
			t.Errorf("unexpectedly accepted %q", bad)
		}
	}
	for _, good := range []string{"Rock", "Road Trip.zip", "Classical (2025)"} {
		if err := musicaSafeName(good); err != nil {
			t.Errorf("rejected %q: %v", good, err)
		}
	}
}

type musicaTestZip struct{ *bytes.Reader }

func (*musicaTestZip) Close() error { return nil }

func makeMusicaZip(t *testing.T, entryName string, data string) *musicaTestZip {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, err := zw.Create(entryName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte(data)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return &musicaTestZip{bytes.NewReader(buf.Bytes())}
}

func TestMusicaZipExtract(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	z := makeMusicaZip(t, "Disc 1/song.mp3", "fake audio")
	name, err := musicaZipExtract(root, "My Album", z, z.Size())
	if err != nil || name != "My Album" {
		t.Fatalf("extract got %q: %v", name, err)
	}
	content, err := os.ReadFile(filepath.Join(dir, "My Album", "Disc 1", "song.mp3"))
	if err != nil || string(content) != "fake audio" {
		t.Fatalf("incorrect file: %v", err)
	}
	z = makeMusicaZip(t, "song.mp3", "replace")
	if _, err := musicaZipExtract(root, "My Album", z, z.Size()); err == nil {
		t.Fatal("overwrote existing extracted folder")
	}
}

func TestMusicaZipRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, name := range []string{"../escape.mp3", "/escape.mp3", "Folder/../../escape.mp3", "C:/escape.mp3"} {
		z := makeMusicaZip(t, name, "danger")
		if _, err := musicaZipExtract(root, "TestAlbum", z, z.Size()); err == nil {
			t.Errorf("accepted ZIP path %q", name)
		}
	}
	files, _ := os.ReadDir(dir)
	for _, file := range files {
		if strings.HasPrefix(file.Name(), ".musica-") {
			t.Errorf("left staging directory %s", file.Name())
		}
	}
}
