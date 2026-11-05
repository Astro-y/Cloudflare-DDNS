package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testUpdater(t *testing.T, handler http.HandlerFunc) *updaterClient {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return &updaterClient{
		httpClient: server.Client(), apiURL: server.URL + "/latest",
		releaseBase: server.URL + "/releases/download", pageBase: server.URL + "/releases/tag",
		architecture: "amd64", system: "linux",
		validURL: func(u *url.URL) bool {
			return u.Scheme == "https" && u.Host == parsed.Host && u.User == nil && u.Fragment == ""
		},
	}
}

func validReleaseMetadata() string {
	return `{"tag_name":"v0.1.1","draft":false,"prerelease":false,"html_url":"https://untrusted.invalid/ignored","assets":[{"name":"cfddns-linux-amd64","state":"uploaded","browser_download_url":"https://untrusted.invalid/ignored"},{"name":"cfddns-linux-arm64","state":"uploaded"},{"name":"SHA256SUMS","state":"uploaded"}]}`
}

func TestUpdaterVersionComparison(t *testing.T) {
	for _, test := range []struct {
		current, target string
		want            int
	}{{"0.1.0", "v0.1.1", -1}, {"v0.10.0", "0.9.9", 1}, {"v1.0.0", "1.0.0", 0}, {"dev", "v0.1.1", -1}, {"1.0.9", "v1.1.0", -1}, {"2.0.0", "v1.999.999", 1}} {
		got, err := CompareReleaseVersions(test.current, test.target)
		if err != nil || got != test.want {
			t.Fatalf("%s vs %s => %d %v", test.current, test.target, got, err)
		}
	}
	for _, invalid := range []string{"", "v1", "v1.2", "v01.2.3", "v1.2.3-beta", "v1.2.3+build", "v-1.2.3", "v18446744073709551616.1.1", "v1.2.3/../other"} {
		if _, err := CompareReleaseVersions("0.1.0", invalid); err == nil {
			t.Fatalf("invalid target version accepted: %q", invalid)
		}
		if _, err := CompareReleaseVersions(invalid, "v1.2.3"); err == nil {
			t.Fatalf("invalid installed version accepted: %q", invalid)
		}
	}
}

func TestUpdaterMetadataUsesKnownRepositoryURLsAndNoCredentials(t *testing.T) {
	t.Setenv("GH_TOKEN", "must-not-send-environment-token")
	u := testUpdater(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/latest" || r.Method != http.MethodGet || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("metadata request used wrong route, method or credentials")
		}
		fmt.Fprint(w, validReleaseMetadata())
	})
	info, err := u.check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want, _ := u.infoForTag("v0.1.1")
	if info != want || strings.Contains(info.BinaryURL, "untrusted") {
		t.Fatalf("metadata controlled update download URL: %+v", info)
	}
	u.architecture = "arm64"
	info, err = u.check(context.Background())
	if err != nil || !strings.HasSuffix(info.BinaryURL, "cfddns-linux-arm64") {
		t.Fatalf("wrong architecture update: %+v %v", info, err)
	}
}

func TestUpdaterRejectsIncompleteAndNonStableReleases(t *testing.T) {
	for name, mutate := range map[string]func(string) string{
		"draft":        func(s string) string { return strings.Replace(s, `"draft":false`, `"draft":true`, 1) },
		"prerelease":   func(s string) string { return strings.Replace(s, `"prerelease":false`, `"prerelease":true`, 1) },
		"unstable tag": func(s string) string { return strings.Replace(s, "v0.1.1", "v0.1.1-beta", 1) },
		"unsafe tag":   func(s string) string { return strings.Replace(s, "v0.1.1", "v0.1.1/../../evil", 1) },
		"missing file": func(s string) string { return strings.Replace(s, "SHA256SUMS", "OTHER", 1) },
		"pending file": func(s string) string { return strings.Replace(s, `"state":"uploaded"`, `"state":"new"`, 1) },
		"duplicate":    func(s string) string { return strings.Replace(s, "cfddns-linux-arm64", "cfddns-linux-amd64", 1) },
		"invalid JSON": func(string) string { return "{not json}" },
	} {
		t.Run(name, func(t *testing.T) {
			u := testUpdater(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, mutate(validReleaseMetadata())) })
			if _, err := u.check(context.Background()); err == nil {
				t.Fatal("unsafe or incomplete release accepted")
			}
		})
	}
}

func TestUpdaterURLPolicyAndTLSDefaults(t *testing.T) {
	for _, address := range []string{
		"https://api.github.com/repos/Astro-y/Cloudflare-DDNS/releases/latest",
		"https://github.com/Astro-y/Cloudflare-DDNS/releases/download/v0.1.1/SHA256SUMS",
		"https://release-assets.githubusercontent.com/github-production-release-asset/file",
		"https://objects.githubusercontent.com/github-production-release-asset/file",
	} {
		parsed, _ := url.Parse(address)
		if !trustedUpdateURL(parsed) {
			t.Fatalf("GitHub update address rejected: %s", address)
		}
	}
	for _, address := range []string{
		"http://github.com/file", "https://github.com.evil.invalid/file", "https://evil.invalid/github.com/file",
		"https://user:secret@github.com/file", "https://github.com:8443/file", "https://github.com/file#part",
	} {
		parsed, _ := url.Parse(address)
		if trustedUpdateURL(parsed) {
			t.Fatalf("untrusted update address accepted: %s", address)
		}
	}
	client := newUpdaterClient()
	transport := client.httpClient.Transport.(*http.Transport)
	if transport.Proxy == nil || transport.TLSClientConfig != nil && transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("updater ignored network proxy configuration or disabled TLS verification")
	}
}

func TestUpdaterRedirectAndCertificateValidation(t *testing.T) {
	u := testUpdater(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://github.com/unsafe", http.StatusFound)
	})
	if _, err := u.check(context.Background()); err == nil {
		t.Fatal("HTTPS downgrade redirect accepted")
	}
	u = testUpdater(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, validReleaseMetadata()) })
	u.httpClient = &http.Client{Timeout: time.Second}
	if _, err := u.check(context.Background()); err == nil {
		t.Fatal("untrusted TLS server certificate accepted")
	}
}

func TestUpdaterVerifiedDownloadAndCleanup(t *testing.T) {
	binary := []byte("verified-update-program")
	digest := sha256.Sum256(binary)
	u := testUpdater(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("update request contained credentials")
		}
		if strings.HasSuffix(r.URL.Path, "SHA256SUMS") {
			fmt.Fprintf(w, "%X *cfddns-linux-amd64\r\n%s  cfddns-linux-arm64\r\n", digest, strings.Repeat("0", 64))
		} else {
			w.Write(binary)
		}
	})
	info, _ := u.infoForTag("v0.1.1")
	path, cleanup, err := u.download(context.Background(), info)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != string(binary) {
		t.Fatal("downloaded program differs", err)
	}
	if runtime.GOOS != "windows" {
		file, _ := os.Stat(path)
		dir, _ := os.Stat(filepath.Dir(path))
		if file.Mode().Perm() != 0755 || dir.Mode().Perm() != 0700 {
			t.Fatal("download permissions are incorrect")
		}
	}
	cleanup()
	if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("temporary program directory was not cleaned", err)
	}
}

func TestUpdaterChecksumFailuresNeverReturnInstallableArtifact(t *testing.T) {
	binary := []byte("new-program")
	digest := sha256.Sum256(binary)
	validLine := hex.EncodeToString(digest[:]) + "  cfddns-linux-amd64\n"
	for name, manifest := range map[string]string{
		"missing":   strings.ReplaceAll(validLine, "amd64", "arm64"),
		"duplicate": validLine + validLine,
		"malformed": "not-a-digest  cfddns-linux-amd64\n",
		"extra":     hex.EncodeToString(digest[:]) + " extra cfddns-linux-amd64\n",
		"mismatch":  strings.Repeat("0", 64) + "  cfddns-linux-amd64\n",
	} {
		t.Run(name, func(t *testing.T) {
			var binaryRequests atomic.Int32
			u := testUpdater(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "SHA256SUMS") {
					fmt.Fprint(w, manifest)
				} else {
					binaryRequests.Add(1)
					w.Write(binary)
				}
			})
			info, _ := u.infoForTag("v0.1.1")
			path, cleanup, err := u.download(context.Background(), info)
			defer cleanup()
			if err == nil || path != "" {
				t.Fatal("unverified artifact could be installed")
			}
			if name != "mismatch" && binaryRequests.Load() != 0 {
				t.Fatal("malformed manifest still caused a binary download")
			}
		})
	}
}

func TestUpdaterRejectsTamperedReleaseAndOversizedResponses(t *testing.T) {
	var requests atomic.Int32
	u := testUpdater(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Length", fmt.Sprint(maxUpdateMetadata+1))
		w.WriteHeader(http.StatusOK)
	})
	info, _ := u.infoForTag("v0.1.1")
	info.BinaryURL = "https://evil.invalid/program"
	if path, cleanup, err := u.download(context.Background(), info); err == nil || path != "" {
		cleanup()
		t.Fatal("tampered release info accepted")
	}
	if requests.Load() != 0 {
		t.Fatal("tampered release made a network request")
	}
	if _, err := u.check(context.Background()); err == nil {
		t.Fatal("oversized metadata response accepted")
	}
	for _, test := range []struct {
		name string
		size int
	}{{"manifest", maxUpdateManifest + 1}, {"binary", maxUpdateBinary + 1}} {
		t.Run(test.name, func(t *testing.T) {
			client := testUpdater(t, func(w http.ResponseWriter, r *http.Request) {
				if test.name == "binary" && strings.HasSuffix(r.URL.Path, "SHA256SUMS") {
					fmt.Fprintf(w, "%s  cfddns-linux-amd64\n", strings.Repeat("0", 64))
					return
				}
				w.Header().Set("Content-Length", fmt.Sprint(test.size))
				w.WriteHeader(http.StatusOK)
			})
			info, _ := client.infoForTag("v0.1.1")
			path, cleanup, err := client.download(context.Background(), info)
			defer cleanup()
			if err == nil || path != "" {
				t.Fatal("oversized update response accepted")
			}
		})
	}
}

func TestUpdaterCancellationAndUnavailableRelease(t *testing.T) {
	u := testUpdater(t, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := u.check(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("update check did not observe cancellation: %v", err)
	}
	for _, status := range []int{http.StatusNotFound, http.StatusForbidden, http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			client := testUpdater(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) })
			if _, err := client.check(context.Background()); err == nil {
				t.Fatal("unavailable release was accepted")
			}
		})
	}
}

func TestDownloadedUpdatePreservesConfigCredentialsAndServiceState(t *testing.T) {
	for _, active := range []bool{false, true} {
		t.Run(fmt.Sprint(active), func(t *testing.T) {
			paths, store, source := prepareInstall(t)
			credentials := Credentials{Version: 1, Token: "account-update-test-token", AccountID: strings.Repeat("a", 32)}
			noop := func(context.Context, ...string) error { return nil }
			if err := installFiles(context.Background(), paths, store, testConfig(), credentials.Token, true, ServiceStatus{}, source, noop, credentials.AccountID); err != nil {
				t.Fatal(err)
			}
			artifact := filepath.Join(t.TempDir(), "verified-release")
			if err := os.WriteFile(artifact, []byte("replacement-program"), 0755); err != nil {
				t.Fatal(err)
			}
			var actions []string
			run := func(_ context.Context, args ...string) error {
				actions = append(actions, strings.Join(args, " "))
				return nil
			}
			if err := installDownloadedRelease(context.Background(), paths, store, testConfig(), credentials, artifact, true, ServiceStatus{Active: active, Enabled: false}, run); err != nil {
				t.Fatal(err)
			}
			gotConfig, err := store.LoadConfig()
			if err != nil || !reflect.DeepEqual(gotConfig, testConfig()) {
				t.Fatal("update changed DNS configuration", err)
			}
			gotCredentials, err := store.LoadCredentials()
			if err != nil || gotCredentials != credentials {
				t.Fatal("update changed paired credentials", err)
			}
			wantActions := "daemon-reload"
			if active {
				wantActions += "|restart cfddns.service"
			}
			if strings.Join(actions, "|") != wantActions {
				t.Fatalf("update changed service state: %v", actions)
			}
			program, _ := os.ReadFile(paths.Binary)
			if string(program) != "replacement-program" {
				t.Fatal("update did not install downloaded artifact")
			}
		})
	}
}

func TestDownloadedUpdateActivationFailureRestoresOldProgram(t *testing.T) {
	paths, store, source := prepareInstall(t)
	credentials := Credentials{Version: 1, Token: "preserve-old-token"}
	noop := func(context.Context, ...string) error { return nil }
	if err := installFiles(context.Background(), paths, store, testConfig(), credentials.Token, true, ServiceStatus{}, source, noop); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(paths.Binary)
	artifact := filepath.Join(t.TempDir(), "verified-release")
	if err := os.WriteFile(artifact, []byte("new-program-that-fails-start"), 0755); err != nil {
		t.Fatal(err)
	}
	run := func(_ context.Context, args ...string) error {
		if args[0] == "restart" {
			return errors.New("simulated update restart failure")
		}
		return nil
	}
	if err := installDownloadedRelease(context.Background(), paths, store, testConfig(), credentials, artifact, true, ServiceStatus{Active: true, Enabled: true}, run); err == nil {
		t.Fatal("activation failure was ignored")
	}
	after, _ := os.ReadFile(paths.Binary)
	if string(before) != string(after) {
		t.Fatal("old program was not restored")
	}
	gotCredentials, _ := store.LoadCredentials()
	if gotCredentials != credentials {
		t.Fatal("old credentials were not restored")
	}
}
