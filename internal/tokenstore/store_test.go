package tokenstore

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// refreshServer is a fake refresh endpoint: counts calls, records bodies, replies resp.
func refreshServer(t *testing.T, resp map[string]any) (*httptest.Server, *atomic.Int32, *[]string) {
	t.Helper()
	var n atomic.Int32
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, r.Header.Get("Content-Type")+" "+string(raw))
		json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv, &n, &bodies
}

// writeFile writes content to cred.json in a fresh temp dir.
func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cred.json")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// creds is a credential file holding only an access and a refresh token.
func creds(access, refresh string) string {
	return `{"tokens":{"access_token":"` + access + `","refresh_token":"` + refresh + `"}}`
}

func expired() string { return jwtAt(time.Now().Add(-time.Hour)) }

// jwtAt returns an unsigned JWT with only the exp claim.
func jwtAt(exp time.Time) string {
	p, _ := json.Marshal(map[string]any{"exp": exp.Unix()})
	return "h." + base64.RawURLEncoding.EncodeToString(p) + ".s"
}

// codexStore is configured like the codex provider.
func codexStore(path, refreshURL string) *Store {
	s := New(path, "tokens", 5*time.Minute, 5*time.Second)
	s.AccessKey, s.RefreshKey, s.AccountKey, s.LastRefreshKey = "access_token", "refresh_token", "account_id", "last_refresh"
	s.RefreshURL, s.ClientID = refreshURL, "cid"
	return s
}

func readDoc(t *testing.T, path string) (doc, tok map[string]any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("file is not JSON: %v", err)
	}
	return doc, doc["tokens"].(map[string]any)
}

func TestJWTExp(t *testing.T) {
	want := time.Unix(1792747926, 0)
	if got := jwtExp(jwtAt(want)); !got.Equal(want) {
		t.Fatalf("jwtExp = %v, want %v", got, want)
	}
	if !jwtExp("not-a-jwt").IsZero() {
		t.Fatal("jwtExp must fail on a non-JWT")
	}
}

// Expiry comes from the new JWT, so 3 Gets refresh once (constant rotation
// would log out a CLI sharing the file); write-back keeps other fields and 0600.
func TestRefreshOnlyOnceKeepsFile(t *testing.T) {
	path := writeFile(t, `{"auth_mode":"chatgpt","tokens":{"access_token":"`+expired()+`","refresh_token":"rt0","id_token":"idt"}}`)
	fresh := jwtAt(time.Now().Add(time.Hour))
	srv, n, _ := refreshServer(t, map[string]any{"access_token": fresh, "refresh_token": "rt1"})
	s := codexStore(path, srv.URL)
	for i := 0; i < 3; i++ {
		c, err := s.Get(context.Background())
		if err != nil || c.AccessToken != fresh {
			t.Fatalf("Get #%d = %+v, %v", i, c, err)
		}
	}
	if n.Load() != 1 {
		t.Fatalf("3 Gets must refresh exactly once, got %d", n.Load())
	}
	doc, tok := readDoc(t, path)
	if tok["refresh_token"] != "rt1" || tok["id_token"] != "idt" || doc["auth_mode"] != "chatgpt" {
		t.Fatalf("write-back must keep other fields and replace the refresh token: %v", doc)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0600 {
		t.Fatalf("credential file must stay 0600, got %v", info.Mode().Perm())
	}
}

// A refresh reply without account_id keeps the account_id from the file.
func TestRefreshKeepsAccountID(t *testing.T) {
	path := writeFile(t, `{"tokens":{"access_token":"`+expired()+`","refresh_token":"rt0","account_id":"acc-1"}}`)
	srv, _, bodies := refreshServer(t, map[string]any{"access_token": jwtAt(time.Now().Add(time.Hour)), "refresh_token": "rt1"})
	c, err := codexStore(path, srv.URL).Get(context.Background())
	if err != nil || c.AccountID != "acc-1" {
		t.Fatalf("refreshing Get must keep AccountID=acc-1, got %+v %v", c, err)
	}
	form, _ := url.ParseQuery((*bodies)[0][len("application/x-www-form-urlencoded "):])
	if form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != "rt0" || form.Get("client_id") != "cid" {
		t.Fatalf("bad refresh form body: %s", (*bodies)[0])
	}
}

// A client cancel mid-refresh must not drop the reply: the server already
// rotated the token, so the file would keep a dead one.
func TestRefreshSurvivesClientCancel(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		time.Sleep(200 * time.Millisecond)
		json.NewEncoder(w).Encode(map[string]any{"access_token": jwtAt(time.Now().Add(time.Hour)), "refresh_token": "rt1"})
	}))
	defer srv.Close()
	path := writeFile(t, creds(expired(), "rt0"))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	codexStore(path, srv.URL).Get(ctx)
	if _, tok := readDoc(t, path); n.Load() != 1 || tok["refresh_token"] != "rt1" {
		t.Fatalf("server got %d refreshes, file has refresh_token=%v (want rt1)", n.Load(), tok["refresh_token"])
	}
}

// Account switchers symlink auth.json to per-account files: refresh writes
// the real file and keeps the symlink.
func TestRefreshWritesThroughSymlink(t *testing.T) {
	srv, _, _ := refreshServer(t, map[string]any{"access_token": jwtAt(time.Now().Add(time.Hour)), "refresh_token": "rt1"})
	target := writeFile(t, creds(expired(), "rt0"))
	link := filepath.Join(filepath.Dir(target), "auth.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := codexStore(link, srv.URL).Get(context.Background()); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(link)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink must survive: %v %v", fi.Mode(), err)
	}
	if _, tok := readDoc(t, target); tok["refresh_token"] != "rt1" {
		t.Fatalf("real file must have the new token, got refresh_token=%v", tok["refresh_token"])
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(target), "*planport*")); len(left) != 0 {
		t.Fatalf("temp files left behind: %v", left)
	}
}

// Codex CLI keeps last_refresh at the file ROOT; writing it inside tokens
// would make the CLI see a stale time and rotate the token again.
func TestLastRefreshAtRoot(t *testing.T) {
	srv, _, _ := refreshServer(t, map[string]any{"access_token": jwtAt(time.Now().Add(time.Hour)), "refresh_token": "rt1"})
	path := writeFile(t, `{"auth_mode":"chatgpt","last_refresh":"2020-01-01T00:00:00Z","tokens":{"access_token":"`+expired()+`","refresh_token":"rt0"}}`)
	if _, err := codexStore(path, srv.URL).Get(context.Background()); err != nil {
		t.Fatal(err)
	}
	doc, tok := readDoc(t, path)
	at, err := time.Parse(time.RFC3339Nano, fmt.Sprint(doc["last_refresh"]))
	if err != nil || time.Since(at) > time.Minute || tok["last_refresh"] != nil || doc["auth_mode"] != "chatgpt" {
		t.Fatalf("root last_refresh must be the refresh time, not inside tokens: root=%v tokens=%v", doc["last_refresh"], tok["last_refresh"])
	}
}

// A failed refresh inside the Margin window returns the still-valid token;
// only an expired token fails.
func TestRefreshFailsTokenStillValid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) }))
	defer srv.Close()
	valid := jwtAt(time.Now().Add(2 * time.Minute)) // < 5m Margin, so refresh is attempted
	path := writeFile(t, `{"tokens":{"access_token":"`+valid+`","refresh_token":"rt0","account_id":"acc-1"}}`)
	c, err := codexStore(path, srv.URL).Get(context.Background())
	if err != nil || c.AccessToken != valid || c.AccountID != "acc-1" {
		t.Fatalf("valid token must be used when refresh fails: %+v %v", c, err)
	}
	path = writeFile(t, creds(jwtAt(time.Now().Add(-time.Minute)), "rt0"))
	if _, err := codexStore(path, srv.URL).Get(context.Background()); err == nil {
		t.Fatal("expired token with a failed refresh must fail")
	}
}

// A rejected refresh fails without corrupting the file, and the token is not
// resent until the file holds another one (new login); 429/5xx are transient
// and retried.
func TestRejectedRefreshTokenNotResent(t *testing.T) {
	status := http.StatusBadRequest
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(status)
		fmt.Fprint(w, `{"error":"invalid_grant"}`)
	}))
	defer srv.Close()
	path := writeFile(t, creds(expired(), "rt0"))
	s := codexStore(path, srv.URL)
	for i := 0; i < 3; i++ {
		if _, err := s.Get(context.Background()); err == nil {
			t.Fatal("rejected refresh must fail")
		}
	}
	if raw, _ := os.ReadFile(path); string(raw) == "" || !json.Valid(raw) {
		t.Fatalf("failed refresh must not corrupt the file: %q", raw)
	}
	if n.Load() != 1 {
		t.Fatalf("rejected token must not be resent: server got %d calls", n.Load())
	}
	os.WriteFile(path, []byte(creds(expired(), "rt-relogin")), 0600) // new login
	s.Get(context.Background())
	if n.Load() != 2 {
		t.Fatalf("a new token in the file must be refreshed: server got %d calls", n.Load())
	}
	status = http.StatusTooManyRequests // transient: must not be remembered
	os.WriteFile(path, []byte(creds(expired(), "rt-429")), 0600)
	s.Get(context.Background())
	s.Get(context.Background())
	if n.Load() != 4 {
		t.Fatalf("429 is transient and must be retried: server got %d calls (want 4)", n.Load())
	}
}

// Concurrent Gets on an expired token refresh once: a second refresh with the
// same token would be rejected (already rotated) and log the account out.
func TestConcurrentGetRefreshesOnce(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		time.Sleep(50 * time.Millisecond) // widen the window so a missing lock shows up
		json.NewEncoder(w).Encode(map[string]any{"access_token": jwtAt(time.Now().Add(time.Hour)), "refresh_token": "rt1"})
	}))
	defer srv.Close()
	s := codexStore(writeFile(t, creds(expired(), "rt0")), srv.URL)
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Get(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n.Load() != 1 {
		t.Fatalf("5 concurrent Gets must refresh exactly once, server got %d", n.Load())
	}
}

// If the write fails after a refresh (disk full, read-only dir, bind mount),
// the new token stays in memory, is reused, and is written once possible.
func TestRefreshKeepsTokenWhenSaveFails(t *testing.T) {
	newAccess := jwtAt(time.Now().Add(time.Hour))
	srv, n, _ := refreshServer(t, map[string]any{"access_token": newAccess, "refresh_token": "rt1"})
	path := writeFile(t, creds(expired(), "rt0"))
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0500); err != nil { // no temp file can be created
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0700)
	s := codexStore(path, srv.URL)
	for i := 0; i < 2; i++ {
		if c, err := s.Get(context.Background()); err != nil || c.AccessToken != newAccess {
			t.Fatalf("Get #%d during write failure must return the new token: %+v %v", i, c, err)
		}
	}
	if n.Load() != 1 {
		t.Fatalf("token held in memory must not be refreshed again, server got %d", n.Load())
	}
	os.Chmod(dir, 0700) // writable again
	if c, err := s.Get(context.Background()); err != nil || c.AccessToken != newAccess {
		t.Fatalf("Get after write recovers: %+v %v", c, err)
	}
	if _, tok := readDoc(t, path); tok["refresh_token"] != "rt1" || n.Load() != 1 {
		t.Fatalf("new token must be written to the file: refresh_token=%v, server got %d", tok["refresh_token"], n.Load())
	}
}

// Across repeated write failures each refresh uses the newest in-memory token,
// never the rotated one in the file; once writable the file gets the newest.
func TestRefreshKeepsTokenAcrossRepeatedSaveFailures(t *testing.T) {
	var sent []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		sent = append(sent, r.Form.Get("refresh_token"))
		// 1m access lifetime < 5m Margin: every Get refreshes
		json.NewEncoder(w).Encode(map[string]any{"access_token": jwtAt(time.Now().Add(time.Minute)), "refresh_token": fmt.Sprintf("rt%d", len(sent))})
	}))
	defer srv.Close()
	path := writeFile(t, creds(expired(), "rt0"))
	dir := filepath.Dir(path)
	os.Chmod(dir, 0500)
	defer os.Chmod(dir, 0700)
	s := codexStore(path, srv.URL)
	for i := 0; i < 3; i++ {
		if _, err := s.Get(context.Background()); err != nil {
			t.Fatalf("Get #%d: %v", i, err)
		}
	}
	if want := "rt0 rt1 rt2"; fmt.Sprint(sent) != "["+want+"]" {
		t.Fatalf("each refresh must use the newest token (%s), server got %v", want, sent)
	}
	os.Chmod(dir, 0700)
	s.Get(context.Background())
	if _, tok := readDoc(t, path); tok["refresh_token"] != "rt4" {
		t.Fatalf("once writable the file must have the newest token rt4, got %v (server got %v)", tok["refresh_token"], sent)
	}
}

// One Get writes the unsaved token, refreshes again and fails to write: the
// in-memory copy must track the token JUST written, not the older one.
func TestUnsavedTracksFileAfterCatchUpSave(t *testing.T) {
	path := writeFile(t, creds(expired(), "rt0"))
	dir := filepath.Dir(path)
	var sent []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		sent = append(sent, r.Form.Get("refresh_token"))
		if len(sent) == 2 {
			os.Chmod(dir, 0500) // the disk fails again right after the catch-up write
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": jwtAt(time.Now().Add(time.Minute)), "refresh_token": fmt.Sprintf("rt%d", len(sent))})
	}))
	defer srv.Close()
	defer os.Chmod(dir, 0700)
	s := codexStore(path, srv.URL)
	os.Chmod(dir, 0500)
	s.Get(context.Background()) // rt0 → rt1, write fails
	os.Chmod(dir, 0700)
	s.Get(context.Background()) // writes rt1, refreshes rt1 → rt2, write fails
	s.Get(context.Background()) // must refresh with rt2, not the dead rt1
	if fmt.Sprint(sent) != "[rt0 rt1 rt2]" {
		t.Fatalf("server must get [rt0 rt1 rt2], got %v", sent)
	}
}

// The symlink switches to B DURING A's refresh: A's new token goes to file A
// and B is untouched (overwriting B would log B out and mix up accounts).
func TestRefreshSymlinkSwitchedMidRefresh(t *testing.T) {
	a := writeFile(t, creds(expired(), "rtA"))
	b := writeFile(t, creds(jwtAt(time.Now().Add(time.Hour)), "rtB"))
	link := filepath.Join(t.TempDir(), "auth.json")
	if err := os.Symlink(a, link); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		os.Remove(link) // switch to B while A's refresh is in flight
		os.Symlink(b, link)
		json.NewEncoder(w).Encode(map[string]any{"access_token": jwtAt(time.Now().Add(time.Hour)), "refresh_token": "rtA2"})
	}))
	defer srv.Close()
	if _, err := codexStore(link, srv.URL).Get(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, tok := readDoc(t, b); tok["refresh_token"] != "rtB" {
		t.Fatalf("file B got A's token: refresh_token=%v (want rtB)", tok["refresh_token"])
	}
	if _, tok := readDoc(t, a); tok["refresh_token"] != "rtA2" {
		t.Fatalf("A's new token must go to file A: refresh_token=%v (want rtA2)", tok["refresh_token"])
	}
}

// A's unsaved token belongs to file A: kept while the link points to B, then
// written to A on return, without refreshing A's rotated token again.
func TestUnsavedStaysWithItsFile(t *testing.T) {
	accessB := jwtAt(time.Now().Add(time.Hour))
	a, b, link := writeFile(t, creds(expired(), "rtA")), writeFile(t, creds(accessB, "rtB")), filepath.Join(t.TempDir(), "auth.json")
	dirA := filepath.Dir(a)
	pointTo := func(target string) {
		os.Remove(link)
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	newA := jwtAt(time.Now().Add(time.Hour))
	srv, n, _ := refreshServer(t, map[string]any{"access_token": newA, "refresh_token": "rtA2"})
	pointTo(a)
	os.Chmod(dirA, 0500) // file A is not writable
	defer os.Chmod(dirA, 0700)
	s := codexStore(link, srv.URL)
	if c, err := s.Get(context.Background()); err != nil || c.AccessToken != newA {
		t.Fatalf("A must return the new token while the write fails: %+v %v", c, err)
	}
	pointTo(b)
	if c, err := s.Get(context.Background()); err != nil || c.AccessToken != accessB {
		t.Fatalf("pointing at B must use B's token: %+v %v", c, err)
	}
	os.Chmod(dirA, 0700)
	pointTo(a)
	if c, err := s.Get(context.Background()); err != nil || c.AccessToken != newA {
		t.Fatalf("back on A must use the in-memory token: %+v %v", c, err)
	}
	if _, tok := readDoc(t, a); tok["refresh_token"] != "rtA2" || n.Load() != 1 {
		t.Fatalf("rtA2 must be written to A without a new refresh: refresh_token=%v, server got %d", tok["refresh_token"], n.Load())
	}
	if _, tok := readDoc(t, b); tok["refresh_token"] != "rtB" {
		t.Fatalf("file B must not change: %v", tok["refresh_token"])
	}
}

// After a transient refresh failure (here 503) with the token still valid, Gets
// use it without refreshing for refreshRetry, then retry; a new login in the
// file or an expired token is refreshed at once.
func TestTransientRefreshFailureWaits(t *testing.T) {
	defer func(old time.Duration) { refreshRetry = old }(refreshRetry)
	refreshRetry = time.Second // the 7 Gets below must fit in it, even on a loaded machine
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	valid := jwtAt(time.Now().Add(2 * time.Minute)) // < 5m Margin, so refresh is due
	path := writeFile(t, creds(valid, "rt0"))
	s := codexStore(path, srv.URL)
	get := func() {
		if c, err := s.Get(context.Background()); err != nil || c.AccessToken != valid {
			t.Errorf("Get must return the still-valid token: %+v %v", c, err)
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			get()
		}()
	}
	wg.Wait()
	get()
	get()
	if n.Load() != 1 {
		t.Fatalf("7 Gets within refreshRetry must refresh once, server got %d", n.Load())
	}
	time.Sleep(refreshRetry)
	get()
	if n.Load() != 2 {
		t.Fatalf("after refreshRetry the refresh must be retried: server got %d (want 2)", n.Load())
	}
	os.WriteFile(path, []byte(creds(valid, "rt-relogin")), 0600) // new login
	get()
	if n.Load() != 3 {
		t.Fatalf("a new token in the file must be refreshed at once: server got %d (want 3)", n.Load())
	}
	os.WriteFile(path, []byte(creds(expired(), "rt-relogin")), 0600) // same refresh token, access token now expired
	if _, err := s.Get(context.Background()); err == nil || n.Load() != 4 {
		t.Fatalf("an expired token must be refreshed even within refreshRetry: err=%v, server got %d (want 4)", err, n.Load())
	}
}

// A transient refresh failure (429/5xx, even with a JSON body) must not tell the
// operator to log in again: only a rejected token (400/401/403) needs that.
func TestTransientRefreshErrorNoRelogin(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			fmt.Fprint(w, `{"error":"busy"}`)
		}))
		_, err := codexStore(writeFile(t, creds(expired(), "rt0")), srv.URL).Get(context.Background())
		srv.Close()
		if err == nil || strings.Contains(err.Error(), "log in again") || !strings.Contains(err.Error(), fmt.Sprint(status)) {
			t.Fatalf("HTTP %d: want an error naming the status without \"log in again\", got %v", status, err)
		}
	}
}
