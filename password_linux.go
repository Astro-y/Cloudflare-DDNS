//go:build linux

package main

import (
	"context"
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// readPasswordContext preserves canonical line editing and signal handling,
// while checking cancellation between short polls. It never closes shared stdin.
func readPasswordContext(ctx context.Context, input *os.File) (password []byte, result error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if input == nil {
		return nil, errors.New("凭据输入终端不可用")
	}
	fd := int(input.Fd())
	original, err := term.GetState(fd)
	if err != nil {
		return nil, errors.New("凭据输入需要交互式终端")
	}
	settings, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return nil, errors.New("无法读取终端设置")
	}
	changed := *settings
	changed.Lflag &^= unix.ECHO | unix.ECHONL
	changed.Lflag |= unix.ICANON | unix.ISIG
	changed.Iflag &^= unix.IGNCR | unix.INLCR
	changed.Iflag |= unix.ICRNL
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &changed); err != nil {
		return nil, errors.New("无法关闭终端凭据回显")
	}
	defer func() {
		if result != nil {
			clear(password)
			password = nil
			// Do not leave a partially entered secret for the shell or next menu.
			_ = unix.IoctlSetInt(fd, unix.TCFLSH, unix.TCIFLUSH)
		}
		if err := term.Restore(fd, original); err != nil {
			clear(password)
			password = nil
			result = errors.New("恢复终端回显失败")
		}
	}()
	var chunk [4096]byte
	defer clear(chunk[:])
	password = make([]byte, 0, 4096)
	for {
		if err := ctx.Err(); err != nil {
			return password, err
		}
		poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		_, err := unix.Poll(poll, 100)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return password, errors.New("读取凭据终端失败")
		}
		if err := ctx.Err(); err != nil {
			return password, err
		}
		if poll[0].Revents&unix.POLLIN == 0 {
			if poll[0].Revents&(unix.POLLHUP|unix.POLLERR|unix.POLLNVAL) != 0 {
				return password, io.EOF
			}
			continue
		}
		n, err := unix.Read(fd, chunk[:])
		if errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN) {
			continue
		}
		if err != nil {
			return password, errors.New("读取凭据输入失败")
		}
		if n == 0 {
			return password, io.EOF
		}
		if len(password)+n > 4096 {
			return password, errors.New("凭据长度超过限制")
		}
		password = append(password, chunk[:n]...)
		clear(chunk[:n])
		if password[len(password)-1] == '\n' || password[len(password)-1] == '\r' {
			for len(password) > 0 && (password[len(password)-1] == '\n' || password[len(password)-1] == '\r') {
				password[len(password)-1] = 0
				password = password[:len(password)-1]
			}
			if err := ctx.Err(); err != nil {
				return password, err
			}
			return password, nil
		}
	}
}
