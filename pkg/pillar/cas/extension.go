// Copyright (c) 2026 Zededa, Inc.
// SPDX-License-Identifier: Apache-2.0

package cas

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/lf-edge/edge-containers/pkg/registry"
	"github.com/sirupsen/logrus"
)

// ErrNoExtensionDisk is returned by ExtractExtensionDisk when the image is
// monolithic or lacks the org.lfedge.eci.artifact.disk-0 label.
var ErrNoExtensionDisk = errors.New(
	"image carries no Extension disk (monolithic image or missing org.lfedge.eci.artifact.disk-0 label)")

// ExtractExtensionDisk pulls the Extension disk (disk-0) of the OCI image
// reference from CAS into targetPath. The pull lands in targetPath + ".tmp"
// and is renamed only once it is known to be non-empty, so a failed attempt
// never leaves a truncated Extension behind for the next attempt to find.
func ExtractExtensionDisk(casClient CAS, reference, targetPath string) error {
	ctrdCtx, done := casClient.CtrNewUserServicesCtx()
	defer done()

	resolver, err := casClient.Resolver(ctrdCtx)
	if err != nil {
		return fmt.Errorf("ExtractExtensionDisk: failed to get CAS resolver: %w", err)
	}

	tmpPath := targetPath + ".tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("ExtractExtensionDisk: failed to create %s: %w", tmpPath, err)
	}

	// The disk-0 config label routes the Extension into Disks[0].
	puller := registry.Puller{Image: reference}
	target := &registry.FilesTarget{Disks: []io.Writer{f}, AcceptHash: true}
	_, _, err = puller.Pull(target, 0, false, io.Discard, resolver)
	f.Close()
	if err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("ExtractExtensionDisk: pull failed for %s: %w", reference, err)
	}

	info, err := os.Stat(tmpPath)
	if err != nil || info.Size() == 0 {
		os.Remove(tmpPath)
		return fmt.Errorf("ExtractExtensionDisk: %s: %w", reference, ErrNoExtensionDisk)
	}

	if err := os.Rename(tmpPath, targetPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("ExtractExtensionDisk: failed to rename %s to %s: %w", tmpPath, targetPath, err)
	}

	logrus.Infof("ExtractExtensionDisk: wrote Extension (%d bytes) to %s", info.Size(), targetPath)
	return nil
}
