package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func prepareInstall(t *testing.T) (Paths, *Store, func() (string, error)) {
	t.Helper()
	paths := testPaths(t)
	if err := os.MkdirAll(filepath.Dir(paths.UnitFile), 0755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "running-program")
	if err := os.WriteFile(source, []byte("new-program"), 0755); err != nil {
		t.Fatal(err)
	}
	return paths, NewStore(paths), func() (string, error) { return source, nil }
}

func TestInstallFreshStartsService(t *testing.T) {
	paths, store, executable := prepareInstall(t)
	var actions []string
	run := func(_ context.Context, args ...string) error {
		actions = append(actions, strings.Join(args, " "))
		return nil
	}
	if err := installFiles(context.Background(), paths, store, testConfig(), "new-secret", true, ServiceStatus{}, executable, run); err != nil {
		t.Fatal(err)
	}
	if strings.Join(actions, "|") != "daemon-reload|enable cfddns.service|start cfddns.service" {
		t.Fatal(actions)
	}
	unit, err := os.ReadFile(paths.UnitFile)
	if err != nil || !strings.Contains(string(unit), " --internal-daemon") || !strings.Contains(string(unit), "Restart=on-failure") {
		t.Fatal("invalid unit", err)
	}
	if strings.Contains(string(unit), "new-secret") {
		t.Fatal("token leaked into service")
	}
	if _, err := store.LoadConfig(); err != nil {
		t.Fatal(err)
	}
}

func TestReinstallPreservesStoppedAndDisabled(t *testing.T) {
	paths, store, executable := prepareInstall(t)
	run := func(context.Context, ...string) error { return nil }
	if err := installFiles(context.Background(), paths, store, testConfig(), "old", true, ServiceStatus{}, executable, run); err != nil {
		t.Fatal(err)
	}
	var actions []string
	run = func(_ context.Context, args ...string) error {
		actions = append(actions, strings.Join(args, " "))
		return nil
	}
	if err := installFiles(context.Background(), paths, store, testConfig(), "new", true, ServiceStatus{Active: false, Enabled: false}, executable, run); err != nil {
		t.Fatal(err)
	}
	if strings.Join(actions, "|") != "daemon-reload" {
		t.Fatal("reinstall changed stopped/enabled state", actions)
	}
}

func TestReinstallRestartsActiveWithoutChangingEnabled(t *testing.T) {
	paths, store, executable := prepareInstall(t)
	run := func(context.Context, ...string) error { return nil }
	if err := installFiles(context.Background(), paths, store, testConfig(), "old", true, ServiceStatus{}, executable, run); err != nil {
		t.Fatal(err)
	}
	var actions []string
	run = func(_ context.Context, args ...string) error {
		actions = append(actions, strings.Join(args, " "))
		return nil
	}
	if err := installFiles(context.Background(), paths, store, testConfig(), "new", true, ServiceStatus{Active: true, Enabled: false}, executable, run); err != nil {
		t.Fatal(err)
	}
	if strings.Join(actions, "|") != "daemon-reload|restart cfddns.service" {
		t.Fatal(actions)
	}
}

func TestFreshInstallStartFailureRollsBackLocalFiles(t *testing.T) {
	paths, store, executable := prepareInstall(t)
	var actions []string
	run := func(_ context.Context, args ...string) error {
		actions = append(actions, strings.Join(args, " "))
		if args[0] == "start" {
			return errors.New("test start failure")
		}
		return nil
	}
	if err := installFiles(context.Background(), paths, store, testConfig(), "new", true, ServiceStatus{}, executable, run); err == nil {
		t.Fatal("expected failure")
	}
	for _, path := range []string{paths.ConfigFile(), paths.TokenFile(), paths.Binary, paths.UnitFile} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("new file left by rollback: %s %v", path, err)
		}
	}
	if !strings.Contains(strings.Join(actions, "|"), "stop cfddns.service|disable cfddns.service|daemon-reload") {
		t.Fatal("service rollback incomplete", actions)
	}
}

func TestReinstallFailureRestoresPriorFilesAndService(t *testing.T) {
	paths, store, executable := prepareInstall(t)
	run := func(context.Context, ...string) error { return nil }
	if err := installFiles(context.Background(), paths, store, testConfig(), "old-secret", true, ServiceStatus{}, executable, run); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.Binary, []byte("old-program"), 0755); err != nil {
		t.Fatal(err)
	}
	var actions []string
	run = func(_ context.Context, args ...string) error {
		actions = append(actions, strings.Join(args, " "))
		if args[0] == "restart" {
			return errors.New("test restart failure")
		}
		return nil
	}
	if err := installFiles(context.Background(), paths, store, testConfig(), "new-secret", true, ServiceStatus{Active: true, Enabled: true}, executable, run); err == nil {
		t.Fatal("expected failure")
	}
	token, _ := store.LoadToken()
	if token != "old-secret" {
		t.Fatal("old token not restored")
	}
	b, _ := os.ReadFile(paths.Binary)
	if string(b) != "old-program" {
		t.Fatal("old binary not restored")
	}
	if !strings.Contains(strings.Join(actions, "|"), "stop cfddns.service|daemon-reload|enable cfddns.service|start cfddns.service") {
		t.Fatal("old service state not restored", actions)
	}
}

func TestInstallWithoutSystemdDoesNotCreateUnit(t *testing.T) {
	paths, store, executable := prepareInstall(t)
	run := func(context.Context, ...string) error { t.Fatal("non-systemd install invoked systemctl"); return nil }
	if err := installFiles(context.Background(), paths, store, testConfig(), "test", false, ServiceStatus{}, executable, run); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(paths.UnitFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("non-systemd created unit", err)
	}
}

func TestInstallAccountCredentialsAndRollbackTogether(t *testing.T) {
	paths, store, executable := prepareInstall(t)
	oldAccount := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	newAccount := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	run := func(context.Context, ...string) error { return nil }
	if err := installFiles(context.Background(), paths, store, testConfig(), "old-account-token", true, ServiceStatus{}, executable, run, oldAccount); err != nil {
		t.Fatal(err)
	}
	old := Credentials{Version: 1, Token: "old-account-token", AccountID: oldAccount}
	got, err := store.LoadCredentials()
	if err != nil || got != old {
		t.Fatal("account installation did not persist paired credentials", err)
	}
	run = func(_ context.Context, args ...string) error {
		if args[0] == "restart" {
			return errors.New("simulated service activation failure")
		}
		return nil
	}
	if err := installFiles(context.Background(), paths, store, testConfig(), "new-account-token", true, ServiceStatus{Active: true, Enabled: true}, executable, run, newAccount); err == nil {
		t.Fatal("expected activation failure")
	}
	got, err = store.LoadCredentials()
	if err != nil || got != old {
		t.Fatal("rollback did not restore both the old Token and Account ID", err)
	}
	unit, err := os.ReadFile(paths.UnitFile)
	if err != nil || strings.Contains(string(unit), old.Token) || strings.Contains(string(unit), oldAccount) {
		t.Fatal("credential metadata leaked into the service unit", err)
	}
}

func TestInstallInvalidAccountIDDoesNotWriteFiles(t *testing.T) {
	paths, store, executable := prepareInstall(t)
	run := func(context.Context, ...string) error { t.Fatal("invalid credentials reached systemctl"); return nil }
	if err := installFiles(context.Background(), paths, store, testConfig(), "test-token", true, ServiceStatus{}, executable, run, "invalid-account"); err == nil {
		t.Fatal("invalid account accepted")
	}
	for _, path := range []string{paths.ConfigFile(), paths.TokenFile(), paths.Binary, paths.UnitFile} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("invalid account wrote a local file", err)
		}
	}
}
