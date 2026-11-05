//go:build linux

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

type installerFixture struct {
	root       string
	bin        string
	tmp        string
	payload    string
	manifest   string
	executed   string
	downloads  string
	curlArgs   string
	sudoLog    string
	osName     string
	arch       string
	uid        string
	failKind   string
	programErr string
}

func newInstallerFixture(t *testing.T) *installerFixture {
	t.Helper()
	for _, tool := range []string{"bash", "sha256sum", "mktemp", "chmod", "rm", "rmdir", "cat"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("Linux installer test requires %s: %v", tool, err)
		}
	}
	root := t.TempDir()
	f := &installerFixture{
		root: root, bin: filepath.Join(root, "bin"), tmp: filepath.Join(root, "tmp"),
		payload: filepath.Join(root, "payload"), manifest: filepath.Join(root, "manifest"),
		executed: filepath.Join(root, "executed"), downloads: filepath.Join(root, "downloads"),
		curlArgs: filepath.Join(root, "curl-args"), sudoLog: filepath.Join(root, "sudo"),
		osName: "Linux", arch: "x86_64", uid: "0", programErr: "0",
	}
	for _, dir := range []string{f.bin, f.tmp} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	f.write(t, f.payload, `#!/bin/bash
set -eu
[[ -t 0 ]] || exit 91
[[ -t 1 ]] || exit 92
printf '%s\n' "$0" > "$CFDDNS_TEST_EXECUTED"
exit "$CFDDNS_TEST_PROGRAM_EXIT"
`, 0600)
	hash := f.payloadDigest(t)
	f.write(t, f.manifest, fmt.Sprintf("%s  cfddns-linux-amd64\n%s  cfddns-linux-arm64\n", hash, hash), 0600)
	f.write(t, filepath.Join(f.bin, "uname"), `#!/bin/bash
set -eu
case "${1-}" in
  -s) printf '%s\n' "$CFDDNS_TEST_OS" ;;
  -m) printf '%s\n' "$CFDDNS_TEST_ARCH" ;;
  *) exit 80 ;;
esac
`, 0700)
	f.write(t, filepath.Join(f.bin, "id"), `#!/bin/bash
set -eu
[[ "${1-}" == -u ]] || exit 80
printf '%s\n' "$CFDDNS_TEST_UID"
`, 0700)
	f.write(t, filepath.Join(f.bin, "sudo"), `#!/bin/bash
set -eu
printf '%s\n' "$*" > "$CFDDNS_TEST_SUDO_LOG"
if [[ "${1-}" == -- ]]; then shift; fi
exec "$@"
`, 0700)
	f.write(t, filepath.Join(f.bin, "curl"), `#!/bin/bash
set -eu
printf '%s\n' "$*" >> "$CFDDNS_TEST_CURL_ARGS"
output=''
url=''
while (($#)); do
  case "$1" in
    --output|-o) output="$2"; shift 2 ;;
    https://*) url="$1"; shift ;;
    *) shift ;;
  esac
done
[[ -n "$output" && -n "$url" ]] || exit 81
printf '%s\n' "$url" >> "$CFDDNS_TEST_DOWNLOADS"
case "$url" in
  */SHA256SUMS)
    [[ "$CFDDNS_TEST_FAIL_KIND" != manifest ]] || exit 22
    cat -- "$CFDDNS_TEST_MANIFEST" > "$output"
    ;;
  */cfddns-linux-amd64|*/cfddns-linux-arm64)
    [[ "$CFDDNS_TEST_FAIL_KIND" != binary ]] || exit 22
    cat -- "$CFDDNS_TEST_PAYLOAD" > "$output"
    ;;
  *) exit 82 ;;
esac
`, 0700)
	return f
}

func (f *installerFixture) write(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func (f *installerFixture) payloadDigest(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(f.payload)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

// Run the script as bash -c, matching the documented command substitution.
// Bash is used here because the deployed bootstrap is a native Linux Bash script.
func (f *installerFixture) run(t *testing.T, withTTY bool) (int, string) {
	t.Helper()
	script, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bash, "-c", string(script))
	cmd.Env = append(os.Environ(),
		"PATH="+f.bin+string(os.PathListSeparator)+os.Getenv("PATH"), "TMPDIR="+f.tmp,
		"BASH_ENV=", "ENV=", "CFDDNS_TEST_OS="+f.osName, "CFDDNS_TEST_ARCH="+f.arch,
		"CFDDNS_TEST_UID="+f.uid, "CFDDNS_TEST_FAIL_KIND="+f.failKind,
		"CFDDNS_TEST_PAYLOAD="+f.payload, "CFDDNS_TEST_MANIFEST="+f.manifest,
		"CFDDNS_TEST_EXECUTED="+f.executed, "CFDDNS_TEST_DOWNLOADS="+f.downloads,
		"CFDDNS_TEST_CURL_ARGS="+f.curlArgs, "CFDDNS_TEST_SUDO_LOG="+f.sudoLog,
		"CFDDNS_TEST_PROGRAM_EXIT="+f.programErr)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	var output []byte
	if withTTY {
		master, slave := openPasswordPTY(t)
		masterFD := int(master.Fd())
		if err := unix.SetNonblock(masterFD, true); err != nil {
			t.Fatal(err)
		}
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		cmd.SysProcAttr.Setctty, cmd.SysProcAttr.Ctty = true, 0
		err = cmd.Run()
		var buffer [4096]byte
		for {
			n, readErr := unix.Read(masterFD, buffer[:])
			if n > 0 {
				output = append(output, buffer[:n]...)
			}
			if readErr != nil || n == 0 {
				break
			}
		}
	} else {
		cmd.Stdin = strings.NewReader("")
		output, err = cmd.CombinedOutput()
	}
	if ctx.Err() != nil {
		t.Fatalf("installer did not terminate: %v; output: %s", ctx.Err(), output)
	}
	entries, readErr := os.ReadDir(f.tmp)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("installer left temporary files: %v; error: %v; output: %s", entries, readErr, output)
	}
	if err == nil {
		return 0, string(output)
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("could not run installer: %v; output: %s", err, output)
	}
	return exit.ExitCode(), string(output)
}

func (f *installerFixture) assertNotExecuted(t *testing.T) {
	t.Helper()
	if data, err := os.ReadFile(f.executed); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unverified program was executed: %q; error: %v", data, err)
	}
}

func TestInstallerSupportedArchitecturesAndVerifiedTTYLaunch(t *testing.T) {
	for _, test := range []struct{ machine, asset string }{
		{"x86_64", "cfddns-linux-amd64"}, {"amd64", "cfddns-linux-amd64"},
		{"aarch64", "cfddns-linux-arm64"}, {"arm64", "cfddns-linux-arm64"},
	} {
		t.Run(test.machine, func(t *testing.T) {
			f := newInstallerFixture(t)
			f.arch = test.machine
			if code, output := f.run(t, true); code != 0 {
				t.Fatalf("installer failed (%d): %s", code, output)
			}
			executed, err := os.ReadFile(f.executed)
			if err != nil || filepath.Base(strings.TrimSpace(string(executed))) != test.asset {
				t.Fatalf("wrong executable launched: %q; error: %v", executed, err)
			}
			downloads, err := os.ReadFile(f.downloads)
			if err != nil {
				t.Fatal(err)
			}
			base := "https://github.com/Astro-y/Cloudflare-DDNS/releases/download/v0.1.1/"
			lines := strings.Fields(string(downloads))
			if len(lines) != 2 || !bytes.Contains(downloads, []byte(base+test.asset+"\n")) || !bytes.Contains(downloads, []byte(base+"SHA256SUMS\n")) {
				t.Fatalf("unexpected release downloads: %s", downloads)
			}
			args, err := os.ReadFile(f.curlArgs)
			if err != nil {
				t.Fatal(err)
			}
			for _, line := range strings.Split(strings.TrimSpace(string(args)), "\n") {
				if (!strings.HasPrefix(line, "-q ") && !strings.HasPrefix(line, "--disable ")) || !strings.Contains(line, "--proto =https") || !strings.Contains(line, "--proto-redir =https") {
					t.Fatalf("download allowed curl config or non-HTTPS transport: %s", line)
				}
			}
		})
	}
}

func TestInstallerRejectsUnsupportedPlatformBeforeDownload(t *testing.T) {
	for _, test := range []struct{ name, osName, arch string }{{"OS", "Darwin", "x86_64"}, {"architecture", "Linux", "riscv64"}} {
		t.Run(test.name, func(t *testing.T) {
			f := newInstallerFixture(t)
			f.osName, f.arch = test.osName, test.arch
			if code, output := f.run(t, true); code == 0 {
				t.Fatalf("unsupported platform accepted: %s", output)
			}
			f.assertNotExecuted(t)
			if _, err := os.Stat(f.downloads); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("unsupported platform attempted a download", err)
			}
		})
	}
}

func TestInstallerChecksumFailureNeverExecutesPayload(t *testing.T) {
	for _, kind := range []string{"mismatch", "missing", "duplicate", "malformed"} {
		t.Run(kind, func(t *testing.T) {
			f := newInstallerFixture(t)
			hash := f.payloadDigest(t)
			var manifest string
			switch kind {
			case "mismatch":
				manifest = strings.Repeat("0", 64) + "  cfddns-linux-amd64\n"
			case "missing":
				manifest = hash + "  cfddns-linux-arm64\n"
			case "duplicate":
				manifest = strings.Repeat(hash+"  cfddns-linux-amd64\n", 2)
			case "malformed":
				manifest = "not-a-sha256  cfddns-linux-amd64\n"
			}
			f.write(t, f.manifest, manifest, 0600)
			if code, output := f.run(t, true); code == 0 {
				t.Fatalf("bad checksum accepted: %s", output)
			}
			f.assertNotExecuted(t)
		})
	}
}

func TestInstallerDownloadFailureNeverExecutesPayload(t *testing.T) {
	for _, kind := range []string{"manifest", "binary"} {
		t.Run(kind, func(t *testing.T) {
			f := newInstallerFixture(t)
			f.failKind = kind
			if code, output := f.run(t, true); code == 0 {
				t.Fatalf("download failure ignored: %s", output)
			}
			f.assertNotExecuted(t)
		})
	}
}

func TestInstallerNonRootUsesSudoForVerifiedPayload(t *testing.T) {
	f := newInstallerFixture(t)
	f.uid = "1000"
	if code, output := f.run(t, true); code != 0 {
		t.Fatalf("sudo launch failed (%d): %s", code, output)
	}
	args, err := os.ReadFile(f.sudoLog)
	if err != nil || !strings.HasPrefix(string(args), "-- ") || !strings.Contains(string(args), "cfddns-linux-amd64") {
		t.Fatalf("expected sudo of verified binary: %q; error: %v", args, err)
	}
	if _, err := os.Stat(f.executed); err != nil {
		t.Fatal("sudo did not launch verified program", err)
	}
}

func TestInstallerPreservesProgramExitStatusAfterCleanup(t *testing.T) {
	f := newInstallerFixture(t)
	f.programErr = "37"
	if code, output := f.run(t, true); code != 37 {
		t.Fatalf("program exit status was masked (%d): %s", code, output)
	}
}

func TestInstallerWithoutTTYDoesNotDownloadOrRun(t *testing.T) {
	f := newInstallerFixture(t)
	if code, output := f.run(t, false); code == 0 {
		t.Fatalf("noninteractive installer accepted: %s", output)
	}
	f.assertNotExecuted(t)
	if _, err := os.Stat(f.downloads); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("installer downloaded before checking controlling terminal", err)
	}
}
