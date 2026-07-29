// Copyright (c) 2026 Zededa, Inc.
// SPDX-License-Identifier: Apache-2.0

package splitrootfs_test

import (
	"encoding/json"
	"strings"
	"time"

	// revive:disable:dot-imports
	. "github.com/onsi/gomega"

	eveinfo "github.com/lf-edge/eve-api/go/info"
	"github.com/lf-edge/eve/evetest"
)

const (
	// splitUpgradeTimeout bounds how long we wait for the device to fetch,
	// extract and boot a split (universal) EVE image. Extracting the Extension
	// (disk-0) layer and self-healing it into the CAS can take a while on a
	// slow/virtualized target, so the timeout is generous.
	splitUpgradeTimeout = 30 * time.Minute

	// shortSSHTimeout bounds a single quick shell command executed on the device.
	shortSSHTimeout = 30 * time.Second

	// extsloaderStatusFile is where the socketdriver materializes extsloader's
	// ExtsloaderStatus publication ("global" is the only key it ever uses).
	extsloaderStatusFile = "/run/extsloader/ExtsloaderStatus/global.json"

	// extsloaderStateReady mirrors types.ExtsloaderStateReady
	// (0=starting, 1=ready, 2=failed).
	extsloaderStateReady uint8 = 1
)

// upgradeToSplitImage drives an EVE base-OS update to a split (universal) OCI
// image and waits for the target to reach its terminal state.
//
// It deliberately does NOT use the framework's built-in EdgeDevice.UpgradeEVE.
// UpgradeEVE flattens the image into a single rootfs.img served over plain HTTP,
// which drops the Extension (disk-0) layer and never populates the containerd
// content-addressable store (CAS). Split-rootfs instead needs the whole OCI
// image delivered through a registry datastore, so that baseosmgr can extract
// the Extension layer and extsloader can CAS-self-heal it if needed. We
// therefore drive the update directly via SetBaseOS(DockerContainer{...}).
//
// expectedShortVersion is the EVE short version the device is expected to report
// for the target image (for universal split images this equals the OCI tag).
// When expectRevert is true, the update is expected to be rejected and the
// device to roll back to the previous version.
func upgradeToSplitImage(t Gomega, device *evetest.EdgeDevice,
	imageDomain, imageRepo, imageTag, expectedShortVersion string,
	expectRevert bool) {
	log := evetest.Logger()
	log.Infof("Updating base OS to split image %s/%s:%s (expected version %q)",
		imageDomain, imageRepo, imageTag, expectedShortVersion)

	config := device.GetConfig()
	config.SetBaseOS(evetest.DockerContainer{
		Domain:    imageDomain,
		ImageName: imageRepo,
		Tag:       imageTag,
	}, expectedShortVersion)
	device.ApplyConfig(config, false, false)

	waitForSplitBaseOS(t, device, expectedShortVersion, expectRevert, splitUpgradeTimeout)
}

// waitForSplitBaseOS blocks until the device reaches the terminal state of a
// split base-OS update, or fails the test on timeout.
//
// It mirrors the logic of the framework's private waitForUpgrade/waitForRevert,
// but uses the public WatchDeviceInfo channel. It scans each device-info
// message's SwList (info.GetSwList()) for the entry matching targetShortVersion:
//   - success path: returns once that entry reports PartitionState=="active";
//     fails immediately if it reports UserStatus==FAILED.
//   - revert path: returns once that entry reports UserStatus==FAILED (the
//     target was rejected and the device rolled back to the previous version).
func waitForSplitBaseOS(t Gomega, device *evetest.EdgeDevice,
	targetShortVersion string, expectRevert bool, timeout time.Duration) {
	log := evetest.Logger()
	updates, stop := device.WatchDeviceInfo()
	defer stop()

	deadline := time.After(timeout)
	var lastState, lastStatus string
	for {
		select {
		case info, ok := <-updates:
			if !ok {
				t.Expect(ok).To(BeTrue(),
					"device-info watch closed unexpectedly while waiting for %s",
					targetShortVersion)
				return
			}
			for _, sw := range info.GetSwList() {
				if sw.GetShortVersion() != targetShortVersion {
					continue
				}
				status := sw.GetUserStatus()
				partState := sw.GetPartitionState()
				if expectRevert {
					if status == eveinfo.BaseOsStatus_FAILED {
						log.Infof("Device reverted from split image %s: %s",
							targetShortVersion, sw.GetSubStatusStr())
						return
					}
				} else {
					t.Expect(status).NotTo(Equal(eveinfo.BaseOsStatus_FAILED),
						"split base-OS update to %s failed: %s",
						targetShortVersion, sw.GetSubStatusStr())
					if partState == "active" {
						log.Infof("Device booted split image %s on the active partition",
							targetShortVersion)
						return
					}
				}
				if partState != lastState || status.String() != lastStatus {
					log.Infof("Split base-OS update in progress (state=%s, status=%s)",
						partState, status.String())
					lastState, lastStatus = partState, status.String()
				}
			}
		case <-deadline:
			verb := "boot"
			if expectRevert {
				verb = "revert from"
			}
			t.Expect(false).To(BeTrue(),
				"timed out after %s waiting for device to %s split image %s",
				timeout, verb, targetShortVersion)
			return
		}
	}
}

// extsloaderStatus mirrors the fields of types.ExtsloaderStatus that this test
// asserts on. It is declared locally rather than imported because the evetest
// container is compiled against a pinned published pkg/pillar that predates the
// type (see assertExtensionHealthy).
type extsloaderStatus struct {
	State     uint8
	Reason    string
	Partition string
	ImagePath string
}

// readExtsloaderStatus reads and decodes extsloader's published status.
func readExtsloaderStatus(g Gomega, device *evetest.EdgeDevice) extsloaderStatus {
	var status extsloaderStatus
	g.Expect(device.FileExists(extsloaderStatusFile)).To(BeTrue(),
		"%s does not exist -- extsloader published no status", extsloaderStatusFile)
	g.Expect(json.Unmarshal(device.ReadFile(extsloaderStatusFile), &status)).To(Succeed())
	return status
}

// assertExtensionHealthy asserts that the Extension (disk-0) layer is loaded and
// healthy on the device: it is mounted read-only at /persist/exts, the mount is
// dm-verity-backed, and extsloader reports it started all Extension services.
//
// The checks are shell probes (device.RunShellScript) rather than an API-level
// read of the ExtsloaderStatus pubsub object. That is deliberate: evetest is a
// separate Go module whose container is compiled against a PINNED published
// pkg/pillar that predates types.ExtsloaderStatus, and the container mounts only
// evetest/tests (not the local pkg/pillar), so importing the new type does not
// build where the test actually runs. Reading the published JSON over ssh is the
// closest we get to an API-level assertion until that type is published.
// Everything in EVE is asynchronous, so the probes are wrapped in Eventually
// (extsloader may still be mounting/self-healing the Extension right after boot).
func assertExtensionHealthy(t Gomega, device *evetest.EdgeDevice, timeout time.Duration) {
	t.Eventually(func(g Gomega) {
		// Extension mounted at the expected point.
		mnt, _, err := device.RunShellScript(
			"mountpoint -q /persist/exts && echo mounted || echo no",
			shortSSHTimeout, 0)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(mnt).To(ContainSubstring("mounted"),
			"Extension is not mounted at /persist/exts")

		// The Extension mount is backed by a dm-verity device.
		ver, _, err := device.RunShellScript(
			"ls /dev/mapper/exts-verity-* >/dev/null 2>&1 && echo verity || echo no",
			shortSSHTimeout, 0)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(ver).To(ContainSubstring("verity"),
			"Extension is not backed by a dm-verity device")

		// extsloader published its terminal Ready state. This reads the
		// ExtsloaderStatus pubsub object -- the same signal nodeagent gates
		// update success on -- rather than scraping logs.
		status := readExtsloaderStatus(g, device)
		g.Expect(status.State).To(Equal(extsloaderStateReady),
			"extsloader state is %d (want %d=ready), reason: %q",
			status.State, extsloaderStateReady, status.Reason)
	}, timeout, 10*time.Second).Should(Succeed())
}

// assertCoreExpectsExtension asserts that the running core rootfs is a split
// image, i.e. it ships the ext-verity-roothash marker that tells the core to
// expect and mount a separate Extension.
func assertCoreExpectsExtension(t Gomega, device *evetest.EdgeDevice) {
	out, _, err := device.RunShellScript(
		"test -f /hostfs/etc/ext-verity-roothash && echo split || echo monolithic",
		shortSSHTimeout, 0)
	t.Expect(err).NotTo(HaveOccurred())
	t.Expect(strings.TrimSpace(out)).To(Equal("split"),
		"running core does not expect an Extension (not a split image)")
}

// assertExtensionSelfHealed asserts that the Extension image was recovered from
// the CAS by extsloader (the self-heal path). This is expected when upgrading
// from a monolithic EVE whose baseosmgr cannot pre-extract the Extension, so
// extsloader must reconstruct it from the CAS on first boot of the split image.
//
// It proves this from the Extension file's provenance rather than from a log
// entry: the file must have been created *after* the split image booted, since a
// pre-extracted Extension would have been written by the previous EVE before the
// reboot. Provenance is durable state; the log entry is not. extsloader logs the
// self-heal exactly once, within the first minute of boot, and that window
// survives neither the on-device ring buffer (~5000 lines, which the Extension
// services' own debug logging churns through in about a minute) nor the log
// stream shipped to the controller (which only starts once newlogd is up).
//
// TODO: assert on an ExtsloaderStatus.Source pubsub field instead, once pillar
// records how the Extension was obtained, making provenance explicit rather
// than inferred.
func assertExtensionSelfHealed(t Gomega, device *evetest.EdgeDevice) {
	status := readExtsloaderStatus(t, device)
	t.Expect(status.ImagePath).NotTo(BeEmpty(),
		"extsloader published no Extension image path")

	// btime is the kernel boot wall-clock time in seconds since the epoch.
	out, _, err := device.RunShellScript(
		`boot=$(grep '^btime ' /proc/stat | cut -d' ' -f2); `+
			`mtime=$(stat -c %Y `+status.ImagePath+`); `+
			`echo "boot=$boot mtime=$mtime"; `+
			`[ "$mtime" -gt "$boot" ] && echo self-healed || echo pre-existing`,
		shortSSHTimeout, 0)
	t.Expect(err).NotTo(HaveOccurred())
	t.Expect(out).To(ContainSubstring("self-healed"),
		"Extension %s predates this boot, so it was not self-healed from the CAS (%s)",
		status.ImagePath, strings.TrimSpace(out))
}
