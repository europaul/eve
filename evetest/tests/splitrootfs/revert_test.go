// Copyright (c) 2026 Zededa, Inc.
// SPDX-License-Identifier: Apache-2.0

package splitrootfs_test

import (
	"testing"

	// revive:disable:dot-imports
	. "github.com/onsi/gomega"

	"github.com/lf-edge/eve/evetest"
	"github.com/lf-edge/eve/evetest/constants"
	"github.com/lf-edge/eve/evetest/netmodels"
)

// revertToMonolithImage drives an EVE base-OS update back to a monolithic
// (single-rootfs) image. A monolithic image carries no Extension layer, so it is
// delivered the ordinary way: flattened to a rootfs.img over HTTP.
func revertToMonolithImage(t Gomega, device *evetest.EdgeDevice,
	targetVersion string, targetHypervisor evetest.Hypervisor) string {
	return updateBaseOS(t, device, targetVersion, targetHypervisor, false,
		evetest.UpgradeDeliveryHTTPRootfs)
}

// TestSplitRevertToMonolith updates a device from a monolithic (single-rootfs)
// EVE version to a split (universal) image and then reverts it back to the
// monolithic version, verifying the device comes back healthy and manageable on
// both legs.
//
// Objective:
//
//	Splitting the rootfs must not be a one-way door: an operator who updates a
//	fleet to a split image has to be able to put it back. This is the escape
//	hatch for the whole feature, so it is worth proving explicitly. The revert
//	is controller-initiated (the controller simply points the base OS at the
//	previous image) rather than the automatic rollback nodeagent performs when
//	an update fails its test window -- that path is covered separately by the
//	broken-Extension test.
//
//	Reverting is not merely the update run backwards. The monolithic image
//	carries no Extension, so on the way back the device must stop expecting one:
//	the ext-verity-roothash marker disappears, /persist/exts is no longer
//	mounted, and the Extension image files left on /persist become orphans. The
//	device must tolerate those leftovers -- an unsupported downgrade must not
//	depend on Extension cleanup having happened.
//
// Network model:
//
//	SingleEthWithDHCP -- a single management Ethernet port with DHCP. Enough for
//	the device to reach the controller and the container registry (the split
//	image is pulled via a registry datastore) and for the deployed app to get an
//	IP for SSH reachability checks.
//
// Device configuration:
//
//	One DHCP network on eth0 (mgmt + apps), one local network instance, and one
//	Ubuntu container app with a 2222->22 port-forward and an allow-all ACL, so
//	the app can be reached over SSH to prove the device still runs workloads.
//
// Phases:
//  1. Boot the device on the initial monolithic version, deploy the app and
//     verify it is reachable (checkpoint "pre-upgrade").
//  2. Update the base OS to the split image, wait until it boots active, and
//     assert the Extension is verity-mounted and extsloader is Ready
//     (checkpoint "split-active").
//  3. Revert the base OS to the initial monolithic version and wait until it
//     boots active (checkpoint "reverted").
//  4. Assert the running core is monolithic again and the app is still running
//     and reachable, i.e. the device is fully functional and manageable after
//     the round trip (checkpoint "revert-verified").
//
// Parameters:
//   - EVE_VERSION: the split (universal) EVE version to update to. The image is
//     fetched from the "<EVE_VERSION>-<HYPERVISOR>-<arch>" tag; the version the
//     device then reports is read from the image itself.
//   - HYPERVISOR: target hypervisor of the split image (default: kvm).
//   - TPM: enable TPM emulation (default: true).
//   - DISK_SIZE_MB: device disk size in MiB (0 = framework default).
//   - INITIAL_EVE_VERSION: monolithic EVE version to start on and revert to
//     (required; default "16.0.0-lts").
//   - INITIAL_HYPERVISOR: hypervisor of the monolithic version (default: kvm).
func TestSplitRevertToMonolith(test *testing.T) {
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
			DefaultValue: "16.0.0-lts",
			Description: evetest.TestParameterDescription{
				Summary: "Monolithic EVE version the device starts on and reverts back to",
				Default: "16.0.0-lts",
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
		evetestT.Fatalf("%s%s is required for TestSplitRevertToMonolith",
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

	// Apply initial device config: management adapter, a local network instance,
	// and a container app reachable over SSH.
	devConfig, appUUID := newOTATestDeviceConfig(devName)
	device.ApplyConfig(devConfig, false, false)

	assertAppReachable(t, device, appUUID, "before the split update")
	assertCoreIsMonolithic(t, device)

	evetest.Checkpoint("pre-upgrade")

	// Leg 1: monolith -> split. Covered in depth by TestSplitUpgradeFromMonolith;
	// here it only has to get the device onto a healthy split image so there is
	// something to revert from.
	upgradeToSplitImage(t, device, splitVersion, splitHypervisor, false)
	assertCoreExpectsExtension(t, device)
	assertExtensionHealthy(t, device, extensionHealthTimeout)

	evetest.Checkpoint("split-active")

	// Leg 2: split -> monolith. The controller points the base OS back at the
	// monolithic image; nodeagent must accept it and boot it active.
	revertToMonolithImage(t, device, monolithVersion, monolithHypervisor)

	evetest.Checkpoint("reverted")

	// The running core must expect no Extension again.
	assertCoreIsMonolithic(t, device)

	// The Extension images the split version left on /persist are orphans now.
	// They are harmless -- a later split update rewrites the file paired with the
	// slot it installs into -- so this is logged for diagnostics, not asserted.
	if out, _, err := device.RunShellScript(
		"ls /persist/ext-img*.img 2>/dev/null | wc -l", shortSSHTimeout, 0); err == nil {
		evetest.Logger().Infof("Orphaned Extension images left on /persist: %s", out)
	}

	// The device must still run workloads and take new configuration, i.e. the
	// round trip cost it neither its apps nor its manageability.
	assertAppReachable(t, device, appUUID, "after the revert to monolithic")
	device.ApplyConfig(devConfig, true, true)

	evetest.Checkpoint("revert-verified")
}
