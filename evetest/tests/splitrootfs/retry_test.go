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

// TestSplitUpdateRetryAfterFailure makes a split update fail for a transient
// reason, then retries the same image and verifies it installs.
//
// Objective:
//
//	A device that has rejected an image will not install that image again, even
//	though the controller keeps asking for it -- it remembers the failure. That
//	is deliberate: without it, a genuinely bad image would put the device in an
//	upgrade-fail-upgrade loop. But it means a failure with a transient cause,
//	such as a network outage during the test window, leaves an update that can
//	never complete on its own.
//
//	The way out is the retry counter in the base-OS configuration. baseosmgr
//	re-attempts a rejected image only when that counter changes, and persists
//	the new value so the loop still cannot happen. This test drives that path:
//	the failure is caused by losing the controller (the same mechanism as the
//	disconnect test, so the image itself is known-good), and after connectivity
//	returns the counter is bumped and the update is expected to succeed.
//
//	The point is that the SAME image installs the second time. A test that
//	simply pushed a different image afterwards would not touch the
//	refuse-until-retried logic at all.
//
// Network model:
//
//	SingleEthWithDHCP, re-applied mid-test with a firewall rule dropping traffic
//	to the controller, then re-applied again without it.
//
// Device configuration:
//
//	One DHCP network on eth0 (mgmt + apps), one local network instance, and one
//	Ubuntu container app reachable over SSH. timer.update.fallback.no.network is
//	set to its one-minute floor so the induced failure happens promptly.
//
// Phases:
//  1. Boot on the monolithic version, deploy the app (checkpoint "pre-update").
//  2. Start the update to the split image, wait until it is running, then cut
//     controller access so the device falls back (checkpoint "update-failed").
//  3. Restore connectivity and confirm the device settled back on the
//     monolithic image with the update reported failed.
//  4. Bump the retry counter and verify the same split image now installs and
//     becomes active, with a healthy Extension and the app still running
//     (checkpoint "retry-succeeded").
//
// Parameters:
//   - EVE_VERSION: the split (universal) EVE version to update to.
//   - HYPERVISOR: target hypervisor of the split image (default: kvm).
//   - TPM: enable TPM emulation (default: true).
//   - DISK_SIZE_MB: device disk size in MiB (0 = framework default).
//   - INITIAL_EVE_VERSION: monolithic EVE version to start on (required;
//     default "16.0.1-lts").
//   - INITIAL_HYPERVISOR: hypervisor of the monolithic version (default: kvm).
func TestSplitUpdateRetryAfterFailure(test *testing.T) {
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
				Summary: "Monolithic EVE version the device starts on",
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
		evetestT.Fatalf("%s%s is required for TestSplitUpdateRetryAfterFailure",
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

	// As in the disconnect test, the window is long enough that the fallback
	// timer is what ends the first attempt. Only the fallback timer is added
	// here, because SetConfigProperties appends rather than replaces.
	devConfig, appUUID := newOTATestDeviceConfig(devName, disconnectTestWindow)
	fallbackProps := types.NewConfigItemValueMap()
	fallbackProps.SetGlobalValueInt(types.FallbackIfCloudGoneTime,
		uint32(cloudGoneFallback.Seconds()))
	devConfig.SetConfigProperties(fallbackProps)
	device.ApplyConfig(devConfig, false, false)

	assertAppReachable(t, device, appUUID, "before the split update")
	assertCoreIsMonolithic(t, device)

	evetest.Checkpoint("pre-update")

	// First attempt: start the update, let it boot, then take the controller
	// away so the device gives up on an otherwise-good image.
	splitShortVersion := device.UpgradeEVE(splitVersion, splitHypervisor,
		false, true, evetest.WithUpgradeDelivery(evetest.UpgradeDeliveryOCIRegistry))
	waitUntilTargetBooted(t, device, splitShortVersion, baseOSUpdateTimeout)
	waitForDeviceReachable(t, device, deviceReachableTimeout)

	setControllerReachable(false)
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

	setControllerReachable(true)
	waitForDeviceReachable(t, device, deviceReachableTimeout)
	waitForBaseOSUpdate(t, device, splitShortVersion, true, baseOSUpdateTimeout)
	assertCoreIsMonolithic(t, device)

	evetest.Checkpoint("update-failed")

	// Second attempt: the controller is still asking for the same image, and the
	// device is still refusing it. Bumping the retry counter is what releases it.
	device.RetryEVEUpgrade(false, false)
	waitForBaseOSUpdate(t, device, splitShortVersion, false, baseOSUpdateTimeout)

	// The retried image must come up properly, not merely boot: same standard as
	// a first-time split update.
	waitForDeviceReachable(t, device, deviceReachableTimeout)
	assertCoreExpectsExtension(t, device)
	assertExtensionHealthy(t, device, extensionHealthTimeout)
	assertAppReachable(t, device, appUUID, "after the successful retry")
	assertDeviceStillReporting(t, device, deviceReportingTimeout)

	evetest.Checkpoint("retry-succeeded")
}
