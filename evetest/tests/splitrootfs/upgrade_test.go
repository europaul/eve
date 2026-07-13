// Copyright (c) 2026 Zededa, Inc.
// SPDX-License-Identifier: Apache-2.0

package splitrootfs_test

import (
	"strings"
	"testing"
	"time"

	// revive:disable:dot-imports
	. "github.com/onsi/gomega"

	eveconfig "github.com/lf-edge/eve-api/go/config"
	"github.com/lf-edge/eve-api/go/evecommon"
	"github.com/lf-edge/eve/evetest"
	"github.com/lf-edge/eve/evetest/constants"
	"github.com/lf-edge/eve/evetest/netmodels"
	"github.com/lf-edge/eve/pkg/pillar/types"
)

const (
	initialEVEVersionParamKey = "INITIAL_EVE_VERSION"
	initialHypervisorParamKey = "INITIAL_HYPERVISOR"
	splitImageDomainParamKey  = "SPLIT_IMAGE_DOMAIN"
	splitImageRepoParamKey    = "SPLIT_IMAGE_REPO"
	splitImageTagParamKey     = "SPLIT_IMAGE_TAG"

	appSSHUser     = "root"
	appSSHPassword = "testpassword"
	appSSHFwdPort  = 2222

	// extensionHealthTimeout bounds how long we wait for extsloader to report a
	// Ready Extension after the device boots the split image.
	extensionHealthTimeout = 5 * time.Minute
)

// TestSplitUpgradeFromMonolith performs an end-to-end base-OS update from a
// monolithic (single-rootfs) EVE version to a split (universal) EVE image and
// verifies that the device correctly extracts, self-heals, mounts and runs the
// separate Extension (disk-0) layer while keeping workloads running.
//
// Objective:
//
//	A monolithic EVE cannot pre-extract the Extension layer, so on the first
//	boot of a split image the Extension must be reconstructed from the
//	containerd CAS (the "self-heal" path). This test proves that path works:
//	the device comes up split, the Extension is verity-mounted, extsloader
//	reports Ready, the self-heal happened, and a previously-deployed app keeps
//	running across the update (i.e. the device is not degraded).
//
// Network model:
//
//	SingleEthWithDHCP -- a single management Ethernet port with DHCP. This is
//	the simplest model that lets the device reach the controller and the
//	container registry (needed to pull the split OCI image via a registry
//	datastore) and lets the deployed app get an IP for SSH reachability checks.
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
//  3. Update the base OS to the split image and wait until it boots active
//     (checkpoint "upgrade-complete").
//  4. Assert the running core is now split, the Extension is verity-mounted and
//     extsloader is Ready, and that the Extension was self-healed from the CAS.
//  5. Verify the app is still running and reachable after the update, proving
//     the device is not in degraded mode (checkpoint "post-upgrade-verified").
//
// Parameters:
//   - EVE_VERSION: standard target-version parameter; not used as the update
//     target here (the target is the explicit split OCI image below), defined
//     only for framework/suite consistency.
//   - HYPERVISOR: target hypervisor (default: kvm).
//   - TPM: enable TPM emulation (default: true).
//   - DISK_SIZE_MB: device disk size in MiB (0 = framework default).
//   - INITIAL_EVE_VERSION: monolithic EVE version to start on (required; default
//     "16.0.0-lts").
//   - INITIAL_HYPERVISOR: hypervisor of the initial version (default: kvm).
//   - SPLIT_IMAGE_DOMAIN: registry domain of the split image
//     (default "index.docker.io").
//   - SPLIT_IMAGE_REPO: registry repo of the split image (default "lfedge/eve").
//   - SPLIT_IMAGE_TAG: split OCI tag to update to (required, e.g.
//     "0.0.0-abcdef-uni-amd64"). The EVE-reported short version equals this tag
//     for universal split images.
func TestSplitUpgradeFromMonolith(test *testing.T) {
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
				Summary: "Monolithic EVE version the device starts on before the split update",
				Default: "16.0.0-lts",
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
			Key:          splitImageDomainParamKey,
			DefaultValue: "index.docker.io",
			Description: evetest.TestParameterDescription{
				Summary: "Registry domain hosting the split (universal) EVE OCI image",
				Default: "index.docker.io",
			},
		},
		evetest.TestParameterDefinition{
			Key:          splitImageRepoParamKey,
			DefaultValue: "lfedge/eve",
			Description: evetest.TestParameterDescription{
				Summary: "Registry repository of the split (universal) EVE OCI image",
				Default: "lfedge/eve",
			},
		},
		evetest.TestParameterDefinition{
			Key:          splitImageTagParamKey,
			DefaultValue: "",
			Description: evetest.TestParameterDescription{
				Summary: "OCI tag of the split (universal) EVE image to update to " +
					"(e.g. \"0.0.0-abcdef-uni-amd64\"); the EVE-reported short version " +
					"equals this tag",
				Default: "(required)",
			},
		},
	)

	// Get parameter values set for this test execution.
	withTPM := evetest.GetTPMParameterValue()
	diskSizeMiB := evetest.GetDiskSizeMiBParameterValue()
	initialVersion := evetest.GetTestParameter[string](initialEVEVersionParamKey)
	if initialVersion == "" {
		evetestT.Fatalf("%s%s is required for TestSplitUpgradeFromMonolith",
			constants.EnvPrefix, initialEVEVersionParamKey)
	}
	initialHypervisor := evetest.GetTestParameter[evetest.Hypervisor](initialHypervisorParamKey)
	splitImageDomain := evetest.GetTestParameter[string](splitImageDomainParamKey)
	splitImageRepo := evetest.GetTestParameter[string](splitImageRepoParamKey)
	splitImageTag := evetest.GetTestParameter[string](splitImageTagParamKey)
	if splitImageTag == "" {
		evetestT.Fatalf("%s%s is required for TestSplitUpgradeFromMonolith",
			constants.EnvPrefix, splitImageTagParamKey)
	}
	// For universal split images, EVE reports the OCI tag as its short version.
	expectedShortVersion := splitImageTag

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
	niUUID := devConfig.AddNetworkInstance(evetest.LocalNetworkInstanceConfig{
		DisplayName: "local-ni",
		Port:        "eth0",
		Subnet:      evetest.IPSubnet("10.11.12.0/24"),
		DHCPRange: types.IPRange{
			Start: evetest.IPAddress("10.11.12.2"),
			End:   evetest.IPAddress("10.11.12.254"),
		},
		Gateway: evetest.IPAddress("10.11.12.1"),
		MTU:     1500,
	})
	appUUID := devConfig.AddApplication(evetest.ApplicationInstanceConfig{
		DisplayName: "splitrootfs-test-app",
		Activate:    true,
		Image: evetest.DockerContainer{
			ImageName: "milan4zededa/evetest-ubuntu-ctr",
			Tag:       "1.0",
		},
		VirtualizationMode: eveconfig.VmMode_HVM,
		CPUs:               1,
		MemoryBytes:        500 * evetest.MiB,
		NetworkAdapters: []evetest.AppNetworkAdapter{
			evetest.VirtualNetworkAdapter{
				LogicalLabel:        "vif0",
				NetworkInstanceUUID: niUUID,
				PortFwdRules: []evetest.PortFwdRule{
					{
						Protocol:     evetest.NetworkProtocolTCP,
						EdgeNodePort: appSSHFwdPort,
						AppPort:      22,
					},
				},
				ACLAllowRules: []evetest.ACLAllowRule{
					{
						Protocol:     evetest.NetworkProtocolAny,
						RemoteSubnet: evetest.IPSubnet("0.0.0.0/0"),
					},
				},
			},
		},
	})
	device.ApplyConfig(devConfig, false, false)

	device.WaitUntilAppIsRunning(appUUID, 5*time.Minute)

	// Verify the app is reachable before the update.
	appAuth := evetest.UsernamePasswordAuth{Username: appSSHUser, Password: appSSHPassword}
	sshTimeout := 20 * time.Second
	log := evetest.Logger()
	log.Infof("Verifying app is reachable before the split update")
	t.Eventually(func(t Gomega) {
		out, _, err := device.RunShellScriptInsideApp(
			appUUID, appAuth, "hostname", sshTimeout, 0)
		t.Expect(err).NotTo(HaveOccurred())
		t.Expect(strings.TrimSpace(out)).To(Equal(appUUID.String()))
	}, 3*time.Minute, 5*time.Second).Should(Succeed())

	// The device must currently be monolithic (no ext-verity-roothash marker).
	monoOut, _, err := device.RunShellScript(
		"test -f /hostfs/etc/ext-verity-roothash && echo split || echo monolithic",
		shortSSHTimeout, 0)
	t.Expect(err).NotTo(HaveOccurred())
	t.Expect(strings.TrimSpace(monoOut)).To(Equal("monolithic"),
		"device is expected to start on a monolithic EVE image")

	evetest.Checkpoint("pre-upgrade")

	// Update the base OS to the split image (expect success, no revert).
	upgradeToSplitImage(t, device, splitImageDomain, splitImageRepo, splitImageTag,
		expectedShortVersion, false)

	evetest.Checkpoint("upgrade-complete")

	// The running core must now be split, with a healthy, verity-backed
	// Extension that was self-healed from the CAS (monolithic baseosmgr cannot
	// pre-extract the Extension).
	assertCoreExpectsExtension(t, device)
	assertExtensionHealthy(t, device, extensionHealthTimeout)
	assertExtensionSelfHealed(t, device)

	// Verify the app comes back up and is still reachable under the split image.
	// A running workload also proves the device is not in degraded mode.
	device.WaitUntilAppIsRunning(appUUID, 5*time.Minute)
	log.Infof("Verifying app is reachable after the split update")
	t.Eventually(func(t Gomega) {
		out, _, err := device.RunShellScriptInsideApp(
			appUUID, appAuth, "hostname", sshTimeout, 0)
		t.Expect(err).NotTo(HaveOccurred())
		t.Expect(strings.TrimSpace(out)).To(Equal(appUUID.String()))
	}, 3*time.Minute, 5*time.Second).Should(Succeed())

	evetest.Checkpoint("post-upgrade-verified")
}
