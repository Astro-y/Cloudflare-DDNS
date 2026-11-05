//go:build linux

package main

import "context"

// The panel holds the shared operation lock while the transaction replaces the
// binary and restarts an active service. Downloading happens before taking it.
func InstallDownloadedRelease(ctx context.Context, paths Paths, store *Store, cfg Config, credentials Credentials, artifact string) error {
	if err := RequireAdmin(); err != nil {
		return err
	}
	return installDownloadedRelease(ctx, paths, store, cfg, credentials, artifact, HasSystemd(), GetServiceStatus(), systemctl)
}
