package nativeapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMusicaPlaylistIDFromURL(t *testing.T) {
	cases := []struct {
		input    string
		expected string
	}{
		{"https://music.youtube.com/playlist?list=PLabcdef012345", "PLabcdef012345"},
		{"https://www.youtube.com/watch?v=ABC12345678&list=PLabcdef012345", "PLabcdef012345"},
		{"https://evil.example.com/playlist?list=PLabcdef012345", ""},
		{"http://music.youtube.com/playlist?list=PLabcdef012345", ""},
		{"https://music.youtube.com/playlist?list=PLa&list=PLb", ""},
		{"https://music.youtube.com/playlist?list=bad", ""},
		{"https://music.youtube.com:8080/playlist?list=PLabcdef012345", ""},
		{"https://music.youtube.com.evil.example/playlist?list=PLabcdef012345", ""},
	}
	for _, test := range cases {
		result, err := musicaPlaylistIDFromURL(test.input)
		if test.expected == "" && err == nil || test.expected != "" && (err != nil || result != test.expected) {
			t.Errorf("playlist ID for %s: got %q, err %v", test.input, result, err)
		}
	}
}

func TestMusicaYTWorkerHeaders(t *testing.T) {
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/sync" && r.Header.Get("X-API-Key") != "test-secret" {
			t.Errorf("sync did not receive API key")
		}
		if r.URL.Path == "/status" && r.Header.Get("X-API-Key") != "" {
			t.Errorf("status should not receive API key")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ready":true,"state":"ready"}`))
	}))
	defer service.Close()
	t.Setenv("MUSICASYT_URL", service.URL)
	t.Setenv("MUSICASYT_API_KEY", "test-secret")
	for _, tc := range []struct {
		method, path string
		payload      any
	}{
		{http.MethodGet, "/status", nil},
		{http.MethodPost, "/sync", map[string]any{"username": "alice", "playlist_url": "https://music.youtube.com/playlist?list=PLabcdef012345"}},
	} {
		status, result, err := musicaYTCall(tc.method, tc.path, tc.payload)
		if err != nil || status != http.StatusOK || !strings.Contains(string(result), `"ready":true`) {
			t.Fatalf("got code %d, body %s, err %v", status, result, err)
		}
	}
}

func TestMusicaYTRequiresConfiguration(t *testing.T) {
	t.Setenv("MUSICASYT_URL", "")
	t.Setenv("MUSICASYT_API_KEY", "")
	status, _, err := musicaYTCall(http.MethodGet, "/status", nil)
	if status != http.StatusServiceUnavailable || err == nil {
		t.Fatalf("expected service unavailable, got status=%d error=%v", status, err)
	}
}
