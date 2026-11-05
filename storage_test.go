package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func testPaths(t *testing.T) Paths {
	t.Helper()
	root := t.TempDir()
	return Paths{ConfigDir: filepath.Join(root, "config"), StateDir: filepath.Join(root, "state"), Binary: filepath.Join(root, "bin", "cfddns"), UnitFile: filepath.Join(root, "units", "cfddns.service")}
}

func testConfig() Config {
	return Config{Version: configVersion, IntervalSeconds: 60, TTL: 1, Records: []RecordConfig{{Type: "A", Name: "home.example.com", ZoneID: "zone1", ZoneName: "example.com", Enabled: true, Source: IPSource{Mode: "external"}}}}
}

func TestStoreRoundTripAndVersion(t *testing.T) {
	paths := testPaths(t)
	s := NewStore(paths)
	if err := s.SaveConfig(testConfig()); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveToken("secret-for-test"); err != nil {
		t.Fatal(err)
	}
	cfg, err := s.LoadConfig()
	if err != nil || cfg.Records[0].Name != "home.example.com" {
		t.Fatalf("config: %+v %v", cfg, err)
	}
	token, err := s.LoadToken()
	if err != nil || token != "secret-for-test" {
		t.Fatal("token round trip failed")
	}
	data, _ := os.ReadFile(paths.ConfigFile())
	if strings.Contains(string(data), "secret-for-test") {
		t.Fatal("token leaked into config")
	}
	state, err := s.LoadState()
	if err != nil || state.Records == nil {
		t.Fatal("missing state must produce initialized state", err)
	}
	state.Records["A"] = &RecordState{Name: "home.example.com", Address: "8.8.8.8"}
	if err := s.SaveState(state); err != nil {
		t.Fatal(err)
	}
	state, err = s.LoadState()
	if err != nil || state.Records["A"].Address != "8.8.8.8" {
		t.Fatal("state round trip failed", err)
	}
	if runtime.GOOS != "windows" {
		for _, path := range []string{paths.ConfigFile(), paths.TokenFile(), paths.StateFile()} {
			st, _ := os.Stat(path)
			if st.Mode().Perm() != 0600 {
				t.Fatalf("%s permissions %o", path, st.Mode().Perm())
			}
		}
		st, _ := os.Stat(paths.ConfigDir)
		if st.Mode().Perm() != 0700 {
			t.Fatalf("directory permissions %o", st.Mode().Perm())
		}
	}
	cfg.Version++
	b, _ := json.Marshal(cfg)
	if err := atomicWrite(paths.ConfigFile(), b, 0600, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadConfig(); err == nil {
		t.Fatal("future version accepted")
	}
}

func TestStoreInvalidWritePreservesOldValue(t *testing.T) {
	s := NewStore(testPaths(t))
	if err := s.SaveConfig(testConfig()); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveToken("old-secret"); err != nil {
		t.Fatal(err)
	}
	bad := testConfig()
	bad.IntervalSeconds = 1
	if err := s.SaveConfig(bad); err == nil {
		t.Fatal("invalid config accepted")
	}
	if err := s.SaveToken("new\nsecret"); err == nil {
		t.Fatal("newline in token accepted")
	}
	cfg, err := s.LoadConfig()
	if err != nil || cfg.IntervalSeconds != 60 {
		t.Fatal("old config lost", err)
	}
	token, _ := s.LoadToken()
	if token != "old-secret" {
		t.Fatal("old token lost")
	}
}

func TestCredentialsLegacyPlaintextAndAtomicAccountPair(t *testing.T) {
	s := NewStore(testPaths(t))
	if err := atomicWrite(s.Paths.TokenFile(), []byte("legacy-user-token\n"), 0600, true); err != nil {
		t.Fatal(err)
	}
	legacy, err := s.LoadCredentials()
	if err != nil || legacy.Version != 1 || legacy.Token != "legacy-user-token" || legacy.AccountID != "" {
		t.Fatal("legacy credentials were not retained", err)
	}
	want := Credentials{Version: 1, Token: "cfat_fake-account-token", AccountID: "0123456789abcdef0123456789abcdef"}
	if err := s.SaveCredentials(want); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadCredentials()
	if err != nil || got != want {
		t.Fatal("account credential pair did not round-trip", err)
	}
	token, err := s.LoadToken()
	if err != nil || token != want.Token {
		t.Fatal("legacy LoadToken accessor lost account token", err)
	}
	data, err := os.ReadFile(s.Paths.TokenFile())
	if err != nil || !bytes.Contains(data, []byte(`"account_id"`)) || !bytes.Contains(data, []byte(`"version": 1`)) {
		t.Fatal("account metadata is not in the same versioned credential file", err)
	}
	if err := s.SaveToken("replacement-user-token"); err != nil {
		t.Fatal(err)
	}
	user, err := s.LoadCredentials()
	if err != nil || user.AccountID != "" || user.Token != "replacement-user-token" {
		t.Fatal("switching back to a user token retained account metadata", err)
	}
}

func TestInvalidCredentialsWritePreservesEntirePair(t *testing.T) {
	s := NewStore(testPaths(t))
	old := Credentials{Version: 1, Token: "old-account-token", AccountID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	if err := s.SaveCredentials(old); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(s.Paths.TokenFile())
	if err != nil {
		t.Fatal(err)
	}
	for name, invalid := range map[string]Credentials{
		"account":         {Version: 1, Token: "new-token", AccountID: "../../accounts"},
		"version":         {Version: 2, Token: "new-token"},
		"missing-version": {Token: "new-token"},
		"token":           {Version: 1, Token: "new\ntoken"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := s.SaveCredentials(invalid); err == nil {
				t.Fatal("invalid credential pair accepted")
			}
			after, err := os.ReadFile(s.Paths.TokenFile())
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("failed validation changed stored credentials", err)
			}
			got, err := s.LoadCredentials()
			if err != nil || got != old {
				t.Fatal("old account/token pair lost", err)
			}
		})
	}
}

func TestCredentialsMalformedFilesNeverExposeToken(t *testing.T) {
	s := NewStore(testPaths(t))
	secret := "never-echo-this-fake-secret"
	valid := `{"version":1,"token":"` + secret + `","account_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`
	for name, content := range map[string]string{
		"syntax":          `{"version":1,"token":"` + secret,
		"unknown-field":   `{"version":1,"token":"` + secret + `","` + secret + `":true}`,
		"trailing-json":   valid + ` {"` + secret + `":true}`,
		"future-version":  `{"version":2,"token":"` + secret + `"}`,
		"missing-version": `{"token":"` + secret + `"}`,
		"bad-account":     `{"version":1,"token":"` + secret + `","account_id":"` + secret + `"}`,
		"array":           `["` + secret + `"]`,
		"invalid-token":   `{"version":1,"token":"` + secret + `\n"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := atomicWrite(s.Paths.TokenFile(), []byte(content), 0600, true); err != nil {
				t.Fatal(err)
			}
			credentials, err := s.LoadCredentials()
			if err == nil {
				t.Fatal("malformed credentials accepted")
			}
			if strings.Contains(err.Error(), secret) || credentials != (Credentials{}) {
				t.Fatal("invalid credential file exposed secret data")
			}
		})
	}
}

func TestAtomicCredentialsReadersAlwaysSeeMatchingPair(t *testing.T) {
	s := NewStore(testPaths(t))
	a := Credentials{Version: 1, Token: "token-A", AccountID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	b := Credentials{Version: 1, Token: "token-B", AccountID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
	if err := s.SaveCredentials(a); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	failures := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 80; i++ {
			got, err := s.LoadCredentials()
			if err == nil && got != a && got != b {
				err = errors.New("reader saw mismatched account/token pair")
			}
			if err != nil {
				failures <- err
				return
			}
		}
	}()
	for i := 0; i < 40; i++ {
		value := a
		if i%2 == 0 {
			value = b
		}
		if err := s.SaveCredentials(value); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
	select {
	case err := <-failures:
		t.Fatal(err)
	default:
	}
}

func TestNormalizeDomainAndConfigValidation(t *testing.T) {
	for in, want := range map[string]string{"@": "example.com", "home": "home.example.com", "HOME.EXAMPLE.COM.": "home.example.com"} {
		got, err := NormalizeDomain(in, "example.com")
		if err != nil || got != want {
			t.Fatalf("%q => %q %v", in, got, err)
		}
	}
	for _, in := range []string{"other.invalid", "-home", "a..example.com", "家庭.example.com", "*.example.com"} {
		if _, err := NormalizeDomain(in, "example.com"); err == nil {
			t.Fatalf("invalid domain accepted: %q", in)
		}
	}
	for name, change := range map[string]func(*Config){
		"duplicate": func(c *Config) { c.Records = append(c.Records, c.Records[0]) },
		"ttl":       func(c *Config) { c.TTL = 30 },
		"interval":  func(c *Config) { c.IntervalSeconds = 86401 },
		"source":    func(c *Config) { c.Records[0].Source.URL = "http://example.com" },
		"zone":      func(c *Config) { c.Records[0].ZoneID = "../../bad" },
	} {
		t.Run(name, func(t *testing.T) {
			c := testConfig()
			change(&c)
			if ValidateConfig(c) == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
	c := testConfig()
	c.Records[0].Enabled = false
	if err := ValidateConfig(c); err != nil {
		t.Fatal("all protocols may be paused", err)
	}
}

func TestStoreRejectsSymlink(t *testing.T) {
	s := NewStore(testPaths(t))
	if err := ensurePrivateDir(s.Paths.ConfigDir); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "valuable")
	if err := os.WriteFile(target, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, s.Paths.TokenFile()); err != nil {
		t.Skip("symlinks unavailable", err)
	}
	if err := s.SaveToken("replacement"); err == nil {
		t.Fatal("symlink overwritten")
	}
	if _, err := s.LoadToken(); err == nil {
		t.Fatal("symlink read")
	}
	b, _ := os.ReadFile(target)
	if string(b) != "keep" {
		t.Fatal("symlink target modified")
	}
}

func TestAtomicConfigReadersNeverSeePartialJSON(t *testing.T) {
	s := NewStore(testPaths(t))
	if err := s.SaveConfig(testConfig()); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	failures := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 40; i++ {
			if _, err := s.LoadConfig(); err != nil {
				select {
				case failures <- err:
				default:
				}
				return
			}
		}
	}()
	for i := 0; i < 40; i++ {
		cfg := testConfig()
		cfg.IntervalSeconds = 60 + i
		if err := s.SaveConfig(cfg); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
	select {
	case err := <-failures:
		t.Fatal(err)
	default:
	}
	entries, _ := os.ReadDir(s.Paths.ConfigDir)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".cfddns-") {
			t.Fatal("temporary file leaked")
		}
	}
}

func TestStoreLockContextAndRelease(t *testing.T) {
	s := NewStore(testPaths(t))
	unlock, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := s.Acquire(ctx); !errors.Is(err, context.DeadlineExceeded) {
		unlock()
		t.Fatalf("second locker: %v", err)
	}
	unlock()
	unlocked, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	unlocked()
}

func TestLockProcessHelper(t *testing.T) {
	dir := os.Getenv("CFDDNS_TEST_LOCK_DIR")
	if dir == "" {
		return
	}
	s := NewStore(Paths{ConfigDir: dir})
	unlock, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	fmt.Println("LOCKED")
	_, _ = os.Stdin.Read(make([]byte, 1))
}

func TestStoreLockReleasedAfterProcessDeath(t *testing.T) {
	s := NewStore(testPaths(t))
	cmd := exec.Command(os.Args[0], "-test.run=^TestLockProcessHelper$")
	cmd.Env = append(os.Environ(), "CFDDNS_TEST_LOCK_DIR="+s.Paths.ConfigDir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "LOCKED" {
		t.Fatalf("child did not acquire lock: %q %v", line, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := s.Acquire(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cross-process lock failed: %v", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel2()
	unlock, err := s.Acquire(ctx2)
	if err != nil {
		t.Fatal("dead process kept lock", err)
	}
	unlock()
}
