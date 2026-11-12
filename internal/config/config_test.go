package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isolateEnv blanks every PLANPORT_* for the test (empty counts as unset) and
// restores it afterwards.
func isolateEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "PLANPORT_") {
			t.Setenv(k, "")
		}
	}
}

// exampleKeys returns the .env.example keys in order, with their values.
func exampleKeys(t *testing.T) ([]string, map[string]string) {
	t.Helper()
	raw, err := os.ReadFile("../../.env.example")
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	vals := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			k, v, _ := strings.Cut(line, "=")
			keys = append(keys, k)
			vals[k] = v
		}
	}
	return keys, vals
}

// projectValue is the ALLOWLIST of keys that may have a value in .env.example.
// Every other key (secrets, provider data, future keys) stays blank there.
func projectValue(key string) bool {
	switch key {
	case "PLANPORT_HOST", "PLANPORT_PORT", "PLANPORT_TIMEOUT",
		"PLANPORT_REFRESH_MARGIN", "PLANPORT_REFRESH_TIMEOUT", "PLANPORT_PROVIDERS":
		return true
	}
	return strings.HasSuffix(key, "_TYPE") || strings.HasSuffix(key, "_ORIGINATOR")
}

// useExample loads .env.example and fills its blank keys with valid dummies,
// one credential file per provider.
func useExample(t *testing.T) {
	t.Helper()
	isolateEnv(t)
	if err := LoadDotEnv("../../.env.example", true); err != nil {
		t.Fatal(err)
	}
	keys, vals := exampleKeys(t)
	for _, k := range keys {
		switch {
		case k == "PLANPORT_API_KEY" || vals[k] != "":
		case strings.HasSuffix(k, "_URL"):
			os.Setenv(k, "https://example.invalid/x")
		case strings.HasSuffix(k, "_CRED_FILE"):
			os.Setenv(k, filepath.Join(t.TempDir(), k+".json"))
		default:
			os.Setenv(k, "dummy-value")
		}
	}
}

func mustFail(t *testing.T, want string) {
	t.Helper()
	if _, err := FromEnv(); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("want error containing %q, got: %v", want, err)
	}
}

// Gate: the committed template must not carry secrets or provider data.
func TestEnvExampleProjectValuesOnly(t *testing.T) {
	keys, vals := exampleKeys(t)
	blank := 0
	for _, k := range keys {
		if projectValue(k) {
			continue
		}
		blank++
		if vals[k] != "" {
			t.Errorf("%s has a value in .env.example; only project keys may, the rest go in .env", k)
		}
	}
	if blank == 0 {
		t.Fatal("no 🔒 keys found; malformed template?")
	}
}

// Gate: .env.example is complete and minimal: filled it builds, dropping any
// key (except optional API_KEY) fails, and every adapter has a sample provider.
func TestEnvExampleComplete(t *testing.T) {
	isolateEnv(t)
	mustFail(t, "PLANPORT_HOST, PLANPORT_PORT, PLANPORT_PROVIDERS")

	useExample(t)
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf(".env.example with dummy values must build: %v", err)
	}
	if cfg.Host == "" || cfg.Port <= 0 || len(cfg.Providers) == 0 || strings.Contains(strings.Join(cfg.Warnings, ","), `provider "claude"`) {
		t.Fatalf("bad config (claude_cli has no credential file to warn about): %+v", cfg)
	}
	keys, vals := exampleKeys(t)
	types := map[string]bool{}
	for _, name := range strings.Split(vals["PLANPORT_PROVIDERS"], ",") {
		types[vals["PLANPORT_PROVIDER_"+strings.ToUpper(strings.TrimSpace(name))+"_TYPE"]] = true
	}
	for typ := range adapters {
		if !types[typ] {
			t.Errorf("adapter %q has no provider in .env.example; its PLANPORT_TYPE_%s_* block is unchecked", typ, strings.ToUpper(typ))
		}
	}
	for _, k := range keys {
		if k == "PLANPORT_API_KEY" {
			continue
		}
		v := os.Getenv(k)
		os.Unsetenv(k)
		if _, err := FromEnv(); err == nil {
			t.Errorf("builds without %s; unused key in .env.example?", k)
		}
		os.Setenv(k, v)
	}
}

func TestConfigValidation(t *testing.T) {
	for _, tc := range []struct{ key, val, want string }{
		{"PLANPORT_HOST", "0.0.0.0", "PLANPORT_HOST (non-loopback host requires PLANPORT_API_KEY"},
		{"PLANPORT_HOST", "192.0.2.5", "PLANPORT_HOST (non-loopback host requires PLANPORT_API_KEY"},
		{"PLANPORT_HOST", "evil.example", "PLANPORT_HOST (must be an IP or localhost)"},
		{"PLANPORT_PORT", "70000", "PLANPORT_PORT (must be ≤ 65535)"},
		{"PLANPORT_PORT", "0", "PLANPORT_PORT (must be a positive integer)"},
		{"PLANPORT_TIMEOUT", "-1s", "PLANPORT_TIMEOUT (must be a positive duration"},
		{"PLANPORT_TYPE_OPENAI_COMPAT_BASE_URL", "api.example.com", "PLANPORT_TYPE_OPENAI_COMPAT_BASE_URL | PLANPORT_BASE_URL (must be a full http/https URL"},
		{"PLANPORT_TYPE_CODEX_TOKEN_URL", "ftp://x/y", "PLANPORT_TYPE_CODEX_TOKEN_URL | PLANPORT_TOKEN_URL (must be a full http/https URL"},
		{"PLANPORT_TYPE_CODEX_BASE_URL", "https://x/y?a=1", "PLANPORT_TYPE_CODEX_BASE_URL | PLANPORT_BASE_URL (must be a full http/https URL"},
		{"PLANPORT_TYPE_CODEX_BASE_URL", "https://x/y?", "PLANPORT_TYPE_CODEX_BASE_URL | PLANPORT_BASE_URL (must be a full http/https URL"},
		{"PLANPORT_TYPE_OPENAI_COMPAT_BASE_URL", "https://x/y#", "PLANPORT_TYPE_OPENAI_COMPAT_BASE_URL | PLANPORT_BASE_URL (must be a full http/https URL"},
		{"PLANPORT_PROVIDER_COMPAT_TYPE", "nope", `PLANPORT_PROVIDER_COMPAT_TYPE="nope" (adapters: claude_cli, codex, openai_compat)`},
		{"PLANPORT_PROVIDERS", "codex,CODEX", `name "CODEX": duplicate`},
		{"PLANPORT_PROVIDERS", "co-dex", `name "co-dex"`},
		{"PLANPORT_PROVIDERS", " , ", "PLANPORT_PROVIDERS (no providers)"},
		{"PLANPORT_PROVIDERS", "acc3", "PLANPORT_PROVIDER_ACC3_TYPE"},
	} {
		t.Run(tc.key+"="+tc.val, func(t *testing.T) {
			useExample(t)
			os.Setenv(tc.key, tc.val)
			mustFail(t, tc.want)
		})
	}
	// loopback is valid
	for _, host := range []string{"127.0.0.1", "::1", "localhost"} {
		useExample(t)
		os.Setenv("PLANPORT_HOST", host)
		if _, err := FromEnv(); err != nil {
			t.Fatalf("host %s must be valid: %v", host, err)
		}
	}
	// network host with an API key is valid
	for _, host := range []string{"0.0.0.0", "192.0.2.16"} {
		useExample(t)
		os.Setenv("PLANPORT_HOST", host)
		os.Setenv("PLANPORT_API_KEY", "k")
		if _, err := FromEnv(); err != nil {
			t.Fatalf("host %s with API key must be valid: %v", host, err)
		}
	}
	// trailing "/" is trimmed
	useExample(t)
	os.Setenv("PLANPORT_TYPE_OPENAI_COMPAT_BASE_URL", "https://api.example.com/")
	if _, err := FromEnv(); err != nil {
		t.Fatal(err)
	}
	if got := (env{prefixes: []string{"PLANPORT_TYPE_OPENAI_COMPAT_"}, errs: new([]string)}).url("BASE_URL"); got != "https://api.example.com" {
		t.Fatalf("url must trim trailing /, got %q", got)
	}
}

// A second account needs only TYPE + CRED_FILE (the rest comes from the type
// values); sharing a credential file is rejected.
func TestConfigMultiAccount(t *testing.T) {
	useExample(t)
	os.Setenv("PLANPORT_PROVIDERS", "codex, compat,codex2")
	os.Setenv("PLANPORT_PROVIDER_CODEX2_TYPE", "codex")
	os.Setenv("PLANPORT_PROVIDER_CODEX2_CRED_FILE", filepath.Join(t.TempDir(), "acc2.json"))
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("codex2 must build from its 2 own vars: %v", err)
	}
	if p, err := cfg.Providers.ByID("codex2"); err != nil || p.ID() != "codex2" {
		t.Fatalf("ByID(codex2): %v %v", p, err)
	}
	if len(cfg.Warnings) != 3 || !strings.Contains(cfg.Warnings[2], `provider "codex2": credential file`) {
		t.Fatalf("missing credential file must be a warning, not fatal: %q", cfg.Warnings)
	}
	os.Setenv("PLANPORT_PROVIDER_CODEX2_CRED_FILE", os.Getenv("PLANPORT_PROVIDER_CODEX_CRED_FILE"))
	mustFail(t, `PLANPORT_PROVIDER_CODEX2_CRED_FILE is the same file as provider "codex"`)
}

// One file spelled different ways is still a duplicate: relative vs absolute,
// symlink, hard link, symlinked parent dir of a not-yet-existing file.
func TestConfigDuplicateCredFileSpellings(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "acc.json")
	if err := os.WriteFile(target, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "link.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(target, filepath.Join(dir, "hard.json")); err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(t.TempDir(), "via")
	if err := os.Symlink(dir, linkDir); err != nil {
		t.Fatal(err)
	}
	for name, pair := range map[string][2]string{
		"relative vs absolute":               {"acc.json", target},
		"relative vs absolute, missing file": {"missing.json", filepath.Join(dir, "missing.json")},
		"symlink to file":                    {filepath.Join(dir, "link.json"), target},
		"hard link":                          {filepath.Join(dir, "hard.json"), target},
		"symlinked parent dir, new file":     {filepath.Join(linkDir, "new.json"), filepath.Join(dir, "new.json")},
	} {
		t.Run(name, func(t *testing.T) {
			useExample(t) // reads .env.example relative to the package, before chdir
			wd, _ := os.Getwd()
			t.Cleanup(func() { os.Chdir(wd) })
			if err := os.Chdir(dir); err != nil { // relative "acc.json" resolves into dir
				t.Fatal(err)
			}
			os.Setenv("PLANPORT_PROVIDERS", "codex,codex2")
			os.Setenv("PLANPORT_PROVIDER_CODEX2_TYPE", "codex")
			os.Setenv("PLANPORT_PROVIDER_CODEX_CRED_FILE", pair[0])
			os.Setenv("PLANPORT_PROVIDER_CODEX2_CRED_FILE", pair[1])
			mustFail(t, `PLANPORT_PROVIDER_CODEX2_CRED_FILE is the same file as provider "codex"`)
		})
	}
}

func TestLoadDotEnv(t *testing.T) {
	isolateEnv(t)
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("# comment\n\nPLANPORT_T_A = one\nPLANPORT_T_B=x=y\nPLANPORT_T_C=from-file\nPLANPORT_T_D=from-file\n"), 0600); err != nil {
		t.Fatal(err)
	}
	os.Setenv("PLANPORT_T_C", "from-env")
	os.Setenv("PLANPORT_T_D", "") // set but empty counts as unset
	if err := LoadDotEnv(path, true); err != nil {
		t.Fatalf("LoadDotEnv: %v", err)
	}
	for key, want := range map[string]string{"PLANPORT_T_A": "one", "PLANPORT_T_B": "x=y", "PLANPORT_T_C": "from-env", "PLANPORT_T_D": "from-file"} {
		if got := os.Getenv(key); got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
	missingFile := filepath.Join(t.TempDir(), "missing")
	if err := LoadDotEnv(missingFile, false); err != nil {
		t.Fatalf("missing default file must be ignored, got: %v", err)
	}
	if err := LoadDotEnv(missingFile, true); err == nil {
		t.Fatal("missing explicit file (-env) must fail")
	}
	for _, bad := range []string{"NO_EQUALS\n", "=x\n", "PLANPORT_T_E=1\nPLANPORT_T_E=2\n", "export PLANPORT_T_F=1\n", "PLANPORT T=1\n"} {
		if err := os.WriteFile(path, []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		if err := LoadDotEnv(path, true); err == nil {
			t.Fatalf("line %q must fail", bad)
		}
	}
}
