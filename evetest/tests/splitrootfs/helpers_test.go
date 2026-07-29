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
	// extract and boot a split (universal) EVE image. It must cover the
	// download, the partition write, the reboot and the whole nodeagent testing
	// window (shortened to updateTestWindow below).
	splitUpgradeTimeout = 20 * time.Minute

	// updateTestWindow is the value we set for timer.test.baseimage.update, the
	// period nodeagent waits after booting the new partition before declaring
	// the update successful. The default is 10 minutes, which dominates the
	// runtime of this test. It cannot be cut much further: on the first boot of
	// the split image the Extension has to be self-healed out of the CAS, which
	// itself waits for the vault to be unlocked with a controller-escrowed key,
	// and nodeagent rolls the update back if extsloader is not Ready by the time
	// the window expires.
	updateTestWindow = 5 * time.Minute

	// shortSSHTimeout bounds a single quick shell command executed on the device.
	shortSSHTimeout = 30 * time.Second

	// extsloaderStatusFile is where the socketdriver materializes extsloader's
	// ExtsloaderStatus publication ("global" is the only key it ever uses).
	extsloaderStatusFile = "/run/extsloader/ExtsloaderStatus/global.json"

	// extsloaderStateReady mirrors types.ExtsloaderStateReady
	// (0=starting, 1=ready, 2=failed).
	extsloaderStateReady uint8 = 1
)

// upgradeToSplitImage drives an EVE base-OS update to a split (universal) image
// and waits for the target to reach its terminal state. It returns the EVE short
// version the device reports for the target image.
//
// The update goes through a registry datastore rather than the framework's
// default HTTP rootfs delivery: flattening the image to a single rootfs.img keeps
// only the Core, dropping the Extension (disk-0) layer, and never populates the
// containerd content-addressable store (CAS) that baseosmgr extracts the
// Extension from and extsloader self-heals it from.
//
// The wait is done here rather than by UpgradeEVE so that a stalled update dumps
// Extension diagnostics instead of just timing out.
// When expectRevert is true, the update is expected to be rejected and the
// device to roll back to the previous version.
func upgradeToSplitImage(t Gomega, device *evetest.EdgeDevice,
	targetVersion string, targetHypervisor evetest.Hypervisor,
	expectRevert bool) string {
	log := evetest.Logger()
	log.Infof("Updating base OS to split image %s (%s) via registry datastore",
		targetVersion, targetHypervisor)

	shortVersion := device.UpgradeEVE(targetVersion, targetHypervisor,
		false, expectRevert,
		evetest.WithUpgradeDelivery(evetest.UpgradeDeliveryOCIRegistry))
	log.Infof("Target split image reports EVE short version %q", shortVersion)

	waitForSplitBaseOS(t, device, shortVersion, expectRevert, splitUpgradeTimeout)
	return shortVersion
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
			// A bare timeout says nothing about which stage stalled, so dump the
			// Extension/vault state before failing.
			logExtensionDiagnostics(device)
			t.Expect(false).To(BeTrue(),
				"timed out after %s waiting for device to %s split image %s "+
					"(last state=%s, status=%s)",
				timeout, verb, targetShortVersion, lastState, lastStatus)
			return
		}
	}
}

// extensionDiagnosticsScript collects the state of every stage the
// monolith-to-split update depends on: which partition booted and its state, the
// Extension file on PERSIST, the verity mount, the vault (the CAS lives inside
// it, so a locked vault blocks self-heal), and the BaseOsStatus/ContentTreeStatus
// that self-heal resolves the CAS reference from.
const extensionDiagnosticsScript = `
echo "===== zboot ====="
zboot curpart 2>&1
for p in IMGA IMGB; do echo "$p: $(zboot partstate $p 2>&1)"; done
echo "===== extsloader status ====="
cat ` + extsloaderStatusFile + ` 2>&1
echo "===== extension image files ====="
ls -la /persist/ext-img*.img /persist/ext-img*.img.tmp 2>&1
echo "===== mount / verity ====="
mountpoint /persist/exts 2>&1
ls -la /dev/mapper/ 2>&1
echo "===== vault ====="
cat /run/vaultmgr/VaultStatus/*.json 2>&1
echo "===== baseos / content tree ====="
ls /run/baseosmgr/BaseOsStatus/ /run/volumemgr/ContentTreeStatus/ 2>&1
`

// logExtensionDiagnostics dumps extsloader's log and extensionDiagnosticsScript
// into the test log. Best-effort: it runs on a device that is already
// misbehaving, so failures are reported rather than propagated. The logs come
// from the controller-side stream, which still works when the Extension is down
// -- sshd ships in the Extension, so the shell probes are the part that may be
// unavailable in exactly the failure this is meant to explain.
func logExtensionDiagnostics(device *evetest.EdgeDevice) {
	log := evetest.Logger()

	for _, entry := range device.GetLogs(evetest.LogMsgMatch{Source: "extsloader"}) {
		log.Infof("extsloader log: %s %s", entry.Timestamp.Format(time.RFC3339), entry.Message)
	}

	out, _, err := device.RunShellScript(extensionDiagnosticsScript, 2*time.Minute, 0)
	if err != nil {
		log.Errorf("Failed to collect Extension diagnostics over ssh: %v", err)
		return
	}
	log.Infof("Extension diagnostics:\n%s", out)
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
	// Gomega aborts via runtime.Goexit, so the deferred dump still runs and
	// gives the failure some context.
	healthy := false
	defer func() {
		if !healthy {
			logExtensionDiagnostics(device)
		}
	}()

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

	healthy = true
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
