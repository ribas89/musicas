package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/navidrome/navidrome/log"
)

// Image claims can come from the ID token or Authentik's UserInfo endpoint.
// We accept the OIDC standard 'picture', Authentik/custom 'avatar', and common
// alternate profile keys; the username and identity are always taken from the
// separately verified ID token, never from this metadata.
type oidcAvatarClaims struct {
	Picture   string `json:"picture"`
	Avatar    string `json:"avatar"`
	AvatarURL string `json:"avatar_url"`
	Profile   struct {
		Picture   string `json:"picture"`
		Avatar    string `json:"avatar"`
		AvatarURL string `json:"avatar_url"`
	} `json:"profile"`
}

func (c oidcAvatarClaims) avatar(issuer string) string {
	for _, candidate := range []string{c.Picture, c.Avatar, c.AvatarURL, c.Profile.Picture, c.Profile.Avatar, c.Profile.AvatarURL} {
		if avatar := normalizeOIDCAvatar(candidate, issuer); avatar != "" {
			return avatar
		}
	}
	return ""
}

// Image URLs and image data URIs can be displayed directly by the existing
// React user-menu Avatar component. Reject javascript:, svg, file:, oversized
// images, and insecure HTTP to avoid adding untrusted active content to the UI.
// Keep base64 images bounded: ~1 MiB decoded is enough for a profile photo,
// while allowing Authentik images larger than the original 256 KiB limit.
const maxOIDCAvatarBytes = 1 << 20

func normalizeOIDCAvatar(candidate, issuer string) string {
	candidate = strings.TrimSpace(candidate)
	if candidate == "" || len(candidate) > ((maxOIDCAvatarBytes+2)/3*4)+256 {
		return ""
	}
	if strings.HasPrefix(candidate, "/") && !strings.HasPrefix(candidate, "//") {
		base, err := url.Parse(issuer)
		if err != nil || base.Scheme != "https" || base.Host == "" {
			return ""
		}
		base.Path, base.RawQuery, base.Fragment = "", "", ""
		resolved := base.ResolveReference(&url.URL{Path: candidate})
		if len(resolved.String()) <= 2048 {
			return resolved.String()
		}
		return ""
	}
	if u, err := url.Parse(candidate); err == nil && u.Scheme == "https" && u.Host != "" &&
		u.User == nil && u.Fragment == "" && len(candidate) <= 2048 {
		return u.String()
	}

	media := ""
	raw := candidate
	if strings.HasPrefix(strings.ToLower(candidate), "data:") {
		header, encoded, found := strings.Cut(candidate, ",")
		if !found || !strings.HasSuffix(strings.ToLower(header), ";base64") {
			return ""
		}
		media = strings.ToLower(strings.TrimPrefix(strings.TrimSuffix(strings.ToLower(header), ";base64"), "data:"))
		if media != "image/png" && media != "image/jpeg" && media != "image/gif" && media != "image/webp" {
			return ""
		}
		raw = encoded
	}
	// Some identity providers fold long base64 values across lines. Normalize
	// only whitespace; never decode or display unsafe SVG or HTML content.
	raw = strings.Map(func(r rune) rune {
		if r == ' ' || r == '\n' || r == '\r' || r == '\t' {
			return -1
		}
		return r
	}, raw)
	if len(raw) > ((maxOIDCAvatarBytes+2)/3*4)+8 {
		return ""
	}
	var decoded []byte
	var err error
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		decoded, err = encoding.DecodeString(raw)
		if err == nil {
			break
		}
	}
	if err != nil || len(decoded) == 0 || len(decoded) > maxOIDCAvatarBytes {
		return ""
	}
	kind := http.DetectContentType(decoded)
	if len(decoded) >= 12 && string(decoded[:4]) == "RIFF" && string(decoded[8:12]) == "WEBP" {
		kind = "image/webp"
	}
	if kind != "image/png" && kind != "image/jpeg" && kind != "image/gif" && kind != "image/webp" {
		return ""
	}
	if media != "" && media != kind {
		return ""
	}
	return fmt.Sprintf("data:%s;base64,%s", kind, base64.StdEncoding.EncodeToString(decoded))
}

func (o *nativeOIDC) readUserInfoAvatar(ctx context.Context, identity *oidcIdentity, accessToken, endpoint string) {
	if identity == nil || endpoint == "" || accessToken == "" {
		return
	}
	// The endpoint is HTTPS and same-host verified by discover().
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := o.httpClient.Do(req)
	if err != nil {
		log.Warn(ctx, "Unable to fetch OIDC UserInfo avatar", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	var info struct {
		Subject string `json:"sub"`
		oidcAvatarClaims
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&info); err != nil {
		log.Warn(ctx, "Unable to parse OIDC UserInfo", err)
		return
	}
	if info.Subject != identity.Subject {
		log.Warn(ctx, "OIDC UserInfo subject differs from verified ID token")
		return
	}
	if avatar := info.oidcAvatarClaims.avatar(o.issuer); avatar != "" {
		identity.Avatar = avatar
	}
	// Never log the claim itself: a data:image URL can contain a full private photo.
	log.Debug(ctx, "OIDC UserInfo avatar result", "provided", info.Picture != "" || info.Avatar != "" || info.AvatarURL != "" || info.Profile.Picture != "" || info.Profile.Avatar != "" || info.Profile.AvatarURL != "", "accepted", identity.Avatar != "")
}
