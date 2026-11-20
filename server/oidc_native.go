package server

// Direct OIDC authentication for Navidrome. This is intentionally independent of
// ExtAuth/Remote-User and does not accept client-supplied identity headers.

import (
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/consts"
	"github.com/navidrome/navidrome/core/auth"
	"github.com/navidrome/navidrome/db"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/model/id"
)

type nativeOIDC struct {
	ds                                          model.DataStore
	issuer, clientID, clientSecret, redirectURL string
	browserURL, authBase                        string
	secret                                      []byte
	httpClient                                  *http.Client
	provisionMu                                 sync.Mutex
}

type oidcDiscovery struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
	UserInfoEndpoint      string `json:"userinfo_endpoint"`
}

type oidcTransaction struct {
	State    string `json:"state"`
	Nonce    string `json:"nonce"`
	Verifier string `json:"verifier"`
	Expires  int64  `json:"expires"`
}

type oidcTicket struct {
	UserID  string `json:"uid"`
	Stage   string `json:"stage"`
	CSRF    string `json:"csrf"`
	Expires int64  `json:"expires"`
}

type oidcIdentity struct{ Subject, Username, Avatar string }

var oidcUsernamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)

func newNativeOIDC(ds model.DataStore) (*nativeOIDC, error) {
	issuer := strings.TrimSpace(os.Getenv("OIDC_ISSUER_URL"))
	clientID := strings.TrimSpace(os.Getenv("OIDC_CLIENT_ID"))
	clientSecret := os.Getenv("OIDC_CLIENT_SECRET")
	redirectURL := strings.TrimSpace(os.Getenv("OIDC_REDIRECT_URL"))
	if issuer == "" || clientID == "" || clientSecret == "" || redirectURL == "" {
		return nil, errors.New("OIDC_ISSUER_URL, OIDC_CLIENT_ID, OIDC_CLIENT_SECRET and OIDC_REDIRECT_URL must all be configured")
	}
	issuer = strings.TrimSuffix(issuer, "/")
	iu, err := url.Parse(issuer)
	if err != nil || iu.Scheme != "https" || iu.Host == "" || iu.RawQuery != "" || iu.Fragment != "" {
		return nil, errors.New("OIDC_ISSUER_URL must be a full HTTPS issuer URL")
	}
	ru, err := url.Parse(redirectURL)
	if err != nil || ru.Scheme != "https" || ru.Host == "" || ru.RawQuery != "" || ru.Fragment != "" || !strings.HasSuffix(ru.Path, "/auth/oidc/callback") {
		return nil, errors.New("OIDC_REDIRECT_URL must be a full HTTPS URL ending in /auth/oidc/callback")
	}
	// The client must be registered under this exact callback; don't infer a URL
	// from untrusted Host or Forwarded headers.
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	base := strings.TrimRight(conf.Server().BasePath, "/")
	if ru.Path != path.Join(base, "/auth/oidc/callback") {
		return nil, fmt.Errorf("OIDC_REDIRECT_URL path must match Navidrome BasePath (%s)", base)
	}
	o := &nativeOIDC{ds: ds, issuer: issuer, clientID: clientID, clientSecret: clientSecret, redirectURL: redirectURL,
		browserURL: base + "/app/?oidc=1#/login", authBase: base + "/auth/oidc", secret: secret,
		httpClient: &http.Client{Timeout: 12 * time.Second},
	}
	// Discover at first login (not process startup), allowing Navidrome to start
	// during temporary Authentik outages.
	return o, nil
}

func (o *nativeOIDC) discover(ctx context.Context) (oidcDiscovery, error) {
	endpoint := o.issuer + "/.well-known/openid-configuration"
	var d oidcDiscovery
	if err := o.getJSON(ctx, endpoint, &d); err != nil {
		return d, fmt.Errorf("OIDC discovery: %w", err)
	}
	if strings.TrimSuffix(d.Issuer, "/") != o.issuer {
		return d, errors.New("OIDC discovery issuer mismatch")
	}
	endpoints := []string{d.AuthorizationEndpoint, d.TokenEndpoint, d.JWKSURI}
	if d.UserInfoEndpoint != "" {
		endpoints = append(endpoints, d.UserInfoEndpoint)
	}
	for _, raw := range endpoints {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return d, errors.New("OIDC discovery contains a non-HTTPS endpoint")
		}
		if !strings.EqualFold(u.Host, mustParseURL(o.issuer).Host) {
			return d, errors.New("OIDC endpoint has a different host from the issuer")
		}
	}
	return d, nil
}

func mustParseURL(s string) *url.URL { u, _ := url.Parse(s); return u }

func (o *nativeOIDC) getJSON(ctx context.Context, addr string, result any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, addr, nil)
	if err != nil {
		return err
	}
	resp, err := o.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d at %s", resp.StatusCode, mustParseURL(addr).Host)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(result)
}

func randomURLToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func (o *nativeOIDC) seal(payload any) (string, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	msg := base64.RawURLEncoding.EncodeToString(data)
	mac := hmac.New(sha256.New, o.secret)
	_, _ = mac.Write([]byte(msg))
	return msg + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func (o *nativeOIDC) unseal(value string, out any) error {
	segments := strings.Split(value, ".")
	if len(segments) != 2 {
		return errors.New("invalid ticket")
	}
	given, err := base64.RawURLEncoding.DecodeString(segments[1])
	if err != nil {
		return err
	}
	mac := hmac.New(sha256.New, o.secret)
	_, _ = mac.Write([]byte(segments[0]))
	if !hmac.Equal(given, mac.Sum(nil)) {
		return errors.New("invalid ticket signature")
	}
	raw, err := base64.RawURLEncoding.DecodeString(segments[0])
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

func (o *nativeOIDC) setCookie(w http.ResponseWriter, name, value string, seconds int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: strings.TrimSuffix(o.authBase, "/"),
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: seconds})
}
func (o *nativeOIDC) clearCookie(w http.ResponseWriter, name string) { o.setCookie(w, name, "", -1) }

func (o *nativeOIDC) start(w http.ResponseWriter, r *http.Request) {
	discovery, err := o.discover(r.Context())
	if err != nil {
		log.Error(r, "OIDC discovery failed", err)
		http.Error(w, "Authentik is currently unavailable", http.StatusBadGateway)
		return
	}
	state, err := randomURLToken()
	if err != nil {
		http.Error(w, "Unable to start login", 500)
		return
	}
	nonce, err := randomURLToken()
	if err != nil {
		http.Error(w, "Unable to start login", 500)
		return
	}
	verifier, err := randomURLToken()
	if err != nil {
		http.Error(w, "Unable to start login", 500)
		return
	}
	signed, err := o.seal(oidcTransaction{State: state, Nonce: nonce, Verifier: verifier, Expires: time.Now().Add(5 * time.Minute).Unix()})
	if err != nil {
		http.Error(w, "Unable to start login", 500)
		return
	}
	o.setCookie(w, "nd_oidc_state", signed, 300)
	challenge := sha256.Sum256([]byte(verifier))
	u, _ := url.Parse(discovery.AuthorizationEndpoint)
	q := u.Query()
	q.Set("client_id", o.clientID)
	q.Set("redirect_uri", o.redirectURL)
	q.Set("response_type", "code")
	q.Set("scope", "openid profile email")
	q.Set("state", state)
	q.Set("nonce", nonce)
	q.Set("code_challenge", base64.RawURLEncoding.EncodeToString(challenge[:]))
	q.Set("code_challenge_method", "S256")
	u.RawQuery = q.Encode()
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, u.String(), http.StatusFound)
}

// redirectOIDCCallbackFailure sends the browser back to Musicas without
// displaying a raw authentication error. Never automatically restart an OIDC
// flow here: a stale or replayed callback must not cause a redirect loop.
func (o *nativeOIDC) redirectOIDCCallbackFailure(w http.ResponseWriter, r *http.Request) {
	o.clearCookie(w, "nd_oidc_state")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	root := strings.TrimRight(conf.Server().BasePath, "/") + "/"
	http.Redirect(w, r, root, http.StatusSeeOther)
}

func (o *nativeOIDC) callback(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("error") != "" {
		o.redirectOIDCCallbackFailure(w, r)
		return
	}
	cookie, err := r.Cookie("nd_oidc_state")
	o.clearCookie(w, "nd_oidc_state")
	if err != nil {
		o.redirectOIDCCallbackFailure(w, r)
		return
	}
	var txn oidcTransaction
	if err := o.unseal(cookie.Value, &txn); err != nil || txn.Expires < time.Now().Unix() ||
		subtle.ConstantTimeCompare([]byte(txn.State), []byte(r.URL.Query().Get("state"))) != 1 {
		o.redirectOIDCCallbackFailure(w, r)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" || len(code) > 8192 {
		http.Error(w, "Missing authorization code", http.StatusBadRequest)
		return
	}
	// No fallback to previously discovered endpoint when discovery fails: always
	// validate issuer and endpoints on the incoming login attempt.
	discovery, err := o.discover(r.Context())
	if err != nil {
		http.Error(w, "OIDC discovery failed", http.StatusBadGateway)
		return
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {o.redirectURL},
		"client_id": {o.clientID}, "code_verifier": {txn.Verifier}}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, discovery.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		http.Error(w, "OIDC token request error", 502)
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(o.clientID, o.clientSecret)
	resp, err := o.httpClient.Do(req)
	if err != nil {
		http.Error(w, "OIDC token exchange failed", 502)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		o.redirectOIDCCallbackFailure(w, r)
		return
	}
	var token struct {
		IDToken     string `json:"id_token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&token); err != nil || token.IDToken == "" {
		o.redirectOIDCCallbackFailure(w, r)
		return
	}
	identity, err := o.verifyIDToken(r.Context(), token.IDToken, txn.Nonce, discovery.JWKSURI)
	if err != nil {
		log.Warn(r, "OIDC token validation failed", err)
		o.redirectOIDCCallbackFailure(w, r)
		return
	}
	// UserInfo is optional, but Authentik may expose picture/avatar only there.
	// The UserInfo subject must match the already verified ID-token subject.
	o.readUserInfoAvatar(r.Context(), &identity, token.AccessToken, discovery.UserInfoEndpoint)
	// Diagnostic only: report presence, never the image or authentication token.
	log.Debug(r, "OIDC avatar on login", "accepted", identity.Avatar != "")
	user, err := o.provision(r.Context(), identity)
	if err != nil {
		log.Error(r, "OIDC user provisioning failed", err)
		http.Error(w, "Unable to provision this account: "+err.Error(), 409)
		return
	}
	existing, err := o.ds.User().FindByUsernameWithPassword(r.Context(), user.UserName)
	if err != nil {
		http.Error(w, "Unable to load user", 500)
		return
	}
	stage := "login"
	if strings.HasPrefix(existing.Password, consts.PasswordAutogenPrefix) {
		stage = "password"
	}
	csrf, err := randomURLToken()
	if err != nil {
		http.Error(w, "Unable to complete login", 500)
		return
	}
	ticket, err := o.seal(oidcTicket{UserID: user.ID, Stage: stage, CSRF: csrf, Expires: time.Now().Add(10 * time.Minute).Unix()})
	if err != nil {
		http.Error(w, "Unable to complete login", 500)
		return
	}
	o.setCookie(w, "nd_oidc_ticket", ticket, 600)
	w.Header().Set("Cache-Control", "no-store")
	if stage == "password" {
		http.Redirect(w, r, o.authBase+"/password", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, o.browserURL, http.StatusSeeOther)
}

// Password setup is intentionally a *Navidrome* password for legacy music
// clients. We never request, receive, or store the Authentik password.
var oidcPasswordPage = template.Must(template.New("password").Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Set Navidrome password</title><style>body{margin:0;background:#141b2b;color:#fff;font-family:system-ui;display:grid;place-items:center;min-height:100vh}main{box-sizing:border-box;width:min(92vw,420px);padding:28px;background:#222d42;border-radius:14px}label{display:block;margin-top:18px}input{box-sizing:border-box;width:100%;padding:12px;margin-top:7px;border-radius:7px;border:1px solid #7f8ca3;background:#121827;color:#fff}button{width:100%;margin-top:24px;padding:13px;border:0;border-radius:7px;background:#596be9;color:#fff;font-weight:700;cursor:pointer}small{color:#bac5d5;line-height:1.6}.error{color:#ffb8b8}</style></head><body><main><h2>Set your Navidrome password</h2><p>Signed in with Authentik as <strong>{{.Username}}</strong>.</p><small>This is a separate password for Subsonic music players. Do NOT enter your Authentik password.</small>{{if .Error}}<p class="error">{{.Error}}</p>{{end}}<form method="post" action="{{.Action}}"><input type="hidden" name="csrf" value="{{.CSRF}}"><label>New Navidrome password<input name="password" type="password" autocomplete="new-password" required></label><label>Confirm password<input name="confirm" type="password" autocomplete="new-password" required></label><button type="submit">Save and open Navidrome</button></form></main></body></html>`))

func (o *nativeOIDC) loadTicket(r *http.Request, stage string) (*oidcTicket, *model.User, error) {
	cookie, err := r.Cookie("nd_oidc_ticket")
	if err != nil {
		return nil, nil, err
	}
	var ticket oidcTicket
	if err := o.unseal(cookie.Value, &ticket); err != nil || ticket.Expires < time.Now().Unix() || ticket.Stage != stage {
		return nil, nil, errors.New("expired or invalid login ticket")
	}
	user, err := o.ds.User().Get(r.Context(), ticket.UserID)
	if err != nil || user == nil {
		return nil, nil, errors.New("user not found")
	}
	return &ticket, user, nil
}

func (o *nativeOIDC) password(w http.ResponseWriter, r *http.Request) {
	ticket, user, err := o.loadTicket(r, "password")
	if err != nil {
		http.Error(w, "Sign in again with Authentik", 401)
		return
	}
	render := func(message string) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_ = oidcPasswordPage.Execute(w, map[string]string{"Username": user.UserName, "CSRF": ticket.CSRF, "Action": o.authBase + "/password", "Error": message})
	}
	if r.Method == http.MethodGet {
		render("")
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		render("Invalid form")
		return
	}
	if subtle.ConstantTimeCompare([]byte(ticket.CSRF), []byte(r.PostFormValue("csrf"))) != 1 {
		http.Error(w, "Invalid CSRF token", 403)
		return
	}
	pw := r.PostFormValue("password")
	if pw == "" {
		render("Password cannot be empty")
		return
	}
	if pw != r.PostFormValue("confirm") {
		render("Passwords do not match")
		return
	}
	// Reject stale tickets if password has been set in another tab already.
	current, err := o.ds.User().FindByUsernameWithPassword(r.Context(), user.UserName)
	if err != nil || !strings.HasPrefix(current.Password, consts.PasswordAutogenPrefix) {
		http.Error(w, "Password already configured; sign in again", 409)
		return
	}
	current.NewPassword = pw
	if err := o.ds.User().Put(r.Context(), current); err != nil {
		log.Error(r, "Setting OIDC user's Navidrome password", err)
		http.Error(w, "Could not save password", 500)
		return
	}
	ticket.Stage = "login"
	signed, err := o.seal(ticket)
	if err != nil {
		http.Error(w, "Unable to finish sign in", 500)
		return
	}
	o.setCookie(w, "nd_oidc_ticket", signed, 600)
	http.Redirect(w, r, o.browserURL, http.StatusSeeOther)
}

func (o *nativeOIDC) session(w http.ResponseWriter, r *http.Request) {
	_, u, err := o.loadTicket(r, "login")
	o.clearCookie(w, "nd_oidc_ticket")
	if err != nil {
		http.Error(w, "Sign in again with Authentik", 401)
		return
	}
	full, err := o.ds.User().FindByUsernameWithPassword(r.Context(), u.UserName)
	if err != nil {
		http.Error(w, "Login failed", 500)
		return
	}
	token, err := auth.CreateToken(full)
	if err != nil {
		http.Error(w, "Login failed", 500)
		return
	}
	payload := buildAuthPayload(full)
	payload["token"] = token
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}

// Authentik's stable (iss,sub) is the account key. An existing Navidrome user
// with the same username is never silently claimed by a new OIDC identity.
func (o *nativeOIDC) provision(ctx context.Context, identity oidcIdentity) (*model.User, error) {
	if !oidcUsernamePattern.MatchString(identity.Username) || strings.Contains(identity.Username, "..") {
		return nil, errors.New("Authentik username must use 1-64 alphanumeric, dot, hyphen or underscore characters, and must not contain '..'")
	}
	o.provisionMu.Lock()
	defer o.provisionMu.Unlock()
	var linkedID string
	err := db.Db().QueryRowContext(ctx, "SELECT user_id FROM oidc_identity WHERE issuer = ? AND subject = ?", o.issuer, identity.Subject).Scan(&linkedID)
	switch {
	case err == nil:
		user, err := o.ds.User().Get(ctx, linkedID)
		if err != nil {
			return nil, fmt.Errorf("OIDC identity links to a missing user: %w", err)
		}
		if !strings.EqualFold(user.UserName, identity.Username) {
			return nil, errors.New("Authentik username changed; administrator must migrate the library and account")
		}
		if err := o.ensureUserLibrary(ctx, user); err != nil {
			return nil, err
		}
		if _, err := db.Db().ExecContext(ctx, "UPDATE oidc_identity SET avatar = ? WHERE issuer = ? AND subject = ? AND user_id = ?", identity.Avatar, o.issuer, identity.Subject, user.ID); err != nil {
			return nil, fmt.Errorf("updating OIDC avatar: %w", err)
		}
		user.Avatar = identity.Avatar
		_ = o.ds.User().UpdateLastLoginAt(ctx, user.ID)
		return user, nil
	case !errors.Is(err, sql.ErrNoRows):
		return nil, fmt.Errorf("OIDC lookup failed: %w", err)
	}
	count, err := o.ds.User().CountAll(ctx)
	if err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, errors.New("create the Navidrome administrator before first Authentik login (ADMIN_USER and ADMIN_PASSWORD)")
	}
	old, err := o.ds.User().FindByUsername(ctx, identity.Username)
	if err == nil && old != nil {
		return nil, errors.New("a Navidrome account already uses this username; automatic OIDC linking is disabled to prevent account takeover")
	}
	if err != nil && !errors.Is(err, model.ErrNotFound) {
		return nil, err
	}
	user := &model.User{ID: id.NewRandom(), UserName: identity.Username, Name: identity.Username,
		NewPassword: consts.PasswordAutogenPrefix + id.NewRandom(), IsAdmin: false}
	if err := o.ds.User().Put(ctx, user); err != nil {
		return nil, fmt.Errorf("creating Navidrome user: %w", err)
	}
	if _, err := db.Db().ExecContext(ctx, "INSERT INTO oidc_identity(issuer,subject,user_id,avatar) VALUES(?,?,?,?)", o.issuer, identity.Subject, user.ID, identity.Avatar); err != nil {
		return nil, fmt.Errorf("binding OIDC account: %w", err)
	}
	if err := o.ensureUserLibrary(ctx, user); err != nil {
		return nil, err
	}
	user.Avatar = identity.Avatar
	return user, nil
}

func (o *nativeOIDC) ensureUserLibrary(ctx context.Context, user *model.User) error {
	// Layout is validated and created once at server startup. The public and
	// users roots are siblings, never nested: a public scan cannot enter /users.
	root := filepath.Clean(os.Getenv("MUSIC_USERS_ROOT"))
	shared := filepath.Clean(conf.Server().MusicFolder)
	if err := validateMusicLayout(shared, root); err != nil {
		return err
	}
	dir := filepath.Join(root, user.UserName)
	if info, err := os.Lstat(dir); err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return errors.New("unsafe existing user library directory")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(dir, 0750); err != nil {
		return fmt.Errorf("creating private music library: %w", err)
	}
	libs, err := o.ds.Library().GetAll(ctx)
	if err != nil {
		return err
	}
	var target *model.Library
	for i := range libs {
		lib := &libs[i]
		if filepath.Clean(lib.Path) == dir {
			target = lib
			break
		}
	}
	expectedName := "OIDC: " + user.UserName
	if target != nil && target.Name != expectedName {
		return errors.New("user folder already belongs to a different Navidrome library")
	}
	if target == nil {
		target = &model.Library{Name: expectedName, Path: dir, DefaultNewUsers: false}
		if err := o.ds.Library().Put(ctx, target); err != nil {
			return fmt.Errorf("creating Navidrome library: %w", err)
		}
	}
	if target.ID == 0 {
		return errors.New("could not determine new library ID")
	}
	// The default library is the shared /music/public collection. Every
	// non-admin OIDC user gets exactly that library and their own private one.
	public, err := o.ds.Library().Get(ctx, model.DefaultLibraryID)
	if err != nil {
		return fmt.Errorf("loading shared library: %w", err)
	}
	if filepath.Clean(public.Path) != shared {
		return errors.New("default Navidrome library must point at the configured public folder")
	}
	if err := o.ds.User().SetUserLibraries(ctx, user.ID, []int{public.ID, target.ID}); err != nil {
		return fmt.Errorf("assigning shared and private library access: %w", err)
	}
	return nil
}

func (o *nativeOIDC) verifyIDToken(ctx context.Context, signed, expectedNonce, jwksURL string) (oidcIdentity, error) {
	parts := strings.Split(signed, ".")
	if len(parts) != 3 {
		return oidcIdentity{}, errors.New("malformed ID token")
	}
	headRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return oidcIdentity{}, err
	}
	var header struct {
		Algorithm string `json:"alg"`
		KeyID     string `json:"kid"`
		Type      string `json:"typ"`
	}
	if err := json.Unmarshal(headRaw, &header); err != nil {
		return oidcIdentity{}, err
	}
	// Authentik normally uses RS256. Explicit allowlist eliminates alg=none
	// and symmetric algorithm confusion attacks.
	if header.Algorithm != "RS256" || header.KeyID == "" {
		return oidcIdentity{}, errors.New("unsupported ID token algorithm (RS256 required) or missing key ID")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return oidcIdentity{}, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return oidcIdentity{}, err
	}
	var keyset struct {
		Keys []struct {
			Kty string   `json:"kty"`
			Kid string   `json:"kid"`
			Alg string   `json:"alg"`
			Use string   `json:"use"`
			N   string   `json:"n"`
			E   string   `json:"e"`
			X5C []string `json:"x5c"`
		} `json:"keys"`
	}
	if err := o.getJSON(ctx, jwksURL, &keyset); err != nil {
		return oidcIdentity{}, fmt.Errorf("fetching JWKS: %w", err)
	}
	var key *rsa.PublicKey
	for _, k := range keyset.Keys {
		if k.Kid != header.KeyID || k.Kty != "RSA" || (k.Use != "" && k.Use != "sig") || (k.Alg != "" && k.Alg != "RS256") {
			continue
		}
		n, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil || len(n) == 0 {
			continue
		}
		e, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil || len(e) == 0 || len(e) > 4 {
			continue
		}
		exponent := new(big.Int).SetBytes(e).Int64()
		if exponent < 3 || exponent > 0x7fffffff || exponent%2 == 0 {
			continue
		}
		pub := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(exponent)}
		if pub.N.BitLen() < 2048 {
			continue
		}
		key = pub
		break
	}
	if key == nil {
		return oidcIdentity{}, errors.New("OIDC signing key not found")
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig); err != nil {
		return oidcIdentity{}, errors.New("ID token signature invalid")
	}
	var claims struct {
		Issuer            string          `json:"iss"`
		Subject           string          `json:"sub"`
		Audience          json.RawMessage `json:"aud"`
		AuthorizedParty   string          `json:"azp"`
		Nonce             string          `json:"nonce"`
		Expires           int64           `json:"exp"`
		Issued            int64           `json:"iat"`
		PreferredUsername string          `json:"preferred_username"`
		oidcAvatarClaims
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return oidcIdentity{}, err
	}
	if strings.TrimSuffix(claims.Issuer, "/") != o.issuer || claims.Subject == "" || claims.PreferredUsername == "" {
		return oidcIdentity{}, errors.New("invalid issuer, subject, or preferred_username")
	}
	if claims.Nonce == "" || subtle.ConstantTimeCompare([]byte(claims.Nonce), []byte(expectedNonce)) != 1 {
		return oidcIdentity{}, errors.New("ID token nonce mismatch")
	}
	now := time.Now().Unix()
	if claims.Expires <= now || claims.Issued > now+60 || claims.Issued <= 0 {
		return oidcIdentity{}, errors.New("ID token expired or has invalid issue time")
	}
	var audString string
	var audiences []string
	if err := json.Unmarshal(claims.Audience, &audString); err == nil {
		audiences = []string{audString}
	} else if err := json.Unmarshal(claims.Audience, &audiences); err != nil {
		return oidcIdentity{}, errors.New("invalid token audience")
	}
	matched := false
	for _, aud := range audiences {
		if aud == o.clientID {
			matched = true
		}
	}
	if !matched || (len(audiences) > 1 && claims.AuthorizedParty != o.clientID) {
		return oidcIdentity{}, errors.New("ID token audience mismatch")
	}
	return oidcIdentity{Subject: claims.Subject, Username: claims.PreferredUsername,
		Avatar: claims.oidcAvatarClaims.avatar(o.issuer)}, nil
}

// Avoid unnecessary Go JWT dependencies; this source uses the already existing
// Navidrome auth system for the final application session.

// UI configuration exposes only whether OIDC is enabled, never its client secret.
func sOIDCEnabled() bool {
	return os.Getenv("OIDC_ISSUER_URL") != "" && os.Getenv("OIDC_CLIENT_ID") != "" && os.Getenv("OIDC_CLIENT_SECRET") != "" && os.Getenv("OIDC_REDIRECT_URL") != ""
}
