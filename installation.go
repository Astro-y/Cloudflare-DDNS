package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type fileSnapshot struct {
	path    string
	data    []byte
	mode    os.FileMode
	existed bool
}

func snapshotFile(path string) (fileSnapshot, error) {
	s := fileSnapshot{path: path}
	data, err := readPrivateFile(path, 64<<20)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	st, err := os.Stat(path)
	if err != nil {
		return s, err
	}
	s.data, s.mode, s.existed = data, st.Mode().Perm(), true
	return s, nil
}

func (s fileSnapshot) restore() error {
	if !s.existed {
		return removeKnownFile(s.path)
	}
	return atomicWrite(s.path, s.data, s.mode, false)
}

// installFiles has injected system operations so rollback is tested without
// touching the host's binaries, configuration or service manager.
func installFiles(ctx context.Context, paths Paths, store *Store, cfg Config, token string, systemd bool, prior ServiceStatus, executable func() (string, error), run func(context.Context, ...string) error, accountID ...string) (result error) {
	if store.Paths != paths {
		return errors.New("安装路径与配置存储路径不一致")
	}
	if err := ValidateConfig(cfg); err != nil {
		return err
	}
	credentials := Credentials{Version: 1, Token: token}
	if len(accountID) > 0 {
		credentials.AccountID = accountID[0]
	}
	if len(accountID) > 1 {
		return errors.New("只能设置一个账户 ID")
	}
	if err := validateCredentials(credentials); err != nil {
		return err
	}
	var snapshots []fileSnapshot
	files := []string{paths.ConfigFile(), paths.TokenFile(), paths.Binary}
	if systemd {
		files = append(files, paths.UnitFile)
	}
	for _, path := range files {
		s, err := snapshotFile(path)
		if err != nil {
			return err
		}
		snapshots = append(snapshots, s)
	}
	unitExisted := systemd && snapshots[len(snapshots)-1].existed
	var createdDirs []string
	serviceTouched := false
	defer func() {
		if result == nil {
			return
		}
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		var failures []error
		if systemd && serviceTouched {
			if err := run(rollbackCtx, "stop", "cfddns.service"); err != nil {
				failures = append(failures, err)
			}
			if !unitExisted {
				if err := run(rollbackCtx, "disable", "cfddns.service"); err != nil {
					failures = append(failures, err)
				}
			}
		}
		for i := len(snapshots) - 1; i >= 0; i-- {
			if err := snapshots[i].restore(); err != nil {
				failures = append(failures, err)
			}
		}
		if systemd && serviceTouched {
			if err := run(rollbackCtx, "daemon-reload"); err != nil {
				failures = append(failures, err)
			}
			if unitExisted {
				action := "disable"
				if prior.Enabled {
					action = "enable"
				}
				if err := run(rollbackCtx, action, "cfddns.service"); err != nil {
					failures = append(failures, err)
				}
				if prior.Active {
					if err := run(rollbackCtx, "start", "cfddns.service"); err != nil {
						failures = append(failures, err)
					}
				}
			}
		}
		for i := len(createdDirs) - 1; i >= 0; i-- {
			_ = os.Remove(createdDirs[i])
		}
		if len(failures) > 0 {
			result = errors.Join(result, fmt.Errorf("本地回滚未完整完成: %w", errors.Join(failures...)))
		}
	}()
	for _, dir := range []string{paths.ConfigDir, paths.StateDir} {
		_, err := os.Lstat(dir)
		if errors.Is(err, os.ErrNotExist) {
			createdDirs = append(createdDirs, dir)
		}
		if err := ensurePrivateDir(dir); err != nil {
			return err
		}
	}
	for _, dir := range []string{filepath.Dir(paths.Binary)} {
		if err := rejectSymlinkPath(dir); err != nil {
			return err
		}
		if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
			createdDirs = append(createdDirs, dir)
		}
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}
	if err := store.SaveConfig(cfg); err != nil {
		return err
	}
	if err := store.SaveCredentials(credentials); err != nil {
		return err
	}
	source, err := executable()
	if err != nil {
		return err
	}
	// Resolve a direct launcher symlink when present; an ordinary executable
	// needs no traversal of every parent through EvalSymlinks.
	st, err := os.Lstat(source)
	if err != nil {
		return fmt.Errorf("读取运行程序路径: %w", err)
	}
	if st.Mode()&os.ModeSymlink != 0 {
		source, err = filepath.EvalSymlinks(source)
		if err != nil {
			return fmt.Errorf("解析运行程序路径: %w", err)
		}
	}
	binary, err := readPrivateFile(source, 64<<20)
	if err != nil {
		return fmt.Errorf("读取运行程序: %w", err)
	}
	if err := atomicWrite(paths.Binary, binary, 0755, false); err != nil {
		return fmt.Errorf("安装程序: %w", err)
	}
	if !systemd {
		return nil
	}
	if err := atomicWrite(paths.UnitFile, []byte(buildServiceUnit(paths)), 0644, false); err != nil {
		return err
	}
	serviceTouched = true
	if err := run(ctx, "daemon-reload"); err != nil {
		return err
	}
	if !unitExisted {
		if err := run(ctx, "enable", "cfddns.service"); err != nil {
			return err
		}
		return run(ctx, "start", "cfddns.service")
	}
	// Reinstallation preserves both enabled and active state. An active daemon
	// reloads its new executable and config; a stopped daemon remains stopped.
	if prior.Active {
		return run(ctx, "restart", "cfddns.service")
	}
	return nil
}

func systemdQuote(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	s = strings.ReplaceAll(s, "%", "%%")
	s = strings.ReplaceAll(s, "$", "$$")
	return "\"" + s + "\""
}

func buildServiceUnit(paths Paths) string {
	return "[Unit]\n" +
		"Description=CFDDNS dynamic DNS service\nWants=network-online.target\nAfter=network-online.target\n\n" +
		"[Service]\nType=simple\nExecStart=" + systemdQuote(paths.Binary) + " --internal-daemon\n" +
		"Restart=on-failure\nRestartSec=10s\nTimeoutStopSec=35s\nUMask=0077\n" +
		"NoNewPrivileges=true\nProtectSystem=strict\nProtectHome=true\nPrivateTmp=true\n" +
		"ProtectKernelTunables=true\nProtectKernelModules=true\nProtectControlGroups=true\n" +
		"RestrictSUIDSGID=true\nRestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX AF_NETLINK\n" +
		"ReadWritePaths=" + systemdQuote(paths.ConfigDir) + " " + systemdQuote(paths.StateDir) + "\n" +
		"StandardOutput=journal\nStandardError=journal\n\n[Install]\nWantedBy=multi-user.target\n"
}
