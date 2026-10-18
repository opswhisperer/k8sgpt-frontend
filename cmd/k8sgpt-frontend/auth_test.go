package main

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeIdP is a minimal OIDC provider: discovery, JWKS, an authorize endpoint
// that logs the configured user straight in, and a token endpoint that checks
// the client secret and PKCE verifier.
type fakeIdP struct {
	srv *httptest.Server
	key *rsa.PrivateKey

	mu     sync.Mutex
	user   map[string]interface{} // extra ID token claims
	codes  map[string]authReq
	mutate func(claims map[string]interface{})
}

type authReq struct{ nonce, challenge, redirect string }

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &fakeIdP{key: key, codes: map[string]authReq{},
		user: map[string]interface{}{"preferred_username": "alice", "name": "Alice", "groups": []string{"homelab-admins"}}}
	mux := http.NewServeMux()
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	iss := p.srv.URL + "/application/o/k8sgpt/"

	mux.HandleFunc("/application/o/k8sgpt/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer": iss, "authorization_endpoint": p.srv.URL + "/authorize", "token_endpoint": p.srv.URL + "/token",
			"jwks_uri": p.srv.URL + "/jwks", "end_session_endpoint": p.srv.URL + "/end-session",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		b := func(v []byte) string { return base64.RawURLEncoding.EncodeToString(v) }
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"keys": []map[string]string{{
			"kty": "RSA", "kid": "k1", "use": "sig", "alg": "RS256",
			"n": b(key.N.Bytes()), "e": b(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("client_id") != "k8sgpt" || q.Get("code_challenge_method") != "S256" || !strings.Contains(q.Get("scope"), "openid") {
			http.Error(w, "bad authorize request", http.StatusBadRequest)
			return
		}
		code := randString()
		p.mu.Lock()
		p.codes[code] = authReq{nonce: q.Get("nonce"), challenge: q.Get("code_challenge"), redirect: q.Get("redirect_uri")}
		p.mu.Unlock()
		http.Redirect(w, r, q.Get("redirect_uri")+"?code="+code+"&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		id, secret, _ := r.BasicAuth()
		_ = r.ParseForm()
		p.mu.Lock()
		req, ok := p.codes[r.PostForm.Get("code")]
		delete(p.codes, r.PostForm.Get("code"))
		p.mu.Unlock()
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if id != "k8sgpt" || secret != "s3cret" || !ok || r.PostForm.Get("redirect_uri") != req.redirect ||
			base64.RawURLEncoding.EncodeToString(sum[:]) != req.challenge {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		claims := map[string]interface{}{"iss": iss, "sub": "u-1", "aud": "k8sgpt", "exp": time.Now().Add(5 * time.Minute).Unix(),
			"iat": time.Now().Unix(), "nonce": req.nonce}
		p.mu.Lock()
		for k, v := range p.user {
			claims[k] = v
		}
		if p.mutate != nil {
			p.mutate(claims)
		}
		p.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"id_token": signRS256(t, key, "k1", claims), "access_token": "at"})
	})
	return p
}

func (p *fakeIdP) issuer() string { return p.srv.URL + "/application/o/k8sgpt/" }

func jwtSegment(t *testing.T, v interface{}) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func signRS256(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]interface{}) string {
	t.Helper()
	signed := jwtSegment(t, map[string]string{"alg": "RS256", "kid": kid, "typ": "JWT"}) + "." + jwtSegment(t, claims)
	sum := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return signed + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// newAuthServer is the API test server behind an OIDC login, plus a browser
// (cookie jar) that follows redirects through the fake provider.
func newAuthServer(t *testing.T, idp *fakeIdP, groups ...string) (*httptest.Server, *http.Client) {
	t.Helper()
	kube, _ := newFakeKube(t, k8sgptObj("k8sgpt", true), deploymentObj("k8sgpt", 1, 1),
		resultObj("appsweb", "Pod", "apps/web", "crashloop"))
	app := newApp(kube, openStore(t.TempDir()), &Notifier{}, nil, Options{
		Namespace: testNS, PollInterval: time.Minute, NotifyDelay: time.Minute, HealthWindow: time.Hour,
	})
	app.Poll(t.Context())

	mux := http.NewServeMux()
	registerHandlers(mux, app)
	var handler http.Handler = mux
	srv := httptest.NewUnstartedServer(nil)
	auth, err := newAuth(OIDCConfig{Issuer: idp.issuer(), ClientID: "k8sgpt", ClientSecret: "s3cret",
		RedirectURL: "http://" + srv.Listener.Addr().String() + "/auth/callback", Scopes: []string{"profile", "email"}, AllowedGroups: groups})
	if err != nil {
		t.Fatal(err)
	}
	auth.Register(mux)
	srv.Config.Handler = auth.Middleware(handler)
	srv.Start()
	t.Cleanup(srv.Close)

	jar, _ := cookiejar.New(nil)
	return srv, &http.Client{Jar: jar}
}

func get(t *testing.T, c *http.Client, u string) (*http.Response, string) {
	t.Helper()
	resp, err := c.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestOIDCLoginFlow(t *testing.T) {
	idp := newFakeIdP(t)
	srv, browser := newAuthServer(t, idp, "homelab-admins")
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	// Not logged in: the page redirects to the login, the API answers 401, probes stay open.
	resp, _ := get(t, noFollow, srv.URL+"/?tab=rules")
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/auth/login?next=%2F%3Ftab%3Drules" {
		t.Fatalf("anonymous page = %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp, _ := get(t, noFollow, srv.URL+"/api/results"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous API = %d", resp.StatusCode)
	}
	if code, _ := postJSON(t, srv.URL+"/api/ignore", `{"keys":["Pod/apps/web"]}`); code != http.StatusUnauthorized {
		t.Errorf("anonymous POST = %d", code)
	}
	if resp, _ := get(t, noFollow, srv.URL+"/healthz"); resp.StatusCode != http.StatusOK {
		t.Errorf("healthz = %d", resp.StatusCode)
	}

	// The browser goes through the provider and lands back on the page it asked for.
	resp, body := get(t, browser, srv.URL+"/?tab=rules")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "K8sGPT") || resp.Request.URL.RequestURI() != "/?tab=rules" {
		t.Fatalf("after login = %d at %s", resp.StatusCode, resp.Request.URL)
	}
	resp, body = get(t, browser, srv.URL+"/api/results")
	var res resultsResponse
	if err := json.Unmarshal([]byte(body), &res); err != nil || res.User != "alice" {
		t.Fatalf("results user = %q (%v)", res.User, err)
	}

	// Hides record who made them.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/ignore", strings.NewReader(`{"keys":["Pod/apps/web"],"note":"known"}`))
	req.Header.Set("Content-Type", "application/json")
	if resp, err := browser.Do(req); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("ignore: %v %v", resp, err)
	}
	_, body = get(t, browser, srv.URL+"/api/results")
	_ = json.Unmarshal([]byte(body), &res)
	if len(res.Items) != 1 || res.Items[0].Hidden == nil || res.Items[0].Hidden.User != "alice" {
		t.Errorf("hidden = %+v", res.Items[0].Hidden)
	}

	// Logout clears the session and hands over to the provider's end-session endpoint.
	jarClient := &http.Client{Jar: browser.Jar, CheckRedirect: noFollow.CheckRedirect}
	resp, _ = get(t, jarClient, srv.URL+"/auth/logout")
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != idp.srv.URL+"/end-session" {
		t.Errorf("logout = %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp, _ := get(t, jarClient, srv.URL+"/api/results"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("API after logout = %d", resp.StatusCode)
	}
}

func TestOIDCRejectsBadLogins(t *testing.T) {
	cases := []struct {
		name   string
		groups []string
		mutate func(map[string]interface{})
		status int
	}{
		{"not in an allowed group", []string{"other"}, nil, http.StatusForbidden},
		{"wrong nonce", nil, func(c map[string]interface{}) { c["nonce"] = "x" }, http.StatusBadGateway},
		{"wrong audience", nil, func(c map[string]interface{}) { c["aud"] = "someone-else" }, http.StatusBadGateway},
		{"wrong issuer", nil, func(c map[string]interface{}) { c["iss"] = "https://evil.invalid/" }, http.StatusBadGateway},
		{"expired", nil, func(c map[string]interface{}) { c["exp"] = time.Now().Add(-time.Hour).Unix() }, http.StatusBadGateway},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			idp := newFakeIdP(t)
			idp.mutate = tc.mutate
			srv, browser := newAuthServer(t, idp, tc.groups...)
			resp, _ := get(t, browser, srv.URL+"/")
			if resp.StatusCode != tc.status {
				t.Errorf("login = %d, want %d", resp.StatusCode, tc.status)
			}
			if resp, _ := get(t, browser, srv.URL+"/api/results"); resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("API after failed login = %d", resp.StatusCode)
			}
		})
	}
}

func TestOIDCCallbackWithoutLoginState(t *testing.T) {
	idp := newFakeIdP(t)
	srv, browser := newAuthServer(t, idp)
	// A callback this browser did not start (e.g. login CSRF) is refused, not looped.
	resp, _ := get(t, browser, srv.URL+"/auth/callback?code=abc&state=xyz")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("stray callback = %d", resp.StatusCode)
	}
}

func TestSessionCookieTampering(t *testing.T) {
	a, err := newAuth(OIDCConfig{Issuer: "https://idp.example.invalid/", ClientID: "c", ClientSecret: "s", RedirectURL: "https://dash.example.invalid/auth/callback"})
	if err != nil {
		t.Fatal(err)
	}
	if !a.secure {
		t.Error("https redirect URL should give Secure cookies")
	}
	good := a.seal(Session{Sub: "u", User: "alice", Exp: time.Now().Add(time.Hour).Unix()})
	expired := a.seal(Session{Sub: "u", User: "alice", Exp: time.Now().Add(-time.Second).Unix()})
	payload, sig, _ := strings.Cut(good, ".")
	forged := jwtSegment(t, Session{Sub: "u", User: "mallory", Exp: time.Now().Add(time.Hour).Unix()}) + "." + sig
	other, _ := newAuth(OIDCConfig{Issuer: "https://idp.example.invalid/", ClientID: "c", ClientSecret: "rotated", RedirectURL: "https://dash.example.invalid/auth/callback"})

	for name, tc := range map[string]struct {
		a     *Auth
		value string
		ok    bool
	}{
		"valid": {a, good, true}, "expired": {a, expired, false}, "forged payload": {a, forged, false},
		"no signature": {a, payload, false}, "rotated secret": {other, good, false},
	} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: tc.value})
		if got := tc.a.session(r) != nil; got != tc.ok {
			t.Errorf("%s: session valid = %v", name, got)
		}
	}
}

func TestVerifySignatureES256(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ecdh, _ := key.PublicKey.ECDH()
	pt := ecdh.Bytes() // 0x04 || X || Y
	j := jwk{Kty: "EC", Crv: "P-256", X: base64.RawURLEncoding.EncodeToString(pt[1:33]), Y: base64.RawURLEncoding.EncodeToString(pt[33:])}
	pub, err := j.publicKey()
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("header.payload")
	sum := sha256.Sum256(msg)
	r, s, _ := ecdsa.Sign(rand.Reader, key, sum[:])
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	if err := verifySignature("ES256", pub, msg, sig); err != nil {
		t.Errorf("valid ES256: %v", err)
	}
	if verifySignature("ES256", pub, []byte("tampered"), sig) == nil {
		t.Error("tampered ES256 verified")
	}
	if verifySignature("HS256", pub, msg, sig) == nil || verifySignature("none", pub, msg, nil) == nil {
		t.Error("HS256/none accepted")
	}
	if verifySignature("RS256", pub, msg, sig) == nil {
		t.Error("RS256 accepted with an EC key")
	}
}

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"/?tab=rules": "/?tab=rules", "": "/", "https://evil.invalid/": "/", "//evil.invalid/": "/",
		"/\\evil.invalid": "/", "/auth/logout": "/",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNewAuthValidation(t *testing.T) {
	base := OIDCConfig{Issuer: "https://idp.example.invalid/", ClientID: "c", ClientSecret: "s", RedirectURL: "https://dash.example.invalid/auth/callback"}
	if _, err := newAuth(base); err != nil {
		t.Fatalf("valid config: %v", err)
	}
	for name, mod := range map[string]func(*OIDCConfig){
		"no secret":         func(c *OIDCConfig) { c.ClientSecret = "" },
		"relative redirect": func(c *OIDCConfig) { c.RedirectURL = "/auth/callback" },
		"root redirect":     func(c *OIDCConfig) { c.RedirectURL = "https://dash.example.invalid/" },
		"bad issuer":        func(c *OIDCConfig) { c.Issuer = "idp.example.invalid" },
	} {
		c := base
		mod(&c)
		if _, err := newAuth(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
