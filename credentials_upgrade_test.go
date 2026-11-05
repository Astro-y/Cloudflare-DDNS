package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCredentialUpgradeGuardAllowsSupportedInstalledVersions(t *testing.T) {
	for _, version := range []string{"0.1.1", "v0.1.1\n", "0.2.0", "2.0.0", "dev\n"} {
		t.Run(strings.TrimSpace(version), func(t *testing.T) {
			paths := testPaths(t)
			if err := os.MkdirAll(filepath.Dir(paths.Binary), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(paths.Binary, []byte("fixture binary"), 0755); err != nil {
				t.Fatal(err)
			}
			called := false
			probe := func(_ context.Context, path string) (string, error) {
				called = true
				if path != paths.Binary {
					t.Fatal("probed a different executable")
				}
				return version, nil
			}
			if err := ensureInstalledCredentialSupport(context.Background(), paths, probe); err != nil || !called {
				t.Fatal("supported installed version rejected", err)
			}
		})
	}
}

func TestCredentialUpgradeGuardRejectsOldUnknownAndProbeErrorsWithoutLeaking(t *testing.T) {
	for name, result := range map[string]struct {
		output string
		err    error
	}{
		"old":        {output: "0.1.0"},
		"unknown":    {output: "unexpected-fake-token-output"},
		"old-no-arg": {err: errors.New("unexpected-fake-token-output")},
		"mixed":      {output: "0.1.1\nunexpected-fake-token-output"},
	} {
		t.Run(name, func(t *testing.T) {
			paths := testPaths(t)
			if err := os.MkdirAll(filepath.Dir(paths.Binary), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(paths.Binary, []byte("fixture binary"), 0755); err != nil {
				t.Fatal(err)
			}
			store := NewStore(paths)
			if err := store.SaveToken("preserve-this-old-token"); err != nil {
				t.Fatal(err)
			}
			err := ensureInstalledCredentialSupport(context.Background(), paths, func(context.Context, string) (string, error) { return result.output, result.err })
			if !errors.Is(err, errCredentialUpgradeRequired) || strings.Contains(err.Error(), "unexpected-fake-token-output") || !strings.Contains(err.Error(), "13") || !strings.Contains(err.Error(), "1") {
				t.Fatal("upgrade did not fail safely")
			}
			credentials, err := store.LoadCredentials()
			if err != nil || credentials.Token != "preserve-this-old-token" {
				t.Fatal("upgrade guard modified existing credentials", err)
			}
		})
	}
}

func TestCredentialUpgradeGuardFreshInstallAndCancellationDoNotProbe(t *testing.T) {
	paths := testPaths(t)
	probe := func(context.Context, string) (string, error) { t.Fatal("unnecessary executable probe"); return "", nil }
	if err := ensureInstalledCredentialSupport(context.Background(), paths, probe); err != nil {
		t.Fatal("fresh install rejected", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ensureInstalledCredentialSupport(ctx, paths, probe); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation lost", err)
	}
}

func TestCredentialVersionProbeHelper(t *testing.T) {
	mode := os.Getenv("CFDDNS_TEST_VERSION_PROBE")
	if mode == "" {
		return
	}
	if mode == "flood" {
		fmt.Fprint(os.Stdout, strings.Repeat("fake-sensitive-output", 100000))
		os.Exit(0)
	}
	if mode == "failure" {
		fmt.Fprint(os.Stderr, "fake-sensitive-output")
		os.Exit(2)
	}
	fmt.Fprintln(os.Stdout, "0.1.1")
	os.Exit(0)
}

func TestCredentialVersionProbeBoundsAndRedactsProcessOutput(t *testing.T) {
	for _, mode := range []string{"version", "flood", "failure"} {
		t.Run(mode, func(t *testing.T) {
			command := exec.Command(os.Args[0], "-test.run=^TestCredentialVersionProbeHelper$")
			command.Env = append(os.Environ(), "CFDDNS_TEST_VERSION_PROBE="+mode)
			output, err := captureInstalledVersion(command)
			if mode == "version" {
				if err != nil || strings.TrimSpace(output) != "0.1.1" {
					t.Fatal("version probe failed", err)
				}
				return
			}
			if err == nil || output != "" || strings.Contains(err.Error(), "fake-sensitive-output") {
				t.Fatal("failed or oversized version probe was not safely rejected")
			}
		})
	}
}
