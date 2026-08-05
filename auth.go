package main

// Login via OpenID Connect, with docovia as the relying party and something
// like Pocket ID as the provider. The scope is deliberately the easy side of
// OAuth: one known issuer, the authorization-code flow with PKCE, and claims
// taken from the userinfo endpoint over the same TLS channel the code exchange
// used — which is what lets this file contain no JWT code at all. Verifying
// token signatures is where relying parties historically grow security bugs,
// and a client that got its tokens directly from the token endpoint may skip
// it, because the channel itself is the proof of origin.
//
// Sessions are stateless: an HMAC-signed cookie carrying who you are and until
// when. There is no session table, nothing to migrate, and deleting
// <data>/session.key signs everyone out at once. Who may log in is the
// issuer's decision (Pocket ID's allowed-groups per app), not ours — any
// identity it vouches for is in, which is the right shape for a family
// archive: the user list lives where the passkeys live.

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	sessionCookie = "docovia_session"
	loginCookie   = "docovia_login"

	// A month, renewed whenever a request arrives past the halfway mark, so in
	// practice a session only expires after two weeks of not being used at
	// all. Long on purpose: the cost of expiry is a passkey tap, but the
	// archive is visited rarely enough that a short session would mean logging
	// in nearly every time.
	sessionTTL = 30 * 24 * time.Hour

	// How long a login has to complete once it has been sent to the issuer.
	// Generous for a passkey, short enough that an abandoned attempt's state
	// is not sitting in a cookie for days.
	loginTTL = 5 * time.Minute
)

// Auth is the OIDC client and the session authority — one struct, because the
// second exists only as the outcome of the first.
type Auth struct {
	issuer   string
	clientID string
	// secret is empty for a public client, which is the intended registration:
	// PKCE protects the code exchange, and a secret baked into a self-hosted
	// binary's config guards little. Supported anyway, because honouring a
	// confidential registration costs nothing but this field.
	secret string
	key    []byte
	client *http.Client

	mu   sync.Mutex
	disc *discovery
}

func NewAuth(issuer, clientID, secret string, key []byte) *Auth {
	return &Auth{
		issuer:   strings.TrimSuffix(issuer, "/"),
		clientID: clientID,
		secret:   secret,
		key:      key,
		client:   &http.Client{Timeout: 10 * time.Second},
	}
}

// loadSessionKey loads or creates the HMAC key that makes cookies trustworthy.
// In the data directory rather than /etc because it belongs to the archive:
// restore the archive somewhere else and existing sessions still verify.
func loadSessionKey(dataDir string) ([]byte, error) {
	path := filepath.Join(dataDir, "session.key")
	if b, err := os.ReadFile(path); err == nil {
		key, err := hex.DecodeString(strings.TrimSpace(string(b)))
		if err != nil || len(key) != 32 {
			// Refuse rather than regenerate: silently replacing a corrupt key
			// would sign everyone out and destroy the evidence of what broke.
			return nil, fmt.Errorf("%s exists but is not a 64-character hex key", path)
		}
		return key, nil
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(key)+"\n"), 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

// --- cookies ---------------------------------------------------------------

// session is what being signed in means: an identity the issuer vouched for.
// Email is display material — the tooltip on the name — never an authority;
// the subject is the identity.
type session struct {
	Sub      string `json:"sub"`
	Name     string `json:"name"`
	Email    string `json:"email,omitempty"`
	IssuedAt int64  `json:"iat"`
	Expires  int64  `json:"exp"`
}

// loginFlow is the state carried across the round trip to the issuer: proof
// the callback answers a login this browser started (State), the PKCE secret
// the token exchange must present (Verifier), and where to land afterwards.
type loginFlow struct {
	State    string `json:"state"`
	Verifier string `json:"verifier"`
	Next     string `json:"next"`
	Expires  int64  `json:"exp"`
}

// sign turns a payload into base64(json) + "." + base64(hmac). Both cookies
// use it, so tampering with either is the same non-starter.
func (auth *Auth) sign(v any) string {
	payload, _ := json.Marshal(v)
	body := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, auth.key)
	mac.Write([]byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (auth *Auth) verify(value string, into any) bool {
	body, sig, ok := strings.Cut(value, ".")
	if !ok {
		return false
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, auth.key)
	mac.Write([]byte(body))
	if !hmac.Equal(got, mac.Sum(nil)) {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return false
	}
	return json.Unmarshal(payload, into) == nil
}

// session reads the cookie and answers who this request is, if anyone.
func (auth *Auth) session(r *http.Request) (session, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return session{}, false
	}
	var s session
	if !auth.verify(c.Value, &s) || time.Now().Unix() >= s.Expires {
		return session{}, false
	}
	return s, true
}

func (auth *Auth) setSession(w http.ResponseWriter, r *http.Request, s session) {
	auth.setCookie(w, r, sessionCookie, auth.sign(s), sessionTTL)
}

func (auth *Auth) setCookie(w http.ResponseWriter, r *http.Request, name, value string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: value, Path: "/",
		MaxAge:   int(ttl.Seconds()),
		HttpOnly: true,
		// Lax rather than Strict: the callback is a cross-site navigation from
		// the issuer, and Strict would drop the login cookie on exactly the
		// request that needs it. Lax still keeps cookies off cross-site POSTs,
		// which is the CSRF case that matters.
		SameSite: http.SameSiteLaxMode,
		Secure:   httpsRequest(r),
	})
}

func clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, Path: "/", MaxAge: -1, HttpOnly: true})
}

// httpsRequest decides the cookie's Secure flag from how this request actually
// arrived, because the same binary serves plain http on a LAN and https behind
// a proxy, and a Secure cookie on plain http is a cookie the browser never
// sends back — which presents as a login loop, not as an error.
func httpsRequest(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// --- the flow --------------------------------------------------------------

// discovery is the part of the issuer's metadata this client uses. The tags
// are the wire keys of the issuer's document — theirs to define, not ours to
// rename — and this struct is the only place they appear; everywhere else
// these are AuthURL, TokenURL, UserinfoURL.
type discovery struct {
	Issuer      string `json:"issuer"`
	AuthURL     string `json:"authorization_endpoint"`
	TokenURL    string `json:"token_endpoint"`
	UserinfoURL string `json:"userinfo_endpoint"`
	// EndSessionURL is optional in the spec, so it is allowed to be absent —
	// logout then falls back to the issuer's own page.
	EndSessionURL string `json:"end_session_endpoint"`
}

// discover fetches the issuer's endpoints on first use and keeps them. First
// use rather than boot: in a compose file the issuer's container may come up
// after this one, and refusing to start over an ordering nobody controls would
// be the wrong failure — nobody can log in yet, but nobody could anyway.
func (auth *Auth) discover(ctx context.Context) (*discovery, error) {
	auth.mu.Lock()
	defer auth.mu.Unlock()
	if auth.disc != nil {
		return auth.disc, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		auth.issuer+"/.well-known/openid-configuration", nil)
	if err != nil {
		return nil, err
	}
	resp, err := auth.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("discovery: %s from %s", resp.Status, req.URL)
	}
	var d discovery
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&d); err != nil {
		return nil, fmt.Errorf("discovery: %w", err)
	}
	// The document names its own issuer, and it has to be the one we asked
	// for: a mismatch means a proxy or a typo is answering for someone else,
	// and every identity from there would be vouched for by the wrong party.
	if strings.TrimSuffix(d.Issuer, "/") != auth.issuer {
		return nil, fmt.Errorf("discovery: document says issuer %q, configured %q", d.Issuer, auth.issuer)
	}
	if d.AuthURL == "" || d.TokenURL == "" || d.UserinfoURL == "" {
		return nil, errors.New("discovery: document is missing an endpoint this client needs")
	}
	auth.disc = &d
	return auth.disc, nil
}

func (auth *Auth) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/login", auth.handleLogin)
	mux.HandleFunc("GET /auth/callback", auth.handleCallback)
	mux.HandleFunc("POST /auth/logout", auth.handleLogout)
}

func (auth *Auth) handleLogin(w http.ResponseWriter, r *http.Request) {
	disc, err := auth.discover(r.Context())
	if err != nil {
		logf("login: %v", err)
		http.Error(w, "the login provider is not reachable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	state, err1 := randomToken(16)
	verifier, err2 := randomToken(32)
	if err1 != nil || err2 != nil {
		http.Error(w, "no randomness", http.StatusInternalServerError)
		return
	}
	auth.setCookie(w, r, loginCookie, auth.sign(loginFlow{
		State: state, Verifier: verifier,
		Next:    safeNext(r.URL.Query().Get("next")),
		Expires: time.Now().Add(loginTTL).Unix(),
	}), loginTTL)

	challenge := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {auth.clientID},
		"redirect_uri":          {redirectURI(r)},
		"scope":                 {"openid profile email"},
		"state":                 {state},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"code_challenge_method": {"S256"},
	}
	http.Redirect(w, r, disc.AuthURL+"?"+q.Encode(), http.StatusSeeOther)
}

func (auth *Auth) handleCallback(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(loginCookie)
	// The flow cookie is single-use whatever happens next: a failed callback
	// should start a fresh login, never be retried against the same state.
	clearCookie(w, loginCookie)

	var flow loginFlow
	if err != nil || !auth.verify(c.Value, &flow) || time.Now().Unix() >= flow.Expires {
		http.Error(w, "login expired or was not started here — go back and try again", http.StatusBadRequest)
		return
	}
	if msg := r.URL.Query().Get("error"); msg != "" {
		// The issuer declining is a normal outcome — wrong group, cancelled
		// passkey — and its own words beat a generic failure.
		http.Error(w, "the login provider refused: "+msg+" "+r.URL.Query().Get("error_description"), http.StatusForbidden)
		return
	}
	if !hmac.Equal([]byte(r.URL.Query().Get("state")), []byte(flow.State)) {
		http.Error(w, "login state mismatch — go back and try again", http.StatusBadRequest)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "the login provider sent no code", http.StatusBadRequest)
		return
	}

	who, err := auth.exchange(r, code, flow.Verifier)
	if err != nil {
		logf("login: %v", err)
		http.Error(w, "completing login: "+err.Error(), http.StatusBadGateway)
		return
	}
	now := time.Now()
	auth.setSession(w, r, session{
		Sub: who.Sub, Name: who.displayName(), Email: who.Email,
		IssuedAt: now.Unix(), Expires: now.Add(sessionTTL).Unix(),
	})
	logf("login: %s (%s)", who.displayName(), who.Sub)
	http.Redirect(w, r, flow.Next, http.StatusSeeOther)
}

func (auth *Auth) handleLogout(w http.ResponseWriter, r *http.Request) {
	clearCookie(w, sessionCookie)
	// On to the issuer, not back to "/". Landing on our own front page meant
	// an immediate login redirect, which the issuer's still-alive session
	// answered without asking — sign out, blink, signed back in. The
	// end-session endpoint ends the session that was doing that; an issuer
	// without one still gets the browser, which is at least an honest place
	// to stop. No post_logout_redirect_uri on purpose: coming back is the
	// bug this fixes.
	dest := auth.issuer
	if disc, err := auth.discover(r.Context()); err == nil && disc.EndSessionURL != "" {
		dest = disc.EndSessionURL + "?client_id=" + url.QueryEscape(auth.clientID)
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

type claims struct {
	Sub               string `json:"sub"`
	Name              string `json:"name"`
	PreferredUsername string `json:"preferred_username"`
	Email             string `json:"email"`
}

// displayName is what the topbar shows: the most human of whatever the
// issuer filled in, falling back to the one field it must fill.
func (c claims) displayName() string {
	for _, s := range []string{c.Name, c.PreferredUsername, c.Email} {
		if s != "" {
			return s
		}
	}
	return c.Sub
}

// exchange turns the one-time code into an identity: code → token endpoint →
// access token → userinfo → claims. Every hop is a direct conversation with
// the issuer over its own TLS, which is why no signature checking appears
// anywhere in this file.
func (auth *Auth) exchange(r *http.Request, code, verifier string) (*claims, error) {
	disc, err := auth.discover(r.Context())
	if err != nil {
		return nil, err
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI(r)},
		"client_id":     {auth.clientID},
		"code_verifier": {verifier},
	}
	if auth.secret != "" {
		form.Set("client_secret", auth.secret)
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, disc.TokenURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := auth.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token endpoint: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		// The issuer's error JSON is short and names the actual problem —
		// invalid_client, invalid_grant. Tokens only appear in successes, so
		// an error body is safe to repeat out loud.
		return nil, fmt.Errorf("token endpoint: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &tok); err != nil || tok.AccessToken == "" {
		return nil, errors.New("token endpoint: no access token in reply")
	}

	req, err = http.NewRequestWithContext(r.Context(), http.MethodGet, disc.UserinfoURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	resp, err = auth.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("userinfo: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("userinfo: %s", resp.Status)
	}
	var who claims
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&who); err != nil {
		return nil, fmt.Errorf("userinfo: %w", err)
	}
	if who.Sub == "" {
		return nil, errors.New("userinfo: no subject")
	}
	return &who, nil
}

// redirectURI is where the issuer sends the browser back, derived from the
// request so the same binary works on localhost, a LAN address and behind a
// TLS proxy without being told which it is. The issuer only honours values
// registered for the client, so this derivation cannot send anyone anywhere
// the operator did not already approve.
func redirectURI(r *http.Request) string {
	scheme := "http"
	if httpsRequest(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/auth/callback"
}

// safeNext keeps the post-login redirect inside this site. The value rides in
// a signed cookie, but it starts life as a query parameter anyone can mint,
// and an open redirect off a login page is the classic phishing gift.
func safeNext(next string) string {
	if next == "" || len(next) > 2048 ||
		!strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") ||
		strings.ContainsAny(next, "\\\r\n") {
		return "/"
	}
	return next
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// --- the gate --------------------------------------------------------------

// protect wraps the whole site. Everything requires a session except the
// health check and the login flow itself — static files included, because a
// list of "harmless" exceptions is a list that drifts.
func (auth *Auth) protect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || strings.HasPrefix(r.URL.Path, "/auth/") {
			next.ServeHTTP(w, r)
			return
		}
		s, ok := auth.session(r)
		if !ok {
			// The page JavaScript polls with fetch, and a redirect to the
			// issuer is something fetch can only experience as a baffling
			// cross-origin failure. A plain 401 is a signal it can act on:
			// reload, which re-enters the login flow as a navigation. Two of
			// the pollers ask for HTML and mark themselves with
			// X-Requested-With instead of an Accept header, so both spellings
			// of "this is a program, not a person" are honoured.
			if wantsJSON(r) || r.Header.Get("X-Requested-With") != "" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				io.WriteString(w, `{"error":"signed out"}`)
				return
			}
			switch r.Method {
			case http.MethodGet, http.MethodHead:
				http.Redirect(w, r, "/auth/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			default:
				// A POST cannot survive the round trip to the issuer, so
				// there is nothing useful to redirect to; the resubmit after
				// signing back in has to come from a person.
				http.Error(w, "signed out — reload the page and sign in", http.StatusUnauthorized)
			}
			return
		}
		// Sliding renewal: any visit in the back half of a session's life
		// buys a fresh one, so only a fortnight of absence signs you out.
		if now := time.Now(); now.Unix() > s.IssuedAt+int64(sessionTTL.Seconds())/2 {
			s.IssuedAt, s.Expires = now.Unix(), now.Add(sessionTTL).Unix()
			auth.setSession(w, r, s)
		}
		next.ServeHTTP(w, r)
	})
}
