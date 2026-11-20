package server

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

const tinyPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+lLqkAAAAASUVORK5CYII="

func TestNormalizeOIDCAvatar(t *testing.T) {
	issuer := "https://auth.example.org/application/o/musica"
	valid := []struct{ input, want string }{
		{"https://cdn.example.org/a.png", "https://cdn.example.org/a.png"},
		{"/media/avatars/alice.png", "https://auth.example.org/media/avatars/alice.png"},
		{tinyPNG, "data:image/png;base64," + tinyPNG},
		{"data:image/png;base64," + tinyPNG, "data:image/png;base64," + tinyPNG},
	}
	for _, tc := range valid {
		if got := normalizeOIDCAvatar(tc.input, issuer); got != tc.want {
			t.Errorf("normalize %q: got %q want %q", tc.input[:min(len(tc.input), 50)], got, tc.want)
		}
	}
	invalid := []string{"javascript:alert(1)", "http://insecure.example.org/a.png", "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte("<svg/>")), "https://user:pass@example.org/a.png", "not-an-image", strings.Repeat("A", 400001)}
	for _, s := range invalid {
		if got := normalizeOIDCAvatar(s, issuer); got != "" {
			t.Errorf("accepted unsafe avatar %q", s[:min(len(s), 30)])
		}
	}
}
func TestOIDCAvatarClaimPreference(t *testing.T) {
	c := oidcAvatarClaims{Picture: "javascript:alert(1)", Avatar: "https://auth.example.org/user/avatar.png"}
	if got := c.avatar("https://auth.example.org/application/o/musica"); got != c.Avatar {
		t.Fatalf("unexpected avatar: %q", got)
	}
}

func TestNormalizeOIDCAvatarLargeBase64(t *testing.T) {
	// Authentik users may have profile pictures larger than 256 KiB.
	// The PNG signature is sufficient for http.DetectContentType; this test
	// exercises the image-claim size check without shipping actual user images.
	picture := append([]byte{137, 80, 78, 71, 13, 10, 26, 10}, bytes.Repeat([]byte{0x01}, 350<<10)...)
	raw := base64.StdEncoding.EncodeToString(picture)
	got := normalizeOIDCAvatar("data:image/png;base64,"+raw, "https://auth.example.org/application/o/musica")
	if got != "data:image/png;base64,"+raw {
		t.Fatal("expected a valid 350 KiB profile picture to be accepted")
	}
	if got := normalizeOIDCAvatar("data:image/png;base64,"+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x01}, maxOIDCAvatarBytes+1)), "https://auth.example.org"); got != "" {
		t.Fatal("image larger than the maximum should be rejected")
	}
}

func TestNormalizeOIDCAvatarWrappedBase64(t *testing.T) {
	wrapped := tinyPNG[:20] + "\n" + tinyPNG[20:]
	got := normalizeOIDCAvatar(wrapped, "https://auth.example.org")
	if got != "data:image/png;base64,"+tinyPNG {
		t.Fatal("expected newline-wrapped base64 PNG to be accepted")
	}
}
