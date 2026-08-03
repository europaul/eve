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
	brokenEVEVersionParamKey = "BROKEN_EVE_VERSION"

	// extensionCleanupTimeout bounds how long we allow for the Extension image of
	// the rejected partition to disappear from /persist after the rollback.
	extensionCleanupTimeout = 2 * time.Minute
)

// TestSplitBrokenExtensionRollback updates a device to a split image whose
// Extension cannot be verified, and verifies the device detects this and rolls
// itself back to the previous version without operator intervention.
//
// Objective:
//
//	This is the safety claim the whole Extension design rests on (product goal
//	G5): the Extension is measured with dm-verity, so a tampered or damaged one
//	must be refused rather than executed, and refusing it must not brick the
//	device. The image under test has a corrupted ext-verity-roothash baked into
//	the Core: the Extension file itself is byte-for-byte intact, so nothing
//	fails until dm-verity compares it against the expected root hash. That
//	models both a tampered Extension and a corrupted one with a single
//	artifact.
//
//	The expected sequence is: Core boots and reports in, extsloader finds the
//	Extension but the verity mount fails, extsloader never reaches Ready,
//	nodeagent's test window expires without TestComplete, and the device
//	reboots back onto the previous partition.
//
//	The test therefore asserts more than "the update failed". It asserts the
//	broken image was actually RUNNING first (PartitionState "inprogress"),
//	because an update that never downloaded would also produce a rollback and
//	would prove nothing about dm-verity. It then asserts the device is fully
//	recovered: back on the monolithic image, still running its app, still
//	reporting to the controller, and with the rejected partition's Extension
//	image cleaned off /persist rather than left to accumulate.
//
//	All of that is asserted while the controller is STILL requesting the broken
//	image, since nothing withdraws it. That is the realistic state -- an operator
//	does not retract a failed image the instant it fails -- and it is what makes
//	the recovery assertions meaningful rather than trivially satisfied.
//
// Network model:
//
//	SingleEthWithDHCP -- a single management Ethernet port with DHCP, enough to
//	reach the controller and the registry the split image is pulled from, and
//	to give the app an IP for SSH reachability checks.
//
// Device configuration:
//
//	One DHCP network on eth0 (mgmt + apps), one local network instance, and one
//	Ubuntu container app reachable over SSH. The update test window is shortened
//	to failedUpdateTestWindow: this update is meant to fail, so the window is
//	pure waiting.
//
// Phases:
//  1. Boot the device on the initial monolithic version, deploy the app and
//     verify it is reachable (checkpoint "pre-update").
//  2. Update the base OS to the broken split image and wait for the device to
//     roll back off it (checkpoint "rolled-back").
//  3. Assert the broken image did boot before being rejected.
//  4. Assert the device is back on the monolithic image, the rejected
//     partition's Extension image is gone, and the app and manageability
//     survived (checkpoint "rollback-verified").
//
// Parameters:
//   - BROKEN_EVE_VERSION: version of the split image with the corrupted
//     ext-verity-roothash (required). Build it with
//     tests/eden/prepare-broken-split-image.sh, which flips one hex character of
//     the root hash and repackages the Core under its own version, leaving the
//     good image and its tags untouched. Pass GOOD_VERSION=<existing build> to
//     reuse a split build instead of rebuilding from scratch, and HV_TAG=kvm so
//     the image is also published under the tag this test resolves.
//   - HYPERVISOR: hypervisor the broken image is fetched for (default: kvm).
//   - TPM: enable TPM emulation (default: true).
//   - DISK_SIZE_MB: device disk size in MiB (0 = framework default).
//   - INITIAL_EVE_VERSION: monolithic EVE version to start on and roll back to
//     (required; default "16.0.1-lts").
//   - INITIAL_HYPERVISOR: hypervisor of the monolithic version (default: kvm).
func TestSplitBrokenExtensionRollback(test *testing.T) {
	evetestT := evetest.Init(test)
	t := NewGomegaWithT(evetestT)
	defer evetest.Close()

	// Define configurable parameters available for the test.
	evetest.DefineTestParameters(
		evetest.HypervisorParameter(),
		evetest.TPMParameter(),
		evetest.DiskSizeMiBParameter(),
		evetest.TestParameterDefinition{
			Key: brokenEVEVersionParamKey,
			Description: evetest.TestParameterDescription{
				Summary: "Split EVE version with a corrupted ext-verity-roothash " +
					"(see tests/eden/prepare-broken-split-image.sh)",
			},
		},
		evetest.TestParameterDefinition{
			Key:          initialEVEVersionParamKey,
			DefaultValue: "16.0.1-lts",
			Description: evetest.TestParameterDescription{
				Summary: "Monolithic EVE version the device starts on and rolls back to",
				Default: "16.0.1-lts",
			},
		},
		evetest.TestParameterDefinition{
			Key:          initialHypervisorParamKey,
			DefaultValue: evetest.HypervisorKVM,
			Description: evetest.TestParameterDescription{
				Summary:       "Hypervisor used by the monolithic EVE version",
				Default:       "kvm",
				AllowedValues: "kvm|xen|kubevirt",
			},
		},
	)

	// Get parameter values set for this test execution.
	withTPM := evetest.GetTPMParameterValue()
	diskSizeMiB := evetest.GetDiskSizeMiBParameterValue()
	brokenHypervisor := evetest.GetHypervisorParameterValue()
	brokenVersion := evetest.GetTestParameter[string](brokenEVEVersionParamKey)
	if brokenVersion == "" {
		evetestT.Fatalf("%s%s is required for TestSplitBrokenExtensionRollback",
			constants.EnvPrefix, brokenEVEVersionParamKey)
	}
	monolithVersion := evetest.GetTestParameter[string](initialEVEVersionParamKey)
	if monolithVersion == "" {
		evetestT.Fatalf("%s%s is required for TestSplitBrokenExtensionRollback",
			constants.EnvPrefix, initialEVEVersionParamKey)
	}
	monolithHypervisor := evetest.GetTestParameter[evetest.Hypervisor](initialHypervisorParamKey)

	const devName = "edge-dev"
	evetest.Setup(
		evetest.RequireEdgeDevice{
			Name:              devName,
			WithEVEVersion:    monolithVersion,
			WithHypervisor:    monolithHypervisor,
			WithTPM:           withTPM,
			MinDiskSizeInMiB:  diskSizeMiB,
			DeviceReusePolicy: evetest.CreateFromScratchWithInstaller,
		},
		evetest.RequireNetworkModel{NetworkModel: netmodels.SingleEthWithDHCP},
	)
	device := evetest.GetEdgeDevice(devName)

	// Apply initial device config. The update is meant to fail, so the test
	// window is only as long as the Core needs to boot and report in.
	devConfig, appUUID := newOTATestDeviceConfig(devName, failedUpdateTestWindow)
	device.ApplyConfig(devConfig, false, false)

	assertAppReachable(t, device, appUUID, "before the broken split update")
	assertCoreIsMonolithic(t, device)

	evetest.Checkpoint("pre-update")

	// Update to the broken split image and wait for the device to reject it.
	brokenShortVersion, bootedBrokenImage := upgradeToSplitImage(
		t, device, brokenVersion, brokenHypervisor, true)

	evetest.Checkpoint("rolled-back")

	// The device must have actually run the broken image before rejecting it.
	// Without this, an update that never downloaded would look identical, and the
	// dm-verity refusal this test exists to prove would go untested.
	t.Expect(bootedBrokenImage).To(BeTrue(),
		"broken image %s was rejected without ever reaching the 'inprogress' "+
			"partition state, so the rollback does not demonstrate that "+
			"dm-verity refused the Extension", brokenShortVersion)

	// The rollback reboot is still in flight when the revert is reported, so wait
	// for the device to come back before probing it.
	waitForDeviceReachable(t, device, deviceReachableTimeout)

	// The device must be back on the monolithic image and manageable.
	assertCoreIsMonolithic(t, device)

	// nodeagent cleans the rejected partition's Extension image before rebooting
	// back, so /persist must not be left carrying it.
	t.Eventually(func(g Gomega) {
		out, _, err := device.RunShellScript(
			"ls /persist/ext-img*.img 2>/dev/null | wc -l", shortSSHTimeout, 0)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(strings.TrimSpace(out)).To(Equal("0"),
			"Extension image of the rejected partition was left on /persist")
	}, extensionCleanupTimeout, 10*time.Second).Should(Succeed())

	// The controller is deliberately left still requesting the broken image --
	// the state BASEIMAGE-UPDATE.md describes, where the device "will refuse to
	// try it since it remembers that it tried and failed". Nothing withdraws the
	// image first, because a real operator would not have done so yet, and doing
	// it here would skip past the window this test exists to cover: the device
	// must run its workloads and stay manageable while the failed image is still
	// requested.
	assertAppReachable(t, device, appUUID, "after the rollback")
	assertDeviceStillReporting(t, device, deviceReportingTimeout)

	evetest.Checkpoint("rollback-verified")
}
