package main

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	_ "crypto/sha512" // SHA-384/512 for RS384/RS512/ES384/ES512
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// OIDC login (authorization code flow with PKCE) for the dashboard and API.
// The session is a signed cookie; nothing about logins is persisted.

const (
	sessionCookie = "k8sgpt_session"
	stateCookie   = "k8sgpt_oidc"
	sessionTTL    = 12 * time.Hour
	stateTTL      = 10 * time.Minute
	maxIdPBody    = 1 << 20
)

// OIDCConfig is the OIDC client configuration. Login is off when Issuer is empty.
type OIDCConfig struct {
	Issuer        string
	ClientID      string
	ClientSecret  string
	RedirectURL   string   // https://dashboard.example.com/auth/callback
	Scopes        []string // "openid" is always requested
	AllowedGroups []string // empty = any user the IdP lets through
}

// Auth guards the handlers behind an OIDC login.
type Auth struct {
	cfg      OIDCConfig
	key      []byte // HMAC key for the session and state cookies
	secure   bool   // cookies get the Secure flag (redirect URL is https)
	callback string // path of RedirectURL
	client   *http.Client
	now      func() time.Time

	mu     sync.Mutex
	disc   *oidcDiscovery
	keys   map[string]crypto.PublicKey
	keysAt time.Time
}

type oidcDiscovery struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
	EndSessionEndpoint    string `json:"end_session_endpoint"`
}

// Session is the signed content of the session cookie.
type Session struct {
	Sub  string `json:"sub"`
	User string `json:"user"` // preferred_username, email, name or sub
	Name string `json:"name,omitempty"`
	Exp  int64  `json:"exp"`
}

type ctxKey struct{}

// sessionFrom returns the logged-in user of a request, or nil when login is off.
func sessionFrom(r *http.Request) *Session {
	s, _ := r.Context().Value(ctxKey{}).(*Session)
	return s
}

// userOf names the logged-in user for records ("" when login is off).
func userOf(r *http.Request) string {
	if s := sessionFrom(r); s != nil {
		return s.User
	}
	return ""
}

func newAuth(cfg OIDCConfig) (*Auth, error) {
	if cfg.ClientID == "" || cfg.ClientSecret == "" {
		return nil, errors.New("OIDC needs a client ID and client secret")
	}
	if _, err := url.Parse(cfg.Issuer); err != nil || !strings.HasPrefix(cfg.Issuer, "http") {
		return nil, fmt.Errorf("OIDC issuer %q is not a URL", cfg.Issuer)
	}
	ru, err := url.Parse(cfg.RedirectURL)
	if err != nil || ru.Host == "" || (ru.Scheme != "https" && ru.Scheme != "http") {
		return nil, fmt.Errorf("OIDC redirect URL %q is not an absolute URL (set OIDC_REDIRECT_URL or UI_URL)", cfg.RedirectURL)
	}
	// The cookie key is derived from the client secret, so sessions survive
	// restarts and rotating the secret logs everyone out.
	m := hmac.New(sha256.New, []byte(cfg.ClientSecret))
	m.Write([]byte("k8sgpt-frontend session v1"))
	callback := ru.EscapedPath()
	if callback == "" || callback == "/" || strings.HasPrefix(callback, "/api/") {
		return nil, fmt.Errorf("OIDC redirect URL %q needs a callback path such as /auth/callback", cfg.RedirectURL)
	}
	return &Auth{
		cfg: cfg, key: m.Sum(nil), secure: ru.Scheme == "https", callback: callback,
		client: &http.Client{Timeout: 10 * time.Second},
		now:    time.Now,
	}, nil
}

// Register adds the login, callback and logout endpoints.
func (a *Auth) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/login", a.login)
	mux.HandleFunc("GET "+a.callback, a.handleCallback)
	mux.HandleFunc("GET /auth/logout", a.logout)
}

// Middleware lets probes and the login endpoints through and requires a
// session for everything else: pages redirect to the login, API calls get 401.
func (a *Auth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == "/healthz" || p == "/readyz" || p == a.callback || strings.HasPrefix(p, "/auth/") {
			next.ServeHTTP(w, r)
			return
		}
		s := a.session(r)
		if s == nil {
			if !strings.HasPrefix(p, "/api/") && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
				http.Redirect(w, r, "/auth/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
				return
			}
			writeError(w, http.StatusUnauthorized, "login required")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, s)))
	})
}

func (a *Auth) session(r *http.Request) *Session {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil
	}
	var s Session
	if !a.unseal(c.Value, &s) || s.Sub == "" || a.now().Unix() >= s.Exp {
		return nil
	}
	return &s
}

type loginState struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Next     string `json:"r"`
	Exp      int64  `json:"e"`
}

func (a *Auth) login(w http.ResponseWriter, r *http.Request) {
	d, err := a.discover(r.Context())
	if err != nil {
		log.Printf("oidc: %v", err)
		a.fail(w, http.StatusBadGateway, "The identity provider cannot be reached right now.")
		return
	}
	st := loginState{State: randString(), Nonce: randString(), Verifier: randString() + randString(),
		Next: safeNext(r.URL.Query().Get("next")), Exp: a.now().Add(stateTTL).Unix()}
	a.setCookie(w, stateCookie, a.seal(st), stateTTL)

	challenge := sha256.Sum256([]byte(st.Verifier))
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {a.cfg.ClientID},
		"redirect_uri":          {a.cfg.RedirectURL},
		"scope":                 {strings.Join(a.scopes(), " ")},
		"state":                 {st.State},
		"nonce":                 {st.Nonce},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"code_challenge_method": {"S256"},
	}
	sep := "?"
	if strings.Contains(d.AuthorizationEndpoint, "?") {
		sep = "&"
	}
	http.Redirect(w, r, d.AuthorizationEndpoint+sep+q.Encode(), http.StatusFound)
}

func (a *Auth) handleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var st loginState
	c, err := r.Cookie(stateCookie)
	if err != nil || !a.unseal(c.Value, &st) || a.now().Unix() >= st.Exp {
		// Not redirected to the login again: with cookies blocked that would loop.
		a.fail(w, http.StatusBadRequest, "The login attempt expired or was started in another browser.")
		return
	}
	a.clearCookie(w, stateCookie)
	if e := q.Get("error"); e != "" {
		msg := e
		if d := q.Get("error_description"); d != "" {
			msg += ": " + d
		}
		a.fail(w, http.StatusForbidden, "The identity provider refused the login ("+truncate(scrubMessage(msg), 300)+").")
		return
	}
	if !hmac.Equal([]byte(q.Get("state")), []byte(st.State)) {
		a.fail(w, http.StatusBadRequest, "The login response does not match this browser's login attempt.")
		return
	}
	s, err := a.exchange(r.Context(), q.Get("code"), st)
	if err != nil {
		log.Printf("oidc: login failed: %s", scrubMessage(err.Error()))
		var denied deniedError
		if errors.As(err, &denied) {
			a.fail(w, http.StatusForbidden, string(denied))
			return
		}
		a.fail(w, http.StatusBadGateway, "Login failed. The details are in the server log.")
		return
	}
	a.setCookie(w, sessionCookie, a.seal(s), sessionTTL)
	log.Printf("oidc: login %s", s.User)
	http.Redirect(w, r, st.Next, http.StatusFound)
}

func (a *Auth) logout(w http.ResponseWriter, r *http.Request) {
	a.clearCookie(w, sessionCookie)
	target := "/"
	if d, err := a.discover(r.Context()); err == nil && d.EndSessionEndpoint != "" {
		target = d.EndSessionEndpoint
	}
	http.Redirect(w, r, target, http.StatusFound)
}

// deniedError is a login the IdP accepted but this dashboard does not.
type deniedError string

func (e deniedError) Error() string { return string(e) }

type idClaims struct {
	Iss               string   `json:"iss"`
	Sub               string   `json:"sub"`
	Aud               audience `json:"aud"`
	Azp               string   `json:"azp"`
	Exp               int64    `json:"exp"`
	Nonce             string   `json:"nonce"`
	Name              string   `json:"name"`
	PreferredUsername string   `json:"preferred_username"`
	Email             string   `json:"email"`
	Groups            []string `json:"groups"`
}

// audience is a JWT "aud": a string or a list of strings.
type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var one string
	if json.Unmarshal(b, &one) == nil {
		*a = audience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*a = many
	return nil
}

func (a audience) contains(s string) bool {
	for _, v := range a {
		if v == s {
			return true
		}
	}
	return false
}

// exchange redeems the code, verifies the ID token and checks group access.
func (a *Auth) exchange(ctx context.Context, code string, st loginState) (Session, error) {
	if code == "" {
		return Session{}, errors.New("callback without a code")
	}
	d, err := a.discover(ctx)
	if err != nil {
		return Session{}, err
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {a.cfg.RedirectURL},
		"code_verifier": {st.Verifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return Session{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth(url.QueryEscape(a.cfg.ClientID), url.QueryEscape(a.cfg.ClientSecret))
	var tok struct {
		IDToken     string `json:"id_token"`
		AccessToken string `json:"access_token"`
	}
	if err := a.doJSON(req, &tok); err != nil {
		return Session{}, fmt.Errorf("token endpoint: %w", err)
	}
	if tok.IDToken == "" {
		return Session{}, errors.New("token response has no id_token (is the openid scope allowed?)")
	}
	cl, err := a.verify(ctx, d, tok.IDToken)
	if err != nil {
		return Session{}, fmt.Errorf("id_token: %w", err)
	}
	if !hmac.Equal([]byte(cl.Nonce), []byte(st.Nonce)) {
		return Session{}, errors.New("id_token: nonce does not match")
	}
	if len(a.cfg.AllowedGroups) > 0 {
		groups := cl.Groups
		if groups == nil && d.UserinfoEndpoint != "" && tok.AccessToken != "" {
			groups, err = a.userinfoGroups(ctx, d, tok.AccessToken)
			if err != nil {
				return Session{}, fmt.Errorf("userinfo: %w", err)
			}
		}
		if !anyIn(groups, a.cfg.AllowedGroups) {
			return Session{}, deniedError(fmt.Sprintf("%s is not in a group allowed to use this dashboard.", displayUser(cl)))
		}
	}
	return Session{Sub: cl.Sub, User: displayUser(cl), Name: cl.Name, Exp: a.now().Add(sessionTTL).Unix()}, nil
}

func displayUser(cl *idClaims) string {
	for _, v := range []string{cl.PreferredUsername, cl.Email, cl.Name, cl.Sub} {
		if v != "" {
			return truncate(v, 100)
		}
	}
	return "unknown"
}

func anyIn(have, want []string) bool {
	for _, h := range have {
		for _, w := range want {
			if h == w {
				return true
			}
		}
	}
	return false
}

func (a *Auth) userinfoGroups(ctx context.Context, d *oidcDiscovery, accessToken string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.UserinfoEndpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	var ui struct {
		Groups []string `json:"groups"`
	}
	err = a.doJSON(req, &ui)
	return ui.Groups, err
}

// verify checks the ID token's signature against the provider's JWKS and its
// issuer, audience and expiry. The nonce is checked by the caller.
func (a *Auth) verify(ctx context.Context, d *oidcDiscovery, raw string) (*idClaims, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, errors.New("not a JWT")
	}
	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodeSegment(parts[0], &hdr); err != nil {
		return nil, fmt.Errorf("header: %w", err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, errors.New("signature is not base64url")
	}
	key, err := a.publicKey(ctx, d, hdr.Kid)
	if err != nil {
		return nil, err
	}
	if err := verifySignature(hdr.Alg, key, []byte(parts[0]+"."+parts[1]), sig); err != nil {
		return nil, err
	}
	var cl idClaims
	if err := decodeSegment(parts[1], &cl); err != nil {
		return nil, fmt.Errorf("claims: %w", err)
	}
	if cl.Iss != d.Issuer {
		return nil, fmt.Errorf("issuer %q, want %q", cl.Iss, d.Issuer)
	}
	if !cl.Aud.contains(a.cfg.ClientID) || (len(cl.Aud) > 1 && cl.Azp != a.cfg.ClientID) {
		return nil, fmt.Errorf("audience %v does not name client %q", []string(cl.Aud), a.cfg.ClientID)
	}
	if cl.Exp == 0 || a.now().Add(-time.Minute).Unix() >= cl.Exp {
		return nil, errors.New("expired")
	}
	if cl.Sub == "" {
		return nil, errors.New("no sub claim")
	}
	return &cl, nil
}

func verifySignature(alg string, key crypto.PublicKey, signed, sig []byte) error {
	var h crypto.Hash
	switch alg {
	case "RS256", "ES256":
		h = crypto.SHA256
	case "RS384", "ES384":
		h = crypto.SHA384
	case "RS512", "ES512":
		h = crypto.SHA512
	default:
		// HS256 (signed with the client secret) and "none" are refused.
		return fmt.Errorf("unsupported signing algorithm %q; give the provider an RSA or EC signing key", alg)
	}
	hasher := h.New()
	hasher.Write(signed)
	digest := hasher.Sum(nil)
	switch k := key.(type) {
	case *rsa.PublicKey:
		if alg[0] != 'R' {
			return fmt.Errorf("%s token signed with an RSA key", alg)
		}
		if rsa.VerifyPKCS1v15(k, h, digest, sig) != nil {
			return errors.New("bad signature")
		}
	case *ecdsa.PublicKey:
		size := (k.Curve.Params().BitSize + 7) / 8
		if alg[0] != 'E' || len(sig) != 2*size {
			return fmt.Errorf("%s signature does not fit the EC key", alg)
		}
		r, s := new(big.Int).SetBytes(sig[:size]), new(big.Int).SetBytes(sig[size:])
		if !ecdsa.Verify(k, digest, r, s) {
			return errors.New("bad signature")
		}
	default:
		return errors.New("unsupported key type")
	}
	return nil
}

// publicKey finds a signing key by kid, refetching the JWKS once when the kid
// is unknown (key rotation) but at most once a minute.
func (a *Auth) publicKey(ctx context.Context, d *oidcDiscovery, kid string) (crypto.PublicKey, error) {
	a.mu.Lock()
	keys, at := a.keys, a.keysAt
	a.mu.Unlock()
	if k := pickKey(keys, kid); k != nil {
		return k, nil
	}
	if keys != nil && a.now().Sub(at) < time.Minute {
		return nil, fmt.Errorf("no signing key %q in the provider's JWKS", kid)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.JWKSURI, nil)
	if err != nil {
		return nil, err
	}
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := a.doJSON(req, &set); err != nil {
		return nil, fmt.Errorf("jwks: %w", err)
	}
	keys = map[string]crypto.PublicKey{}
	for _, j := range set.Keys {
		if j.Use != "" && j.Use != "sig" {
			continue
		}
		if k, err := j.publicKey(); err == nil {
			keys[j.Kid] = k
		}
	}
	a.mu.Lock()
	a.keys, a.keysAt = keys, a.now()
	a.mu.Unlock()
	if k := pickKey(keys, kid); k != nil {
		return k, nil
	}
	return nil, fmt.Errorf("no signing key %q in the provider's JWKS", kid)
}

func pickKey(keys map[string]crypto.PublicKey, kid string) crypto.PublicKey {
	if k, ok := keys[kid]; ok {
		return k
	}
	if kid == "" && len(keys) == 1 {
		for _, k := range keys {
			return k
		}
	}
	return nil
}

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

func (j jwk) publicKey() (crypto.PublicKey, error) {
	b := func(s string) []byte { v, _ := base64.RawURLEncoding.DecodeString(s); return v }
	switch j.Kty {
	case "RSA":
		n, e := b(j.N), b(j.E)
		if len(n) == 0 || len(e) == 0 || len(e) > 4 {
			return nil, errors.New("bad RSA key")
		}
		return &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}, nil
	case "EC":
		curves := map[string]elliptic.Curve{"P-256": elliptic.P256(), "P-384": elliptic.P384(), "P-521": elliptic.P521()}
		c, ok := curves[j.Crv]
		if !ok {
			return nil, errors.New("unsupported curve")
		}
		size := (c.Params().BitSize + 7) / 8
		x, y := b(j.X), b(j.Y)
		if len(x) == 0 || len(x) > size || len(y) == 0 || len(y) > size {
			return nil, errors.New("bad EC key")
		}
		pt := make([]byte, 1+2*size)
		pt[0] = 4
		copy(pt[1+size-len(x):1+size], x)
		copy(pt[1+2*size-len(y):], y)
		return ecdsa.ParseUncompressedPublicKey(c, pt)
	}
	return nil, errors.New("unsupported key type")
}

// discover fetches and caches the provider's metadata.
func (a *Auth) discover(ctx context.Context) (*oidcDiscovery, error) {
	a.mu.Lock()
	d := a.disc
	a.mu.Unlock()
	if d != nil {
		return d, nil
	}
	u := strings.TrimSuffix(a.cfg.Issuer, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	d = &oidcDiscovery{}
	if err := a.doJSON(req, d); err != nil {
		return nil, fmt.Errorf("discovery: %w", err)
	}
	if strings.TrimSuffix(d.Issuer, "/") != strings.TrimSuffix(a.cfg.Issuer, "/") {
		return nil, fmt.Errorf("discovery: provider says issuer %q, configured %q", d.Issuer, a.cfg.Issuer)
	}
	if d.AuthorizationEndpoint == "" || d.TokenEndpoint == "" || d.JWKSURI == "" {
		return nil, errors.New("discovery: authorization, token or jwks endpoint missing")
	}
	a.mu.Lock()
	a.disc = d
	a.mu.Unlock()
	return d, nil
}

func (a *Auth) doJSON(req *http.Request, v interface{}) error {
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxIdPBody))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", resp.Status, truncate(string(body), 300))
	}
	return json.Unmarshal(body, v)
}

func (a *Auth) scopes() []string {
	out := []string{"openid"}
	for _, s := range a.cfg.Scopes {
		if s != "" && s != "openid" {
			out = append(out, s)
		}
	}
	return out
}

// seal and unseal sign cookie values: base64url(JSON) "." base64url(HMAC).
func (a *Auth) seal(v interface{}) string {
	b, _ := json.Marshal(v)
	p := base64.RawURLEncoding.EncodeToString(b)
	m := hmac.New(sha256.New, a.key)
	m.Write([]byte(p))
	return p + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (a *Auth) unseal(s string, v interface{}) bool {
	p, sig, ok := strings.Cut(s, ".")
	if !ok {
		return false
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return false
	}
	m := hmac.New(sha256.New, a.key)
	m.Write([]byte(p))
	if !hmac.Equal(got, m.Sum(nil)) {
		return false
	}
	return decodeSegment(p, v) == nil
}

func (a *Auth) setCookie(w http.ResponseWriter, name, value string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: int(ttl.Seconds()),
		HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteLaxMode})
}

func (a *Auth) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteLaxMode})
}

var failPage = template.Must(template.New("fail").Parse(`<!doctype html>
<meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Login failed</title>
<style>body{font:15px/1.5 system-ui,sans-serif;max-width:36em;margin:15vh auto;padding:0 16px;color:#1f2328;background:#fff}
@media (prefers-color-scheme:dark){body{color:#e6edf3;background:#0d1117}a{color:#58a6ff}}</style>
<h1>Login failed</h1><p>{{.}}</p><p><a href="/auth/login">Try again</a> · <a href="/auth/logout">Sign out</a></p>`))

func (a *Auth) fail(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = failPage.Execute(w, msg)
}

func decodeSegment(s string, v interface{}) error {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func randString() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// safeNext keeps post-login redirects on this site.
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") ||
		strings.HasPrefix(next, "/auth/") {
		return "/"
	}
	return next
}
