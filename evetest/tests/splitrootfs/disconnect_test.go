// Copyright (c) 2026 Zededa, Inc.
// SPDX-License-Identifier: Apache-2.0

package splitrootfs_test

import (
	"testing"
	"time"

	// revive:disable:dot-imports
	. "github.com/onsi/gomega"

	"github.com/lf-edge/eve/evetest"
	"github.com/lf-edge/eve/evetest/constants"
	"github.com/lf-edge/eve/evetest/netmodels"
	"github.com/lf-edge/eve/pkg/pillar/types"
)

const (
	// cloudGoneFallback is the value we set for timer.update.fallback.no.network:
	// how long the device tolerates losing the controller while testing a new
	// image before presuming that image is what broke connectivity. The default
	// is 5 minutes and the enforced floor is 1, so this is as fast as the
	// scenario can be made.
	cloudGoneFallback = time.Minute

	// rollbackAfterDisconnectTimeout bounds how long we wait for the device to
	// give up on the new image and come back on the old one. It must cover the
	// fallback timer, the reboot and pillar restarting.
	rollbackAfterDisconnectTimeout = 10 * time.Minute

	// disconnectTestWindow is deliberately longer than the windows the other
	// tests use. The device must still be under test when connectivity is cut,
	// and getting there means waiting for the Extension to mount and sshd to
	// come up; a short window could expire first and commit the update, so the
	// test would report a fallback that never happened.
	disconnectTestWindow = 15 * time.Minute
)

// TestSplitUpdateDisconnectRollback interrupts the device's access to the
// controller while it is testing a freshly booted split image, and verifies the
// device falls back to the previous version on its own.
//
// Objective:
//
//	An update that boots but silently costs the device its manageability is the
//	worst outcome of all: nothing is left to push a fix with. EVE guards against
//	it with a timer -- if the controller stays unreachable for
//	timer.update.fallback.no.network while an update is under test, the new
//	image is presumed responsible and the device reboots back onto the previous
//	partition (BootReasonFallback).
//
//	This test drives that guard for a split image specifically, because a split
//	image has more ways to lose connectivity than a monolithic one: the Core
//	carries the network stack while much of the diagnostic and support
//	machinery lives in the Extension, so a device can reach the point of running
//	the new Core and still fail to talk to anyone.
//
//	The disconnect is imposed with an SDN firewall rule that drops only
//	controller-bound traffic, so the harness keeps its SSH path to the device
//	and can watch the rollback happen. That matters: with the controller
//	unreachable, the device cannot report what it is doing, so the rollback is
//	observed on the device itself rather than through the controller.
//
// Network model:
//
//	SingleEthWithDHCP, re-applied mid-test with a firewall rule dropping traffic
//	to the controller and then re-applied again without it.
//
// Device configuration:
//
//	One DHCP network on eth0 (mgmt + apps), one local network instance, and one
//	Ubuntu container app reachable over SSH. timer.update.fallback.no.network is
//	set to its one-minute floor so the fallback fires promptly.
//
// Phases:
//  1. Boot the device on the monolithic version, deploy the app and verify it
//     is reachable (checkpoint "pre-update").
//  2. Start the update to the split image and wait until the device has booted
//     it and is inside its test window (checkpoint "split-booted").
//  3. Cut controller access and wait, over SSH, for the device to roll back to
//     the monolithic image on its own (checkpoint "rolled-back").
//  4. Restore controller access and assert the device is manageable again, the
//     app survived, and the update is reported as failed rather than left
//     half-applied (checkpoint "recovery-verified").
//
// Parameters:
//   - EVE_VERSION: the split (universal) EVE version to update to.
//   - HYPERVISOR: target hypervisor of the split image (default: kvm).
//   - TPM: enable TPM emulation (default: true).
//   - DISK_SIZE_MB: device disk size in MiB (0 = framework default).
//   - INITIAL_EVE_VERSION: monolithic EVE version to start on and fall back to
//     (required; default "16.0.1-lts").
//   - INITIAL_HYPERVISOR: hypervisor of the monolithic version (default: kvm).
func TestSplitUpdateDisconnectRollback(test *testing.T) {
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
			Key:          initialEVEVersionParamKey,
			DefaultValue: "16.0.1-lts",
			Description: evetest.TestParameterDescription{
				Summary: "Monolithic EVE version the device starts on and falls back to",
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
	splitVersion := evetest.GetEVEVersionParameterValue()
	splitHypervisor := evetest.GetHypervisorParameterValue()
	monolithVersion := evetest.GetTestParameter[string](initialEVEVersionParamKey)
	if monolithVersion == "" {
		evetestT.Fatalf("%s%s is required for TestSplitUpdateDisconnectRollback",
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

	// The update test window is left long enough that it is NOT what ends the
	// update: the fallback timer, set to its one-minute floor, must be the thing
	// that fires, or the test would pass for the wrong reason.
	//
	// Only the fallback timer is added here. SetConfigProperties appends to the
	// config-item list rather than replacing it, so re-setting a key that
	// newOTATestDeviceConfig already set would emit it twice with no defined
	// precedence.
	devConfig, appUUID := newOTATestDeviceConfig(devName, disconnectTestWindow)
	fallbackProps := types.NewConfigItemValueMap()
	fallbackProps.SetGlobalValueInt(types.FallbackIfCloudGoneTime,
		uint32(cloudGoneFallback.Seconds()))
	devConfig.SetConfigProperties(fallbackProps)
	device.ApplyConfig(devConfig, false, false)

	assertAppReachable(t, device, appUUID, "before the split update")
	assertCoreIsMonolithic(t, device)

	evetest.Checkpoint("pre-update")

	// Start the update but do not wait for a verdict: the point is to interfere
	// while it is still under test.
	splitShortVersion := device.UpgradeEVE(splitVersion, splitHypervisor,
		false, true, evetest.WithUpgradeDelivery(evetest.UpgradeDeliveryOCIRegistry))
	evetest.Logger().Infof("Target split image reports EVE short version %q",
		splitShortVersion)

	waitUntilTargetBooted(t, device, splitShortVersion, baseOSUpdateTimeout)

	// "inprogress" is reported through the controller well before the device can
	// be reached over SSH -- on a split image sshd ships in the Extension, so it
	// only appears once extsloader has mounted it. Wait for that before probing,
	// and so that the disconnect below is imposed on a fully-started device
	// rather than one still coming up.
	waitForDeviceReachable(t, device, deviceReachableTimeout)
	assertCoreExpectsExtension(t, device)
	assertExtensionHealthy(t, device, extensionHealthTimeout)

	evetest.Checkpoint("split-booted")

	// Cut the controller off. From here the device cannot report anything, so
	// the rollback is observed directly on the device.
	setControllerReachable(false)

	// The device should give up on the new image and come back monolithic. This
	// polls over SSH and tolerates the reboot in the middle, when the device is
	// briefly unreachable.
	evetest.Logger().Infof("Waiting for the device to fall back to %s",
		monolithVersion)
	t.Eventually(func(g Gomega) {
		out, _, err := device.RunShellScript(
			"test -f /hostfs/etc/ext-verity-roothash && echo split || echo monolithic",
			shortSSHTimeout, 0)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(out).To(ContainSubstring("monolithic"),
			"device is still running the split image")
	}, rollbackAfterDisconnectTimeout, 15*time.Second).Should(Succeed())

	evetest.Checkpoint("rolled-back")

	// Give the controller back and confirm the device is fully manageable again,
	// which is the property the fallback exists to protect.
	setControllerReachable(true)
	waitForDeviceReachable(t, device, deviceReachableTimeout)
	assertCoreIsMonolithic(t, device)
	assertDeviceStillReporting(t, device, deviceReportingTimeout)

	// The device must also tell the controller the update failed, rather than
	// leaving it believing the new version is on its way.
	waitForBaseOSUpdate(t, device, splitShortVersion, true, baseOSUpdateTimeout)

	assertAppReachable(t, device, appUUID, "after the fallback to monolithic")

	evetest.Checkpoint("recovery-verified")
}
