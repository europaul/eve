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

// targetEVERepoParamKey names the repository the split image is pulled from, for
// the common case where it is not published in the same repository as the
// releases the device was installed from.
const targetEVERepoParamKey = "TARGET_EVE_REPO"

// TestSplitUpgradeOverPlainHTTP performs an end-to-end base-OS update from a
// monolithic EVE to a split (universal) image whose OCI blobs are served by an
// ordinary static file server instead of a container registry.
//
// Objective:
//
//	Split images have to be delivered as a whole OCI image, because flattening
//	them to a single rootfs.img drops the Extension layer. That normally implies
//	a container registry, which not every deployment has. This test proves the
//	weaker requirement is enough: an OCI pull is all GETs, so the same image can
//	be laid out as static files and served by anything -- an S3 bucket, a plain
//	web server -- with no registry software and no per-response headers.
//
//	The device is told the datastore is a container registry, so EVE resolves and
//	pulls it with its normal OCI client; only the server behind it is dumb. What
//	makes this work, and what would break it:
//	  - The image server address must be RFC1918. EVE's OCI client selects https
//	    for any registry host that is not RFC1918, loopback or *.local, and the
//	    image server speaks plain HTTP.
//	  - GET /v2/ must answer 200 or 401; a directory listing satisfies it.
//	  - The manifest's Content-Type does not matter: the client digests the body
//	    itself. Docker-Content-Digest is only required for HEAD, which the pull
//	    path never issues.
//
// Network model:
//
//	SingleEthWithDHCP -- one management Ethernet port with DHCP, the simplest
//	model that lets the device reach both the controller and the image server,
//	and lets the deployed app get an IP for the SSH reachability check.
//
// Device configuration:
//
//	One DHCP network on eth0 (mgmt + apps), one local network instance, and one
//	Ubuntu container app with a 2222->22 port-forward and an allow-all ACL, so
//	the app can be reached over SSH to prove it is healthy.
//
// Phases:
//  1. Boot the device on the initial monolithic version and deploy the app;
//     verify the app is reachable over SSH (checkpoint "pre-upgrade").
//  2. Assert the running core is monolithic (no ext-verity-roothash marker).
//  3. Publish the split image as a static registry tree on the image server and
//     update the base OS to it, waiting until it boots active (checkpoint
//     "upgrade-complete").
//  4. Assert the running core is now split, the Extension is verity-mounted and
//     extsloader is Ready, and that the Extension was self-healed from the CAS.
//  5. Verify the app is still running and reachable, proving the device is not
//     in degraded mode (checkpoint "post-upgrade-verified").
//
// Parameters:
//   - EVE_VERSION: the split (universal) EVE version to update to. The image is
//     fetched from the "<EVE_VERSION>-<HYPERVISOR>-<arch>" tag; the version the
//     device then reports is read from the image itself.
//   - HYPERVISOR: target hypervisor (default: kvm).
//   - TPM: enable TPM emulation (default: true).
//   - DISK_SIZE_MB: device disk size in MiB (0 = framework default).
//   - INITIAL_EVE_VERSION: monolithic EVE version to start on (required; default
//     "16.0.1-lts").
//   - INITIAL_HYPERVISOR: hypervisor of the initial version (default: kvm).
func TestSplitUpgradeOverPlainHTTP(test *testing.T) {
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
				Summary: "Monolithic EVE version the device starts on before the split update",
				Default: "16.0.1-lts",
			},
		},
		evetest.TestParameterDefinition{
			Key:          initialHypervisorParamKey,
			DefaultValue: evetest.HypervisorKVM,
			Description: evetest.TestParameterDescription{
				Summary:       "Hypervisor used by the initial (pre-update) EVE version",
				Default:       "kvm",
				AllowedValues: "kvm|xen|kubevirt",
			},
		},
		evetest.TestParameterDefinition{
			Key:          targetEVERepoParamKey,
			DefaultValue: "",
			Description: evetest.TestParameterDescription{
				Summary: "Repository holding the split image, when it is not published " +
					"alongside the releases the device was installed from",
				Default: "same repository as the initial version (EVE_REPO)",
			},
		},
	)

	// Get parameter values set for this test execution.
	withTPM := evetest.GetTPMParameterValue()
	diskSizeMiB := evetest.GetDiskSizeMiBParameterValue()
	targetVersion := evetest.GetEVEVersionParameterValue()
	targetHypervisor := evetest.GetHypervisorParameterValue()
	initialVersion := evetest.GetTestParameter[string](initialEVEVersionParamKey)
	if initialVersion == "" {
		evetestT.Fatalf("%s%s is required for TestSplitUpgradeOverPlainHTTP",
			constants.EnvPrefix, initialEVEVersionParamKey)
	}
	initialHypervisor := evetest.GetTestParameter[evetest.Hypervisor](initialHypervisorParamKey)
	targetRepo := evetest.GetTestParameter[string](targetEVERepoParamKey)

	const devName = "edge-dev"
	evetest.Setup(
		evetest.RequireEdgeDevice{
			Name:              devName,
			WithEVEVersion:    initialVersion,
			WithHypervisor:    initialHypervisor,
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

	assertAppReachable(t, device, appUUID, "before the split update")

	// The device must currently be monolithic (no ext-verity-roothash marker).
	assertCoreIsMonolithic(t, device)

	evetest.Checkpoint("pre-upgrade")

	// Update the base OS to the split image, served as static files rather than
	// from a registry (expect success, no revert).
	var upgradeOpts []evetest.UpgradeOption
	if targetRepo != "" {
		upgradeOpts = append(upgradeOpts, evetest.WithUpgradeImageRepo(targetRepo))
	}
	updateBaseOS(t, device, targetVersion, targetHypervisor, false,
		evetest.UpgradeDeliveryOCIOverHTTP, upgradeOpts...)

	evetest.Checkpoint("upgrade-complete")

	// The running core must now be split, with a healthy, verity-backed Extension
	// that was self-healed from the CAS (monolithic baseosmgr cannot pre-extract
	// the Extension). Reaching this point also proves the whole OCI image -- not
	// just the Core -- came off the plain file server: the Extension layer is only
	// present in the CAS if every blob was pulled.
	assertCoreExpectsExtension(t, device)
	assertExtensionHealthy(t, device, extensionHealthTimeout)
	assertExtensionSelfHealed(t, device)

	// Verify the app comes back up and is still reachable under the split image.
	// A running workload also proves the device is not in degraded mode.
	assertAppReachable(t, device, appUUID, "after the split update")

	evetest.Checkpoint("post-upgrade-verified")
}
