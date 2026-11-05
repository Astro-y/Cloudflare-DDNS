//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

func IsAdmin() bool { return os.Geteuid() == 0 }

func RequireAdmin() error {
	if !IsAdmin() {
		return errors.New("此操作需要管理员权限，请使用 sudo cfddns 重新打开面板")
	}
	return nil
}

func HasSystemd() bool {
	st, err := os.Stat("/run/systemd/system")
	if err != nil || !st.IsDir() {
		return false
	}
	_, err = exec.LookPath("systemctl")
	return err == nil
}

type ServiceStatus struct {
	Active      bool
	Enabled     bool
	Description string
}

func GetServiceStatus() ServiceStatus {
	if !HasSystemd() {
		return ServiceStatus{Description: "当前系统未运行 systemd"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, "systemctl", "show", "cfddns.service", "--property=ActiveState,UnitFileState,LoadState", "--no-pager").Output()
	if err != nil {
		return ServiceStatus{Description: "无法读取服务状态"}
	}
	s := ServiceStatus{Description: "已停止"}
	for _, line := range strings.Split(string(b), "\n") {
		switch line {
		case "ActiveState=active":
			s.Active, s.Description = true, "运行中"
		case "ActiveState=failed":
			s.Description = "运行失败"
		case "ActiveState=activating":
			s.Description = "正在启动"
		case "LoadState=not-found":
			s.Description = "未安装服务"
		case "UnitFileState=enabled", "UnitFileState=enabled-runtime":
			s.Enabled = true
		}
	}
	return s
}

func ServiceAction(ctx context.Context, action string) error {
	if err := RequireAdmin(); err != nil {
		return err
	}
	if !HasSystemd() {
		return errors.New("当前系统未运行 systemd，请从面板选择前台常驻")
	}
	switch action {
	case "start", "stop", "restart", "enable", "disable":
		return systemctl(ctx, action, "cfddns.service")
	default:
		return errors.New("不支持的服务操作")
	}
}

func systemctl(ctx context.Context, args ...string) error {
	child, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	b, err := exec.CommandContext(child, "systemctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s 失败: %w: %.2000s", strings.Join(args, " "), err, strings.TrimSpace(string(b)))
	}
	return nil
}

func ReadLogs(ctx context.Context) (string, error) {
	if !HasSystemd() {
		return "", errors.New("非 systemd 环境的日志显示在前台运行会话中")
	}
	child, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	b, err := exec.CommandContext(child, "journalctl", "--unit=cfddns.service", "--lines=100", "--no-pager", "--output=short-iso").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("读取日志失败: %w", err)
	}
	return string(b), nil
}

func openReadFile(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}

func openLockFile(path string) (*os.File, error) {
	if err := rejectSymlinkPath(path); err != nil {
		return nil, err
	}
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		f.Close()
		return nil, errors.New("锁路径不是普通文件")
	}
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func tryFileLock(f *os.File) (bool, error) {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return false, nil
	}
	return err == nil, err
}

func unlockFile(f *os.File) { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN) }

func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func replaceFile(from, to string) error { return os.Rename(from, to) }

func InstallLocal(ctx context.Context, paths Paths, store *Store, cfg Config, token string) (err error) {
	if err := RequireAdmin(); err != nil {
		return err
	}
	if err := ValidateConfig(cfg); err != nil {
		return err
	}
	if err := validateTokenText(token); err != nil {
		return err
	}
	return installFiles(ctx, paths, store, cfg, token, HasSystemd(), GetServiceStatus(), os.Executable, systemctl)
}

func UninstallLocal(ctx context.Context, paths Paths, keepConfig bool) error {
	if err := RequireAdmin(); err != nil {
		return err
	}
	if HasSystemd() {
		if err := systemctl(ctx, "stop", "cfddns.service"); err != nil {
			return err
		}
		if err := systemctl(ctx, "disable", "cfddns.service"); err != nil {
			return err
		}
	}
	var failures []error
	for _, path := range []string{paths.Binary, paths.UnitFile} {
		if err := removeKnownFile(path); err != nil {
			failures = append(failures, err)
		}
	}
	if !keepConfig {
		for _, path := range []string{paths.ConfigFile(), paths.TokenFile(), paths.StateFile()} {
			if err := removeKnownFile(path); err != nil {
				failures = append(failures, err)
			}
		}
		// Keep operation.lock and ConfigDir: waiters must keep sharing the same inode.
		_ = os.Remove(paths.StateDir) // Remove only if empty; never recursively delete.
	}
	if HasSystemd() {
		if err := systemctl(ctx, "daemon-reload"); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
