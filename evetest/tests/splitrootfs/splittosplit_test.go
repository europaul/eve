// Copyright (c) 2026 Zededa, Inc.
// SPDX-License-Identifier: Apache-2.0

package splitrootfs_test

import (
	"strings"
	"testing"
	"time"

	// revive:disable:dot-imports
	. "github.com/onsi/gomega"

	"github.com/lf-edge/eve/evetest"
	"github.com/lf-edge/eve/evetest/constants"
	"github.com/lf-edge/eve/evetest/netmodels"
)

const (
	// extMarkerFile is the version marker baked into the Extension of a v2 test
	// image by tests/eden/prepare-split-v2-image.sh, read through the mount.
	extMarkerFile = "/persist/exts/etc/eve-ext-release"

	// extCleanupTimeout bounds how long we allow baseosmgr to remove the Extension
	// image of the slot that is no longer in use.
	extCleanupTimeout = 3 * time.Minute

	// efiHVTypeVar is the EFI variable onboot.sh writes on first boot so that
	// later boots of a universal image can recover the hypervisor flavour, since
	// by then both partitions are stamped "uni".
	efiHVTypeVar = "eve-hv-type-7ad58f29-2b49-4f5a-9f0b-4e7bf7c2c311"
)

// TestSplitUpdateSplitToSplit updates a device from one split (universal) EVE
// image to another and verifies the Extension paired with the newly activated
// partition is the one that gets mounted.
//
// Objective:
//
//	Once a fleet is on split images, every subsequent update is split-to-split,
//	so this is the ordinary case rather than the migration case. It differs
//	from the monolith-to-split update in two ways worth testing separately.
//
//	First, the outgoing EVE is itself a split image, so its baseosmgr can
//	extract the incoming Extension for the target slot before rebooting. No CAS
//	self-heal should happen -- the opposite of the monolith-to-split path, and
//	asserted as such.
//
//	Second, the Extension is A/B-paired with the partition. The device holds an
//	ext image per slot, and after activating the new one it must mount THAT
//	slot's Extension and drop the other. To make that observable the two images
//	must carry different Extensions: with identical ones a device that mounted
//	the wrong slot's Extension would be indistinguishable from a correct one.
//	tests/eden/prepare-split-v2-image.sh therefore bakes a version marker into
//	v2's Extension, and this test reads it back through the mount and requires
//	it to name the version now running.
//
// Network model:
//
//	SingleEthWithDHCP -- a single management Ethernet port with DHCP, enough to
//	reach the controller and the registry the split images are pulled from, and
//	to give the app an IP for SSH reachability checks.
//
// Device configuration:
//
//	One DHCP network on eth0 (mgmt + apps), one local network instance, and one
//	Ubuntu container app reachable over SSH, to show workloads survive the
//	update.
//
// Phases:
//  1. Install split v1 and deploy the app; verify the device is split, the
//     Extension is healthy and the app is reachable (checkpoint "v1-installed").
//  2. Update the base OS to split v2 and wait until it boots active
//     (checkpoint "v2-active").
//  3. Assert the Extension came from baseosmgr rather than a CAS self-heal,
//     that the mounted Extension is v2's, and that only one ext image is left
//     on /persist.
//  4. Assert the universal image still resolves its hypervisor on a second OTA,
//     where both partitions are "uni" and the answer can only come from the EFI
//     variable written on first boot (checkpoint "v2-verified").
//
// Parameters:
//   - INITIAL_EVE_VERSION: the split version to install first (v1, required).
//   - EVE_VERSION: the split version to update to (v2, required). Build it with
//     tests/eden/prepare-split-v2-image.sh, which gives its Extension a distinct
//     version marker; without that marker the pairing assertion cannot run.
//   - HYPERVISOR: hypervisor both images run as (default: kvm).
//   - TPM: enable TPM emulation (default: true).
//   - DISK_SIZE_MB: device disk size in MiB (0 = framework default).
func TestSplitUpdateSplitToSplit(test *testing.T) {
	evetestT := evetest.Init(test)
	t := NewGomegaWithT(evetestT)
	defer evetest.Close()

	// Define configurable parameters available for the test.
	evetest.DefineTestParameters(
		evetest.EVEVersionParameter(),
		evetest.HypervisorParameter(),
		evetest.TPMParameter(),
		evetest.DiskSizeMiBParameter(),
		evetest.TestParameterDefinition{
			Key: initialEVEVersionParamKey,
			Description: evetest.TestParameterDescription{
				Summary: "Split (universal) EVE version to install first (v1)",
			},
		},
	)

	// Get parameter values set for this test execution.
	withTPM := evetest.GetTPMParameterValue()
	diskSizeMiB := evetest.GetDiskSizeMiBParameterValue()
	hypervisor := evetest.GetHypervisorParameterValue()
	v2Version := evetest.GetEVEVersionParameterValue()
	v1Version := evetest.GetTestParameter[string](initialEVEVersionParamKey)
	if v1Version == "" {
		evetestT.Fatalf("%s%s is required for TestSplitUpdateSplitToSplit",
			constants.EnvPrefix, initialEVEVersionParamKey)
	}
	if v1Version == v2Version {
		evetestT.Fatalf("v1 and v2 must differ, both are %q", v1Version)
	}

	const devName = "edge-dev"
	evetest.Setup(
		evetest.RequireEdgeDevice{
			Name:              devName,
			WithEVEVersion:    v1Version,
			WithHypervisor:    hypervisor,
			WithTPM:           withTPM,
			MinDiskSizeInMiB:  diskSizeMiB,
			DeviceReusePolicy: evetest.CreateFromScratchWithInstaller,
		},
		evetest.RequireNetworkModel{NetworkModel: netmodels.SingleEthWithDHCP},
	)
	device := evetest.GetEdgeDevice(devName)

	// Apply initial device config: management adapter, a local network instance,
	// and a container app reachable over SSH.
	devConfig, appUUID := newOTATestDeviceConfig(devName, updateTestWindow)
	device.ApplyConfig(devConfig, false, false)

	assertAppReachable(t, device, appUUID, "before the split-to-split update")

	// The device must start out split and healthy -- the installer places the
	// Extension on /persist directly, so no self-heal is involved here either.
	assertCoreExpectsExtension(t, device)
	assertExtensionHealthy(t, device, extensionHealthTimeout)

	// Record which Extension v1 has mounted, purely for the failure story if the
	// update does not swap it. v1 is an ordinary build and need not carry the
	// marker, so this is logged rather than asserted.
	if out, _, err := device.RunShellScript(
		"cat "+extMarkerFile+" 2>/dev/null || echo '(no marker)'",
		shortSSHTimeout, 0); err == nil {
		evetest.Logger().Infof("Extension marker before the update: %s",
			strings.TrimSpace(out))
	}

	evetest.Checkpoint("v1-installed")

	// Update to split v2 (expect success, no revert).
	v2ShortVersion, _ := upgradeToSplitImage(t, device, v2Version, hypervisor, false)

	evetest.Checkpoint("v2-active")

	// Still split, with a healthy verity-backed Extension.
	assertCoreExpectsExtension(t, device)
	assertExtensionHealthy(t, device, extensionHealthTimeout)

	// The outgoing EVE was split, so its baseosmgr pre-extracted the incoming
	// Extension; nothing should have needed the CAS self-heal path.
	assertExtensionPreExtracted(t, device)

	// The mounted Extension must be the one paired with the newly activated
	// partition, i.e. v2's. This is the assertion the differing Extensions exist
	// to make possible.
	markerOut, _, err := device.RunShellScript(
		"cat "+extMarkerFile+" 2>/dev/null || echo '(missing)'", shortSSHTimeout, 0)
	t.Expect(err).NotTo(HaveOccurred())
	t.Expect(strings.TrimSpace(markerOut)).To(Equal(v2ShortVersion),
		"the Extension mounted at /persist/exts reports %q but the device is "+
			"running %q, so the wrong slot's Extension is mounted (or v2 was not "+
			"built with tests/eden/prepare-split-v2-image.sh, which adds the marker)",
		strings.TrimSpace(markerOut), v2ShortVersion)

	// After a successful activation baseosmgr drops the now-unused slot's
	// Extension, so /persist must be left holding exactly one.
	t.Eventually(func(g Gomega) {
		out, _, err := device.RunShellScript(
			"ls /persist/ext-img*.img 2>/dev/null | wc -l", shortSSHTimeout, 0)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(strings.TrimSpace(out)).To(Equal("1"),
			"expected exactly one Extension image on /persist after activation")
	}, extCleanupTimeout, 10*time.Second).Should(Succeed())

	// On a second OTA both partitions are stamped "uni", so GRUB cannot infer the
	// hypervisor from the image alone -- it has to recover it from the EFI
	// variable written on first boot. A regression here would silently leave the
	// device running the wrong flavour, or leak the "uni" sentinel to runtime.
	hvOut, _, err := device.RunShellScript(
		"cat /run/eve-hv-type 2>/dev/null || echo missing", shortSSHTimeout, 0)
	t.Expect(err).NotTo(HaveOccurred())
	runtimeHV := strings.TrimSpace(hvOut)
	t.Expect(runtimeHV).To(Equal(hypervisor.String()),
		"runtime hypervisor is %q after the split-to-split update", runtimeHV)

	// Also assert the EFI variable holds the right flavour. Note this does NOT
	// mean GRUB resolved from it: set_eve_flavor in pkg/grub/rootfs.cfg reads the
	// CONFIG partition's eve-hv-type first and only falls back to the EFI
	// variable when that is absent, and the installer writes CONFIG at install
	// time. So on an installed device the variable is the fallback path's
	// precondition rather than the path actually taken -- worth keeping correct,
	// since a device whose CONFIG lacks the file depends on it entirely.
	efiOut, _, err := device.RunShellScript(
		`for d in /sys /hostfs/sys; do `+
			`f="$d/firmware/efi/efivars/`+efiHVTypeVar+`"; `+
			`[ -r "$f" ] && { dd if="$f" bs=1 skip=4 2>/dev/null | tr -d "\0"; echo; exit 0; }; `+
			`done; echo unreadable`,
		shortSSHTimeout, 0)
	t.Expect(err).NotTo(HaveOccurred())
	t.Expect(strings.TrimSpace(efiOut)).To(Equal(hypervisor.String()),
		"EFI variable eve-hv-type reads %q, expected %s",
		strings.TrimSpace(efiOut), hypervisor.String())

	// eve_hv_type= appears on the kernel command line whenever set_eve_flavor
	// resolved a flavour, whatever the source, so its presence says nothing about
	// which source won. Logged for diagnostics only. (Eden asserts the opposite
	// -- that it is absent once the EFI variable exists -- which does not hold
	// for a device installed with eve-hv-type in CONFIG.)
	if out, _, cmdErr := device.RunShellScript(
		`tr " " "\n" < /proc/cmdline | grep "^eve_hv_type=" || echo "(not on cmdline)"`,
		shortSSHTimeout, 0); cmdErr == nil {
		evetest.Logger().Infof("eve_hv_type on kernel cmdline: %s", strings.TrimSpace(out))
	}

	assertAppReachable(t, device, appUUID, "after the split-to-split update")

	evetest.Checkpoint("v2-verified")
}
