//go:build !linux

package main

import "context"

func InstallDownloadedRelease(context.Context, Paths, *Store, Config, Credentials, string) error {
	return RequireAdmin()
}
