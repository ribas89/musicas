package scanner

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/navidrome/navidrome/model"
)

func TestOIDCManagedUserUsesOnlyPrivateLibraries(t *testing.T) {
	root := filepath.Join(t.TempDir(), "users")
	t.Setenv("MUSIC_USERS_ROOT", root)
	good := model.Library{ID: 7, Name: "OIDC: alice", Path: filepath.Join(root, "alice")}
	if got := oidcManagedUser(good); got != "alice" {
		t.Fatalf("got %q", got)
	}
	for _, bad := range []model.Library{
		{Name: "Public", Path: filepath.Join(filepath.Dir(root), "public")},
		{Name: "OIDC: alice", Path: filepath.Join(root, "other", "alice")},
		{Name: "another name", Path: good.Path},
		{Name: "OIDC: users", Path: root},
	} {
		if got := oidcManagedUser(bad); got != "" {
			t.Fatalf("accepted non-private library: %+v", bad)
		}
	}
}

func TestNativeFolderRulesMatchLibraryAndPrefix(t *testing.T) {
	rules := nativeFolderRules(7, "Road_Trip%")
	raw, err := json.Marshal(rules)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		All []map[string]map[string]any `json:"all"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.All) != 2 || doc.All[0]["is"]["library_id"] != float64(7) ||
		doc.All[1]["startsWith"]["filepath"] != "Road_Trip%/" {
		t.Fatalf("incorrect native rules: %s", raw)
	}
}

func TestManagedFolderMarkerIncludesLibrary(t *testing.T) {
	if folderPlaylistComment(5, "Road Trip") == folderPlaylistComment(6, "Road Trip") {
		t.Fatal("folder marker is not scoped to native library ID")
	}
}
