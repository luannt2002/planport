// Package tokenstore reads and writes the official CLI's credential file and
// refreshes its OAuth token. Writes are atomic (temp file + rename) with mode
// 0600; tokens are never logged.
package tokenstore

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Store is one credential file: where the token lives, its field names and
// how to refresh it.
type Store struct {
	Path    string // credential file of the official CLI
	TokenAt string // field holding the token object, e.g. "tokens"
	// Field names inside the token object; AccountKey is empty if unused.
	AccessKey  string
	RefreshKey string
	AccountKey string
	// Refresh timestamp field at the file ROOT (codex); empty if unused.
	LastRefreshKey string
	RefreshURL     string
	ClientID       string        // public client ID of the official CLI
	Margin         time.Duration // refresh early when less than this remains

	mu       sync.Mutex // one lock per credential file
	client   *http.Client
	rejected string // refresh token the server rejected for good: never resend
	unsaved  *unsaved
}

// unsaved is a refreshed token not yet written to the real file path; stale
// is the old, already rotated refresh token still in that file.
type unsaved struct {
	path     string
	doc, tok map[string]any
	stale    string
}

// New returns a Store for path; each refresh is bounded by timeout. The
// provider sets the remaining fields.
func New(path, tokenAt string, margin, timeout time.Duration) *Store {
	return &Store{Path: path, TokenAt: tokenAt, Margin: margin, client: &http.Client{Timeout: timeout}}
}

// Credentials is what a provider needs to call its backend.
type Credentials struct {
	AccessToken string
	AccountID   string // empty if the file has no AccountKey
}

// Get returns a valid token, refreshing and writing it back when less than
// Margin remains. The lock keeps concurrent requests from refreshing twice.
func (s *Store) Get(ctx context.Context) (Credentials, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tok, doc, path, err := s.load()
	if err != nil {
		return Credentials{}, err
	}
	inFile := field(tok, s.RefreshKey) // refresh token currently in the file
	// An unsaved token of another file (symlink switched accounts) is kept
	// until the link points back to it.
	if u := s.unsaved; u != nil && u.path == path {
		if inFile == u.stale {
			// The file still holds the rotated token, so memory wins: retry the
			// write and keep using memory if it fails again.
			tok, doc = u.tok, u.doc
			if s.save(path, doc, tok) == nil {
				s.unsaved = nil
				inFile = field(tok, s.RefreshKey) // the file now holds this token
			}
		} else {
			s.unsaved = nil // the file changed (e.g. new login): the file wins
		}
	}
	access := field(tok, s.AccessKey)
	if access == "" {
		return Credentials{}, fmt.Errorf("%s has no %s; log in again with the CLI", s.Path, s.AccessKey)
	}
	current := Credentials{AccessToken: access, AccountID: field(tok, s.AccountKey)}
	expiry := jwtExp(access)
	if time.Until(expiry) >= s.Margin {
		return current, nil
	}

	refreshTok := field(tok, s.RefreshKey)
	if refreshTok == "" {
		return Credentials{}, fmt.Errorf("token is expiring and has no %s; log in again with the CLI", s.RefreshKey)
	}
	// Not cancelled with the client (only by the refresh timeout): the server
	// may have rotated the token, and dropping the reply strands a dead one.
	fresh, err := s.refresh(context.WithoutCancel(ctx), refreshTok)
	if err != nil {
		if left := time.Until(expiry); left > 0 {
			// The current token is still valid: use it; the next request retries.
			log.Printf("refresh %s failed, using current token valid for %s: %v", s.Path, left.Round(time.Second), err)
			return current, nil
		}
		return Credentials{}, err
	}
	// Write back to the same file so the official CLI picks up the fresh token.
	tok[s.AccessKey] = fresh.AccessToken
	tok[s.RefreshKey] = fresh.RefreshToken
	if s.LastRefreshKey != "" {
		doc[s.LastRefreshKey] = time.Now().UTC().Format(time.RFC3339Nano) // file root, as Codex CLI writes it
	}
	if err := s.save(path, doc, tok); err != nil {
		// The server already rotated the token: dropping the new one leaves a
		// dead token in the file. Keep it in memory; the next Get retries.
		log.Printf("warning: writing token to %s failed, keeping new token in memory and retrying: %v", s.Path, err)
		// stale is the token IN THE FILE: after a second failed write the token
		// just used (from memory) differs from it.
		s.unsaved = &unsaved{path: path, doc: doc, tok: tok, stale: inFile}
	}
	// Refresh replies usually omit account_id: keep the one from the file.
	return Credentials{AccessToken: fresh.AccessToken, AccountID: cmp.Or(fresh.AccountID, current.AccountID)}, nil
}

func field(tok map[string]any, key string) string {
	if key == "" {
		return ""
	}
	v, _ := tok[key].(string)
	return v
}

// jwtExp returns the JWT "exp" claim without verifying the signature (only the
// expiry matters here); an unreadable token gives the zero time, i.e. expired.
func jwtExp(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || json.Unmarshal(payload, &claims) != nil || claims.Exp == 0 {
		return time.Time{}
	}
	return time.Unix(claims.Exp, 0)
}

// load returns the token object, the whole document and the real file path.
// A symlink is resolved ONCE here and that file is written: resolving again on
// save could write this account's token over another account's file.
func (s *Store) load() (map[string]any, map[string]any, string, error) {
	path := s.Path
	if target, err := filepath.EvalSymlinks(path); err == nil {
		path = target
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, "", fmt.Errorf("read %s: %w", s.Path, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, nil, "", fmt.Errorf("parse JSON %s: %w", s.Path, err)
	}
	tok, ok := doc[s.TokenAt].(map[string]any)
	if !ok {
		return nil, nil, "", fmt.Errorf("%s has no %q object; not logged in?", s.Path, s.TokenAt)
	}
	return tok, doc, path, nil
}

// save writes doc atomically to the real path, not the symlink: renaming over
// a symlink would replace it with a regular file and strand a dead token.
func (s *Store) save(path string, doc map[string]any, tok map[string]any) error {
	doc[s.TokenAt] = tok
	clean, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	// Unique 0600 temp file in the same dir, so the rename is atomic.
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".planport-*")
	if err != nil {
		return fmt.Errorf("create temp file next to %s: %w", path, err)
	}
	tmp := f.Name()
	_, err = f.Write(append(clean, '\n'))
	if err == nil {
		err = f.Sync() // flush before rename so a power loss cannot leave an empty file
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return fmt.Errorf("write temp file %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

type refreshedToken struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	AccountID    string `json:"account_id"`
}

// refresh exchanges refreshToken for new tokens; a reply without a new
// refresh_token keeps the old one. 400/401/403 reject the token for good (e.g.
// revoked; only a new login helps) and it is never resent; 429/5xx are transient.
func (s *Store) refresh(ctx context.Context, refreshToken string) (refreshedToken, error) {
	if refreshToken == s.rejected {
		// Resending only costs a round trip under the lock; a new login puts a
		// different token in the file.
		return refreshedToken{}, fmt.Errorf("refresh token in %s was rejected; log in again with the CLI", s.Path)
	}
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refreshToken}, "client_id": {s.ClientID}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.RefreshURL, strings.NewReader(form.Encode()))
	if err != nil {
		return refreshedToken{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.client.Do(req)
	if err != nil {
		return refreshedToken{}, fmt.Errorf("call %s: %w", s.RefreshURL, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden:
		s.rejected = refreshToken
		return refreshedToken{}, fmt.Errorf("refresh rejected (HTTP %d); log in again with the CLI", resp.StatusCode)
	}
	var out refreshedToken
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return refreshedToken{}, fmt.Errorf("refresh %s returned invalid JSON (HTTP %d): %w", s.RefreshURL, resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK || out.AccessToken == "" {
		return refreshedToken{}, fmt.Errorf("refresh rejected (HTTP %d); log in again with the CLI", resp.StatusCode)
	}
	out.RefreshToken = cmp.Or(out.RefreshToken, refreshToken)
	return out, nil
}
