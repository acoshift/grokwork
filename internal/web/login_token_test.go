package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/acoshift/grokwork/internal/audit"
	"github.com/acoshift/grokwork/internal/config"
	"github.com/acoshift/grokwork/internal/logintoken"
)

func TestLoginTokenPageAndAdmit(t *testing.T) {
	f := newAuthFixture(t, nil)
	h := f.srv.Handler()

	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("login status=%d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="page-login"`,
		`id="login-token-form"`,
		`hx-boost="false"`,
		`name="token"`,
		`id="login-token"`,
		`type="password"`,
		`id="login-discord"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("login page missing %q", want)
		}
	}

	sid, _, err := f.srv.LoginAs("member-1", "Member", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	secret := createLoginToken(t, h, sid, csrfFor(t, h, sid), "")
	wantRole, ok := f.cfg.ResolveWebRoleForConfig("member-1")
	if !ok {
		t.Fatal("member-1 has no web role")
	}

	var cookies []string
	for range 2 {
		w = postLoginToken(t, h, secret, "/config")
		if w.Code < 300 || w.Code >= 400 {
			t.Fatalf("login status=%d body=%s", w.Code, w.Body.String())
		}
		if path := locationPath(t, w); path != "/config" {
			t.Fatalf("next path=%q", path)
		}
		gotSID := postedSessionID(w)
		if gotSID == "" {
			t.Fatal("valid token set no session cookie")
		}
		cookies = append(cookies, gotSID)
		sess, _, found := f.srv.webSessions.Get(gotSID)
		if !found {
			t.Fatal("session cookie does not resolve")
		}
		if sess.DiscordUserID != "member-1" || sess.Role != wantRole || !sess.TokenLogin {
			t.Fatalf("session actor=%q role=%q token=%v want actor=member-1 role=%q token", sess.DiscordUserID, sess.Role, sess.TokenLogin, wantRole)
		}
	}
	if cookies[0] == cookies[1] {
		t.Fatal("two logins reused one session")
	}
	for _, c := range cookies {
		page := authedGet(t, h, "/", c)
		if page.Code != http.StatusOK {
			t.Fatalf("follow-up status=%d", page.Code)
		}
		if !strings.Contains(page.Body.String(), `title="member-1 · `+string(wantRole)+`"`) {
			t.Fatal("follow-up page is not the minting account")
		}
	}

	w = postLoginToken(t, h, secret, "//evil.example/phish")
	loc := w.Header().Get("Location")
	if strings.Contains(loc, "evil") {
		t.Fatalf("open redirect: %s", loc)
	}
	if path := locationPath(t, w); path != "/" {
		t.Fatalf("unsafe next path=%q", path)
	}

	text := auditText(t, f.srv.audit.Dir())
	if strings.Contains(text, secret) {
		t.Fatal("audit row contains the login token")
	}
	if !strings.Contains(text, audit.ActionLoginTokenOK) {
		t.Fatal("successful token login was not audited")
	}
}

func TestLoginTokenRejectsUnknownRevokedExpiredAndRoleless(t *testing.T) {
	f := newAuthFixture(t, nil)
	h := f.srv.Handler()
	sid, _, err := f.srv.LoginAs("member-1", "Member", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	csrf := csrfFor(t, h, sid)

	unknown := "gl_0123456789abcdef_THISSECRETMUSTNOTBELOGGED"
	w := postLoginToken(t, h, unknown, "/config")
	assertLoginRefused(t, h, w, unknown)
	if path := locationPath(t, w); path == "/config" || !strings.HasPrefix(path, "/login") {
		t.Fatalf("unknown token location=%q", path)
	}

	secret := createLoginToken(t, h, sid, csrf, "")
	id := loginTokenID(t, h, sid)
	rw := postAuthedForm(t, h, http.MethodPost, "/account/tokens/revoke", sid, url.Values{
		"csrf": {csrf},
		"id":   {id},
	})
	if rw.Code < 300 || rw.Code >= 400 {
		t.Fatalf("revoke status=%d body=%s", rw.Code, rw.Body.String())
	}
	w = postLoginToken(t, h, secret, "/")
	assertLoginRefused(t, h, w, secret)

	loc := time.Local
	clock := time.Date(2026, 6, 15, 15, 4, 0, 0, loc)
	f.srv.loginTokens.SetNow(func() time.Time { return clock })
	dated := createLoginToken(t, h, sid, csrf, "2026-06-15")
	if cell := expiryCell(t, authedGet(t, h, "/account/tokens", sid).Body.String(), datedID(t, f.srv, "member-1", dated)); cell != "2026-06-15" {
		t.Fatalf("expiry cell=%q", cell)
	}
	clock = time.Date(2026, 6, 15, 23, 30, 0, 0, loc)
	if id := postedSessionID(postLoginToken(t, h, dated, "/")); id == "" {
		t.Fatal("token refused during the chosen local date")
	}
	clock = time.Date(2026, 6, 16, 0, 0, 0, 0, loc)
	w = postLoginToken(t, h, dated, "/")
	assertLoginRefused(t, h, w, dated)

	clock = time.Date(2026, 6, 15, 12, 0, 0, 0, loc)
	timed := createLoginToken(t, h, sid, csrf, "2026-06-15T18:30")
	if id := postedSessionID(postLoginToken(t, h, timed, "/")); id == "" {
		t.Fatal("datetime token refused before its instant")
	}
	clock = time.Date(2026, 6, 15, 18, 30, 0, 0, loc)
	w = postLoginToken(t, h, timed, "/")
	assertLoginRefused(t, h, w, timed)

	before := len(f.srv.loginTokens.ListByOwner("member-1"))
	clock = time.Date(2026, 6, 15, 12, 0, 0, 0, loc)
	past := postAuthedForm(t, h, http.MethodPost, "/account/tokens", sid, url.Values{
		"csrf":    {csrf},
		"expires": {"2026-06-14"},
	})
	if past.Code != http.StatusUnprocessableEntity {
		t.Fatalf("past expiry status=%d", past.Code)
	}
	if strings.Contains(past.Body.String(), `id="login-token-secret"`) {
		t.Fatal("past expiry minted a secret")
	}
	if len(f.srv.loginTokens.ListByOwner("member-1")) != before {
		t.Fatal("past expiry created a token")
	}
	stuffed := postAuthedForm(t, h, http.MethodPost, "/account/tokens", sid, url.Values{
		"csrf":    {csrf},
		"expires": {dated},
	})
	if stuffed.Code != http.StatusUnprocessableEntity || strings.Contains(stuffed.Body.String(), `id="login-token-secret"`) {
		t.Fatalf("garbage expiry status=%d", stuffed.Code)
	}
	if len(f.srv.loginTokens.ListByOwner("member-1")) != before {
		t.Fatal("garbage expiry created a token")
	}

	rolelessSID, _, err := f.srv.LoginAs("stranger", "Stranger", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := f.cfg.ResolveWebRoleForConfig("stranger"); ok {
		t.Fatal("fixture stranger resolved a role")
	}
	roleless := createLoginToken(t, h, rolelessSID, csrfFor(t, h, rolelessSID), "")
	w = postLoginToken(t, h, roleless, "/config")
	assertLoginRefused(t, h, w, roleless)
	if path := locationPath(t, w); !strings.HasPrefix(path, "/login") {
		t.Fatalf("role-less location=%q", path)
	}

	text := auditText(t, f.srv.audit.Dir())
	for _, secret := range []string{unknown, dated, timed, roleless} {
		if strings.Contains(text, secret) {
			t.Fatal("audit row contains a login token")
		}
	}
	if !strings.Contains(text, audit.ActionLoginTokenFail) {
		t.Fatal("refused token login was not audited")
	}
	raw, err := os.ReadFile(filepath.Join(f.cfg.DataDir, logintoken.FileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{dated, timed, roleless} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("store file contains a plaintext token")
		}
	}
}

func TestLoginTokenManagement(t *testing.T) {
	f := newAuthFixture(t, nil)
	h := f.srv.Handler()
	member, _, err := f.srv.LoginAs("member-1", "Member", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	admin, _, err := f.srv.LoginAs("admin-1", "Admin", config.WebRoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	memberCSRF := csrfFor(t, h, member)
	adminCSRF := csrfFor(t, h, admin)

	home := authedGet(t, h, "/", member)
	if !strings.Contains(home.Body.String(), `id="nav-login-tokens"`) {
		t.Fatal("non-token session is not offered the token page")
	}

	created := postAuthedForm(t, h, http.MethodPost, "/account/tokens", member, url.Values{"csrf": {memberCSRF}})
	if created.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	secret := loginTokenSecret(t, created.Body.String())
	if strings.Count(created.Body.String(), secret) != 1 {
		t.Fatalf("secret shown %d times", strings.Count(created.Body.String(), secret))
	}
	if !strings.Contains(created.Body.String(), `class="mono login-token-expiry">none</td>`) {
		t.Fatal("omitted expiry did not render as none")
	}
	page := authedGet(t, h, "/account/tokens", member)
	if strings.Contains(page.Body.String(), secret) {
		t.Fatal("secret still on the page after create")
	}
	memberID := loginTokenID(t, h, member)

	adminSecret := createLoginToken(t, h, admin, adminCSRF, "")
	adminID := loginTokenID(t, h, admin)
	memberPage := authedGet(t, h, "/account/tokens", member)
	if strings.Contains(memberPage.Body.String(), adminID) || strings.Contains(memberPage.Body.String(), adminSecret) {
		t.Fatal("member page shows another account's token")
	}
	adminPage := authedGet(t, h, "/account/tokens", admin)
	if strings.Contains(adminPage.Body.String(), memberID) || strings.Contains(adminPage.Body.String(), secret) {
		t.Fatal("admin page shows another account's token")
	}

	cross := postAuthedForm(t, h, http.MethodPost, "/account/tokens/revoke", admin, url.Values{
		"csrf": {adminCSRF},
		"id":   {memberID},
	})
	if cross.Code != http.StatusNotFound {
		t.Fatalf("cross-account revoke status=%d", cross.Code)
	}
	if got := postedSessionID(postLoginToken(t, h, secret, "/")); got == "" {
		t.Fatal("cross-account revoke killed the token")
	}

	noCSRF := postAuthedForm(t, h, http.MethodPost, "/account/tokens", member, url.Values{"expires": {""}})
	if noCSRF.Code != http.StatusForbidden {
		t.Fatalf("create without csrf status=%d", noCSRF.Code)
	}
	badCSRF := postAuthedForm(t, h, http.MethodPost, "/account/tokens/revoke", member, url.Values{
		"csrf": {"not-the-token"},
		"id":   {memberID},
	})
	if badCSRF.Code != http.StatusForbidden {
		t.Fatalf("revoke without csrf status=%d", badCSRF.Code)
	}
	if len(f.srv.loginTokens.ListByOwner("member-1")) != 1 {
		t.Fatal("csrf failure changed tokens")
	}
	if got := postedSessionID(postLoginToken(t, h, secret, "/")); got == "" {
		t.Fatal("csrf failure revoked the token")
	}

	own := postAuthedForm(t, h, http.MethodPost, "/account/tokens/revoke", member, url.Values{
		"csrf": {memberCSRF},
		"id":   {memberID},
	})
	if own.Code < 300 || own.Code >= 400 {
		t.Fatalf("revoke status=%d", own.Code)
	}
	if got := postedSessionID(postLoginToken(t, h, secret, "/")); got != "" {
		t.Fatal("revoked token still logged in")
	}

	clock := time.Date(2031, 1, 2, 3, 4, 0, 0, time.Local)
	f.srv.loginTokens.SetNow(func() time.Time { return clock })
	open := createLoginToken(t, h, admin, adminCSRF, "")
	clock = clock.AddDate(8, 0, 0)
	if got := postedSessionID(postLoginToken(t, h, open, "/")); got == "" {
		t.Fatal("token with no expiry was refused")
	}

	text := auditText(t, f.srv.audit.Dir())
	for _, s := range []string{secret, adminSecret, open} {
		if strings.Contains(text, s) {
			t.Fatal("audit row contains a login token")
		}
	}
	if !strings.Contains(text, audit.ActionLoginTokenCreate) || !strings.Contains(text, audit.ActionLoginTokenRevoke) {
		t.Fatal("create or revoke was not audited")
	}
}

func TestLoginTokenSessionCannotManage(t *testing.T) {
	f := newAuthFixture(t, nil)
	h := f.srv.Handler()
	owner, _, err := f.srv.LoginAs("member-1", "Member", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	secret := createLoginToken(t, h, owner, csrfFor(t, h, owner), "")
	tokenSID := postedSessionID(postLoginToken(t, h, secret, "/"))
	if tokenSID == "" {
		t.Fatal("setup login failed")
	}
	sess, _, ok := f.srv.webSessions.Get(tokenSID)
	if !ok || !sess.TokenLogin || sess.DiscordUserID != "member-1" {
		t.Fatal("token login did not stamp the session")
	}

	page := authedGet(t, h, "/", tokenSID)
	if page.Code != http.StatusOK || strings.Contains(page.Body.String(), `id="nav-login-tokens"`) || strings.Contains(page.Body.String(), `href="/account/tokens"`) {
		t.Fatal("token session is offered the management page")
	}
	if !strings.Contains(authedGet(t, h, "/", owner).Body.String(), `id="nav-login-tokens"`) {
		t.Fatal("the same account's non-token session lost the link")
	}

	before := len(f.srv.loginTokens.ListByOwner("member-1"))
	tokenCSRF := csrfFor(t, h, tokenSID)
	for _, call := range []struct {
		path string
		form url.Values
	}{
		{"/account/tokens", url.Values{"csrf": {tokenCSRF}}},
		{"/account/tokens/revoke", url.Values{"csrf": {tokenCSRF}, "id": {loginTokenID(t, h, owner)}}},
	} {
		w := postAuthedForm(t, h, http.MethodPost, call.path, tokenSID, call.form)
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s token session status=%d", call.path, w.Code)
		}
	}
	got := authedGet(t, h, "/account/tokens", tokenSID)
	if got.Code != http.StatusForbidden {
		t.Fatalf("GET tokens token session status=%d", got.Code)
	}
	if len(f.srv.loginTokens.ListByOwner("member-1")) != before {
		t.Fatal("token session created a token")
	}
	if id := postedSessionID(postLoginToken(t, h, secret, "/")); id == "" {
		t.Fatal("token session revoked the token")
	}

	f.srv.webSessions.mu.Lock()
	held := f.srv.webSessions.sessions[tokenSID]
	held.ExpiresAt = time.Now().Add(12 * time.Hour)
	f.srv.webSessions.sessions[tokenSID] = held
	f.srv.webSessions.mu.Unlock()
	renewed := authedGet(t, h, "/", tokenSID)
	if postedSessionID(renewed) != tokenSID {
		t.Fatal("sliding renew did not refresh the cookie")
	}
	held2, _, ok := f.srv.webSessions.Get(tokenSID)
	if !ok || !held2.TokenLogin {
		t.Fatal("sliding renew dropped the token stamp")
	}
	if authedGet(t, h, "/account/tokens", tokenSID).Code != http.StatusForbidden {
		t.Fatal("renewed token session can open the page")
	}

	reloaded, err := newSessionStore(f.cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	f.srv.webSessions = reloaded
	again, _, ok := reloaded.Get(tokenSID)
	if !ok || !again.TokenLogin {
		t.Fatal("reloaded session lost the token stamp")
	}
	if authedGet(t, h, "/account/tokens", tokenSID).Code != http.StatusForbidden {
		t.Fatal("reloaded token session can open the page")
	}
	if postAuthedForm(t, h, http.MethodPost, "/account/tokens/revoke", tokenSID, url.Values{
		"csrf": {tokenCSRF},
		"id":   {loginTokenID(t, h, owner)},
	}).Code != http.StatusForbidden {
		t.Fatal("reloaded token session revoked a token")
	}
	if postAuthedForm(t, h, http.MethodPost, "/account/tokens", tokenSID, url.Values{"csrf": {tokenCSRF}}).Code != http.StatusForbidden {
		t.Fatal("reloaded token session created a token")
	}
	home := authedGet(t, h, "/", tokenSID)
	if home.Code != http.StatusOK || !strings.Contains(home.Body.String(), `title="member-1 · `) {
		t.Fatal("reloaded token session is not the account")
	}
	if _, err := f.srv.loginTokens.Authenticate(secret); err != nil {
		t.Fatal(err)
	}
	fresh := createLoginToken(t, h, owner, csrfFor(t, h, owner), "")
	if fresh == "" || fresh == secret {
		t.Fatal("non-token session could not mint after a token session was refused")
	}
}

func TestLoginTokenNotAnAPIBearer(t *testing.T) {
	f := newAuthFixture(t, nil)
	h := f.srv.Handler()
	sid, _, err := f.srv.LoginAs("member-1", "Member", config.WebRoleMember)
	if err != nil {
		t.Fatal(err)
	}
	secret := createLoginToken(t, h, sid, csrfFor(t, h, sid), "")
	if err := f.cfg.SetAPIEnabled(true); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/whoami", nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("login token as API bearer status=%d body=%s", w.Code, w.Body.String())
	}
	if _, err := f.srv.apiTokens.Authenticate(secret); err == nil {
		t.Fatal("api token store accepted a login token")
	}
}

func TestLoginTokenAuthOff(t *testing.T) {
	srv, _, _ := testServer(t)
	h := srv.Handler()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/account/tokens", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `id="page-login-tokens"`) || !strings.Contains(body, `id="login-tokens-unavailable"`) {
		t.Fatal("auth-off page does not explain that tokens are unavailable")
	}
	if strings.Contains(body, `id="login-token-create"`) || strings.Contains(body, `id="nav-login-tokens"`) {
		t.Fatal("auth-off page offers token management")
	}
	post := postAuthedForm(t, h, http.MethodPost, "/account/tokens", "", url.Values{"expires": {"2026-06-15"}})
	if post.Code != http.StatusForbidden {
		t.Fatalf("create with auth off status=%d", post.Code)
	}
	login := postLoginToken(t, h, "gl_0123456789abcdef_nope", "/config")
	if postedSessionID(login) != "" {
		t.Fatal("auth-off login set a session cookie")
	}
	if path := locationPath(t, login); path != "/" {
		t.Fatalf("auth-off login path=%q", path)
	}
}

func postLoginToken(t *testing.T, h http.Handler, token, next string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{"token": {token}}
	if next != "" {
		form.Set("next", next)
	}
	return postAuthedForm(t, h, http.MethodPost, "/login", "", form)
}

func postAuthedForm(t *testing.T, h http.Handler, method, path, sid string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if sid != "" {
		req.AddCookie(sessCookie(sid))
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func authedGet(t *testing.T, h http.Handler, path, sid string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(sessCookie(sid))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func createLoginToken(t *testing.T, h http.Handler, sid, csrf, expires string) string {
	t.Helper()
	form := url.Values{"csrf": {csrf}}
	if expires != "" {
		form.Set("expires", expires)
	}
	w := postAuthedForm(t, h, http.MethodPost, "/account/tokens", sid, form)
	if w.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", w.Code, w.Body.String())
	}
	return loginTokenSecret(t, w.Body.String())
}

func loginTokenSecret(t *testing.T, body string) string {
	t.Helper()
	const open = `<code id="login-token-secret">`
	i := strings.Index(body, open)
	if i < 0 {
		t.Fatal("create response has no secret")
	}
	rest := body[i+len(open):]
	j := strings.Index(rest, "</code>")
	if j <= 0 {
		t.Fatal("secret element not closed")
	}
	return rest[:j]
}

var loginTokenRowRe = regexp.MustCompile(`id="login-token-([0-9a-f]{16})"`)

func loginTokenID(t *testing.T, h http.Handler, sid string) string {
	t.Helper()
	body := authedGet(t, h, "/account/tokens", sid).Body.String()
	m := loginTokenRowRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatal("token page has no token id")
	}
	return m[1]
}

func datedID(t *testing.T, srv *Server, owner, secret string) string {
	t.Helper()
	rec, err := srv.loginTokens.Authenticate(secret)
	if err != nil {
		t.Fatal(err)
	}
	if rec.OwnerID != owner {
		t.Fatalf("owner=%q", rec.OwnerID)
	}
	return rec.ID
}

func expiryCell(t *testing.T, body, id string) string {
	t.Helper()
	needle := `id="login-token-` + id + `"`
	i := strings.Index(body, needle)
	if i < 0 {
		t.Fatalf("row %s missing", id)
	}
	rest := body[i:]
	const mark = `class="mono login-token-expiry">`
	j := strings.Index(rest, mark)
	if j < 0 {
		t.Fatal("expiry cell missing")
	}
	rest = rest[j+len(mark):]
	k := strings.Index(rest, "</td>")
	if k < 0 {
		t.Fatal("expiry cell not closed")
	}
	return rest[:k]
}

func postedSessionID(w *httptest.ResponseRecorder) string {
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookieName && c.Value != "" && c.MaxAge >= 0 {
			return c.Value
		}
	}
	return ""
}

func locationPath(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	loc := w.Header().Get("Location")
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatal(err)
	}
	if u.RawQuery != "" {
		return u.Path + "?" + u.RawQuery
	}
	return u.Path
}

func assertLoginRefused(t *testing.T, h http.Handler, w *httptest.ResponseRecorder, secret string) {
	t.Helper()
	if postedSessionID(w) != "" {
		t.Fatal("refused login set a session cookie")
	}
	loc := w.Header().Get("Location")
	if strings.Contains(loc, secret) || strings.Contains(w.Body.String(), secret) {
		t.Fatal("refused login echoed the token")
	}
	if !strings.HasPrefix(locationPath(t, w), "/login") {
		t.Fatalf("refused login location=%q", loc)
	}
	follow := httptest.NewRecorder()
	h.ServeHTTP(follow, httptest.NewRequest(http.MethodGet, "/config", nil))
	if follow.Code != http.StatusFound {
		t.Fatalf("unauthenticated follow-up status=%d", follow.Code)
	}
}

func auditText(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		b.Write(raw)
	}
	return b.String()
}
