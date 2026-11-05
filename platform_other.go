//go:build !linux

package main

import (
	"context"
	"errors"
	"os"
)

func IsAdmin() bool       { return false }
func RequireAdmin() error { return errors.New("安装和后台服务仅支持 Linux amd64/arm64") }
func HasSystemd() bool    { return false }

type ServiceStatus struct {
	Active      bool
	Enabled     bool
	Description string
}

func GetServiceStatus() ServiceStatus {
	return ServiceStatus{Description: "当前平台不支持 Linux 服务"}
}
func ServiceAction(context.Context, string) error { return RequireAdmin() }
func ReadLogs(context.Context) (string, error)    { return "", RequireAdmin() }
func InstallLocal(context.Context, Paths, *Store, Config, string, ...string) error {
	return RequireAdmin()
}
func UninstallLocal(context.Context, Paths, bool) error { return RequireAdmin() }
func openLockFile(path string) (*os.File, error) {
	if err := rejectSymlinkPath(path); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
}
func syncDirectory(string) error { return nil }
