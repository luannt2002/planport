// Package config is the only place that loads env and reads settings. No
// defaults; every invalid key is reported in one error. Provider <N> of type
// <T> reads <K> from PLANPORT_PROVIDER_<N>_<K>, PLANPORT_TYPE_<T>_<K>, PLANPORT_<K>.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/luannt2002/planport/internal/provider"
	"github.com/luannt2002/planport/internal/provider/claudecli"
	"github.com/luannt2002/planport/internal/provider/codex"
	"github.com/luannt2002/planport/internal/provider/openaicompat"
)

// Config is everything planport reads from env.
type Config struct {
	Host      string
	Port      int
	APIKey    string // "" = no key check
	Providers provider.Registry
	// Warnings: missing credential files; not fatal, since a later login may create them.
	Warnings []string
}

// adapters builds a provider per TYPE plus its credential file ("" for claude_cli: the CLI owns its login).
var adapters = map[string]func(name string, e env) (provider.Provider, string){
	"claude_cli": func(name string, e env) (provider.Provider, string) {
		return claudecli.New(claudecli.Config{Name: name, Binary: e.path("BINARY"), Timeout: e.duration("TIMEOUT")}), ""
	},
	"codex": func(name string, e env) (provider.Provider, string) {
		credFile := e.path("CRED_FILE")
		return codex.New(codex.Config{
			Name:           name,
			CredFile:       credFile,
			BaseURL:        e.url("BASE_URL"),
			TokenURL:       e.url("TOKEN_URL"),
			ClientID:       e.str("CLIENT_ID"),
			Originator:     e.str("ORIGINATOR"),
			Beta:           e.str("BETA"),
			UserAgent:      e.str("USER_AGENT"),
			Timeout:        e.duration("TIMEOUT"),
			RefreshMargin:  e.duration("REFRESH_MARGIN"),
			RefreshTimeout: e.duration("REFRESH_TIMEOUT"),
		}), credFile
	},
	"openai_compat": func(name string, e env) (provider.Provider, string) {
		credFile := e.path("CRED_FILE")
		return openaicompat.New(openaicompat.Config{Name: name, CredFile: credFile, BaseURL: e.url("BASE_URL"), Timeout: e.duration("TIMEOUT")}), credFile
	},
}

var (
	validName = regexp.MustCompile(`^[A-Za-z0-9_]+$`) // provider names become part of env var names
	envKey    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// FromEnv builds Config from env, reporting every missing or invalid key at once.
func FromEnv() (Config, error) {
	var errs []string
	top := env{prefixes: []string{"PLANPORT_"}, errs: &errs}
	cfg := Config{Host: top.str("HOST")}
	if h := cfg.Host; h != "" && h != "localhost" && net.ParseIP(h) == nil {
		top.fail("HOST", " (must be an IP or localhost)")
	}
	cfg.Port, cfg.APIKey = top.port("PORT"), top.optional("API_KEY")
	if h := cfg.Host; h != "" && !strings.EqualFold(h, "localhost") && !net.ParseIP(h).IsLoopback() && cfg.APIKey == "" {
		top.fail("HOST", " (non-loopback host requires PLANPORT_API_KEY: tokens are personal account keys)")
	}
	seenName := map[string]bool{}
	credOwner := map[string]string{} // canonical credential path → last provider using it
	type credFile struct {
		name string
		info os.FileInfo
	}
	var credFiles []credFile // existing files: catches hard links (different path, same file)
	for _, name := range strings.Split(top.str("PROVIDERS"), ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		upper := strings.ToUpper(name)
		if !validName.MatchString(name) || seenName[upper] {
			errs = append(errs, fmt.Sprintf("PLANPORT_PROVIDERS (name %q: duplicate or has characters outside A-Z 0-9 _)", name))
			continue
		}
		seenName[upper] = true
		inst := "PLANPORT_PROVIDER_" + upper + "_"
		typ := strings.ToLower(top.under(inst).str("TYPE"))
		build, ok := adapters[typ]
		if !ok {
			if typ != "" {
				errs = append(errs, fmt.Sprintf("%sTYPE=%q (adapters: %s)", inst, typ, adapterNames()))
			}
			continue
		}
		p, cred := build(name, top.under(inst, "PLANPORT_TYPE_"+strings.ToUpper(typ)+"_", "PLANPORT_"))
		cfg.Providers = append(cfg.Providers, p)
		if cred == "" {
			continue
		}
		// Two providers refreshing one rotating token would log the account out.
		key := canonicalPath(cred)
		other, dup := credOwner[key]
		info, statErr := os.Stat(cred)
		for _, f := range credFiles {
			if !dup && statErr == nil && os.SameFile(f.info, info) {
				other, dup = f.name, true
			}
		}
		if dup {
			errs = append(errs, fmt.Sprintf("%sCRED_FILE is the same file as provider %q; a credential file belongs to one provider", inst, other))
		}
		credOwner[key] = name
		if statErr != nil {
			cfg.Warnings = append(cfg.Warnings, fmt.Sprintf("provider %q: credential file %s not usable yet (%v)", name, cred, statErr))
		} else {
			credFiles = append(credFiles, credFile{name, info})
		}
	}
	if len(cfg.Providers) == 0 && len(errs) == 0 { // PROVIDERS of blank names only (",")
		errs = append(errs, "PLANPORT_PROVIDERS (no providers)")
	}
	if len(errs) > 0 {
		return cfg, fmt.Errorf("missing or invalid env vars: %s (see .env.example)", strings.Join(errs, ", "))
	}
	return cfg, nil
}

func adapterNames() string {
	names := make([]string, 0, len(adapters))
	for n := range adapters {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// canonicalPath: absolute, symlinks resolved (of the parent dir if the file doesn't exist yet).
func canonicalPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = filepath.Clean(p)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	if dir, err := filepath.EvalSymlinks(filepath.Dir(abs)); err == nil {
		return filepath.Join(dir, filepath.Base(abs))
	}
	return abs
}

// env reads K from the first non-empty prefixes[i]+K; errs is shared so all errors are reported at once.
type env struct {
	prefixes []string
	errs     *[]string
}

func (e env) under(prefixes ...string) env { return env{prefixes: prefixes, errs: e.errs} }

func (e env) fail(key, why string) {
	names := make([]string, len(e.prefixes))
	for i, p := range e.prefixes {
		names[i] = p + key
	}
	*e.errs = append(*e.errs, strings.Join(names, " | ")+why)
}

func (e env) optional(key string) string {
	for _, p := range e.prefixes {
		if v := os.Getenv(p + key); v != "" {
			return v
		}
	}
	return ""
}

func (e env) str(key string) string {
	v := e.optional(key)
	if v == "" {
		e.fail(key, "")
	}
	return v
}

func (e env) port(key string) int {
	v := e.str(key)
	n, err := strconv.Atoi(v)
	if v != "" && (err != nil || n <= 0) {
		e.fail(key, " (must be a positive integer)")
	}
	if n > 65535 {
		e.fail(key, " (must be ≤ 65535)")
	}
	return n
}

func (e env) duration(key string) time.Duration {
	v := e.str(key)
	d, err := time.ParseDuration(v)
	if v != "" && (err != nil || d <= 0) {
		e.fail(key, " (must be a positive duration, e.g. 30s, 5m)")
	}
	return d
}

// url trims a trailing "/" so appended paths don't produce "//".
func (e env) url(key string) string {
	v := e.str(key)
	if v == "" {
		return ""
	}
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || strings.ContainsAny(v, "?#") {
		e.fail(key, " (must be a full http/https URL without query/fragment; paths are appended)")
		return ""
	}
	return strings.TrimRight(v, "/")
}

func (e env) path(key string) string {
	p := e.str(key)
	if strings.HasPrefix(p, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[1:])
		}
	}
	return p
}

// LoadDotEnv sets each KEY=VALUE line (no quotes or trailing comments; # lines skipped) unless the var is
// already non-empty. A repeated key is an error; a missing file only when required (an explicit -env).
func LoadDotEnv(path string, required bool) error {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) && !required {
		return nil
	}
	if err != nil {
		return err
	}
	seen := map[string]int{} // key → first line
	for i, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || !envKey.MatchString(key) {
			return fmt.Errorf("%s line %d: want KEY=VALUE (KEY: A-Z a-z 0-9 _, no \"export\")", path, i+1)
		}
		if first, dup := seen[key]; dup {
			return fmt.Errorf("%s: key %s repeated on lines %d and %d", path, key, first, i+1)
		}
		seen[key] = i + 1
		if os.Getenv(key) != "" {
			continue
		}
		if err := os.Setenv(key, strings.TrimSpace(val)); err != nil {
			return fmt.Errorf("%s line %d: %w", path, i+1, err)
		}
	}
	return nil
}
