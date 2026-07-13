// Copyright (c) 2026 Zededa, Inc.
// SPDX-License-Identifier: Apache-2.0

package splitrootfs_test

import (
	"strings"
	"testing"

	// revive:disable:dot-imports
	. "github.com/onsi/gomega"

	"github.com/lf-edge/eve-api/go/evecommon"
	"github.com/lf-edge/eve/evetest"
	"github.com/lf-edge/eve/evetest/netmodels"
)

// TestSplitFreshInstall installs a split (universal) EVE image from scratch via
// the installer, lets the framework onboard it to the embedded Adam controller,
// and verifies the device comes up correctly split: the running core expects an
// Extension, the Extension (disk-0) layer is verity-mounted, extsloader reports
// Ready, and the universal image resolved its hypervisor at runtime.
//
// Unlike TestSplitUpgradeFromMonolith this performs no OTA -- the split image is
// the device's initial EVE version. On the installer path the Extension is
// placed on /persist directly by the installer, so no CAS self-heal is expected.
//
// Network model:
//
//	SingleEthWithDHCP -- a single management Ethernet port with DHCP, enough for
//	the device to reach the controller and onboard.
//
// Device configuration:
//
//	One DHCP network on eth0 (management + apps). No workloads are deployed; the
//	test only validates that the split base OS boots healthy and stays
//	manageable.
//
// Phases:
//  1. Install + onboard the split image (framework Setup) and apply a minimal
//     management config, confirming the device fetches and applies it
//     (checkpoint "installed").
//  2. Assert the running core is split (ext-verity-roothash marker present), the
//     Extension is verity-mounted and extsloader is Ready, and the universal
//     image resolved its hypervisor at runtime (checkpoint "verified").
//
// Parameters:
//   - EVE_VERSION: the split (universal) EVE version to install (default: current
//     repo HEAD). The image must be available to the framework as
//     lfedge/eve:<version>-<hv>-amd64; a universal image built as -uni-amd64 can
//     be retagged to the matching -<hv>-amd64 (e.g. -kvm-amd64).
//   - HYPERVISOR: hypervisor to run as (default: kvm).
//   - TPM: enable TPM emulation (default: true).
//   - DISK_SIZE_MB: device disk size in MiB (0 = framework default).
func TestSplitFreshInstall(test *testing.T) {
	evetestT := evetest.Init(test)
	t := NewGomegaWithT(evetestT)
	defer evetest.Close()

	// Define configurable parameters available for the test.
	evetest.DefineTestParameters(
		evetest.EVEVersionParameter(),
		evetest.HypervisorParameter(),
		evetest.TPMParameter(),
		evetest.DiskSizeMiBParameter(),
	)

	// Get parameter values set for this test execution.
	withTPM := evetest.GetTPMParameterValue()
	hypervisor := evetest.GetHypervisorParameterValue()
	diskSizeMiB := evetest.GetDiskSizeMiBParameterValue()

	const devName = "edge-dev"
	evetest.Setup(
		evetest.RequireEdgeDevice{
			Name:              devName,
			WithHypervisor:    hypervisor,
			WithTPM:           withTPM,
			MinDiskSizeInMiB:  diskSizeMiB,
			DeviceReusePolicy: evetest.CreateFromScratchWithInstaller,
		},
		evetest.RequireNetworkModel{NetworkModel: netmodels.SingleEthWithDHCP},
	)
	device := evetest.GetEdgeDevice(devName)

	// Apply a minimal management config and confirm the device fetches and
	// applies it -- proving it onboarded and stays manageable on the split image.
	devConfig := evetest.NewEdgeDeviceConfig(devName)
	networkUUID := devConfig.AddNetwork(evetest.DHCPNetworkConfig{
		NetworkType: evecommon.NetworkType_V4,
	})
	devConfig.AddNetworkAdapter(evetest.NetworkAdapterConfig{
		LogicalLabel:  "eth0",
		PhysicalLabel: "eth0",
		InterfaceName: "eth0",
		NetworkUUID:   networkUUID,
		Usage:         evecommon.PhyIoMemberUsage_PhyIoUsageMgmtAndApps,
	})
	device.ApplyConfig(devConfig, true, true)

	evetest.Checkpoint("installed")

	// The running core must be a split image with a healthy, verity-backed
	// Extension reported Ready by extsloader.
	assertCoreExpectsExtension(t, device)
	assertExtensionHealthy(t, device, extensionHealthTimeout)

	// The universal image must have resolved its hypervisor at runtime (not left
	// as the "uni" sentinel).
	hvOut, _, err := device.RunShellScript(
		"cat /run/eve-hv-type 2>/dev/null || echo missing", shortSSHTimeout, 0)
	t.Expect(err).NotTo(HaveOccurred())
	runtimeHV := strings.TrimSpace(hvOut)
	t.Expect(runtimeHV).NotTo(Equal("uni"),
		"universal image did not resolve its hypervisor (still 'uni')")
	t.Expect(runtimeHV).NotTo(Equal("missing"), "/run/eve-hv-type not present")
	evetest.Logger().Infof("runtime hypervisor resolved to %q", runtimeHV)

	evetest.Checkpoint("verified")
}
