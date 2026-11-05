package main

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

var errCredentialUpgradeRequired = errors.New("已安装程序尚未确认支持账户凭据格式，请先选择 13 更新程序或 1 重新安装，再更换 Token；旧凭据已保留")

// EnsureInstalledCredentialSupport prevents a newly downloaded panel from
// saving credentials that an older, still-installed daemon cannot read.
func EnsureInstalledCredentialSupport(ctx context.Context, paths Paths) error {
	return ensureInstalledCredentialSupport(ctx, paths, probeInstalledCredentialVersion)
}

func ensureInstalledCredentialSupport(ctx context.Context, paths Paths, probe func(context.Context, string) (string, error)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := os.Lstat(paths.Binary)
	if errors.Is(err, os.ErrNotExist) {
		return nil // A fresh install has no older daemon to protect.
	}
	if err != nil || !info.Mode().IsRegular() {
		return errCredentialUpgradeRequired
	}
	version, err := probe(ctx, paths.Binary)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return errCredentialUpgradeRequired
	}
	version = strings.TrimSpace(version)
	if version == "dev" {
		// Old versions do not implement --internal-version at all, so a
		// successful dev response denotes a build containing this capability.
		return nil
	}
	comparison, err := CompareReleaseVersions(version, "0.1.1")
	if err != nil || comparison < 0 {
		return errCredentialUpgradeRequired
	}
	return nil
}

func probeInstalledCredentialVersion(ctx context.Context, path string) (string, error) {
	child, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return captureInstalledVersion(exec.CommandContext(child, path, "--internal-version"))
}

type installedVersionOutput struct {
	data     []byte
	overflow bool
}

func (w *installedVersionOutput) Write(data []byte) (int, error) {
	const maximum = 256
	remaining := maximum - len(w.data)
	if len(data) > remaining {
		w.data = append(w.data, data[:remaining]...)
		w.overflow = true
	} else {
		w.data = append(w.data, data...)
	}
	return len(data), nil // Keep draining stdout without allocating more memory.
}

func captureInstalledVersion(command *exec.Cmd) (string, error) {
	output := &installedVersionOutput{}
	command.Stdout = output
	command.Stderr = io.Discard
	command.WaitDelay = 500 * time.Millisecond
	if err := command.Run(); err != nil || output.overflow {
		return "", errCredentialUpgradeRequired
	}
	return string(output.data), nil
}
