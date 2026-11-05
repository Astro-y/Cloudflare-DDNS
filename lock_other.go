//go:build !linux && !windows

package main

import (
	"errors"
	"os"
)

func tryFileLock(*os.File) (bool, error) {
	return false, errors.New("此平台不支持进程锁，请使用 Linux")
}
func unlockFile(*os.File)                        {}
func openReadFile(path string) (*os.File, error) { return os.Open(path) }
func replaceFile(from, to string) error          { return os.Rename(from, to) }
