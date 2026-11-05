//go:build !linux

package main

import (
	"context"
	"errors"
	"os"

	"golang.org/x/term"
)

func readPasswordContext(ctx context.Context, input *os.File) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if input == nil {
		return nil, errors.New("凭据输入终端不可用")
	}
	password, err := term.ReadPassword(int(input.Fd()))
	if err != nil {
		clear(password)
		return nil, errors.New("凭据输入需要交互式终端")
	}
	if err := ctx.Err(); err != nil {
		clear(password)
		return nil, err
	}
	return password, nil
}
