// Copyright (c) 2026 Zededa, Inc.
// SPDX-License-Identifier: Apache-2.0

package splitrootfs_test

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	// revive:disable:dot-imports
	. "github.com/onsi/gomega"

	uuid "github.com/satori/go.uuid"
	"google.golang.org/protobuf/proto"

	eveconfig "github.com/lf-edge/eve-api/go/config"
	"github.com/lf-edge/eve-api/go/evecommon"
	eveinfo "github.com/lf-edge/eve-api/go/info"
	"github.com/lf-edge/eve/evetest"
	api "github.com/lf-edge/eve/evetest/grpcapi/go"
	"github.com/lf-edge/eve/evetest/netmodels"
	"github.com/lf-edge/eve/pkg/pillar/types"
)

const (
	// baseOSUpdateTimeout bounds how long we wait for the device to fetch,
	// extract and boot a new EVE image. It must cover the download, the
	// partition write, the reboot and the whole nodeagent testing window
	// (shortened to updateTestWindow below).
	baseOSUpdateTimeout = 20 * time.Minute

	// updateTestWindow is the value we set for timer.test.baseimage.update, the
	// period nodeagent waits after booting the new partition before declaring
	// the update successful. The default is 10 minutes, which dominates the
	// runtime of these tests. It cannot be cut much further for updates that are
	// meant to SUCCEED: on the first boot of the split image the Extension has to
	// be self-healed out of the CAS, which itself waits for the vault to be
	// unlocked with a controller-escrowed key, and nodeagent rolls the update
	// back if extsloader is not Ready by the time the window expires.
	updateTestWindow = 5 * time.Minute

	// failedUpdateTestWindow is used by tests whose update is meant to FAIL. There
	// the window is pure waiting -- nodeagent rolls back once it expires -- so it
	// is cut to the shortest value that still lets the Core boot and report in.
	failedUpdateTestWindow = 2 * time.Minute

	// shortSSHTimeout bounds a single quick shell command executed on the device.
	shortSSHTimeout = 30 * time.Second

	// extsloaderStatusFile is where the socketdriver materializes extsloader's
	// ExtsloaderStatus publication ("global" is the only key it ever uses).
	extsloaderStatusFile = "/run/extsloader/ExtsloaderStatus/global.json"

	// extsloaderStateReady mirrors types.ExtsloaderStateReady
	// (0=starting, 1=ready, 2=failed).
	extsloaderStateReady uint8 = 1

	// Credentials and port-forward of the container app the OTA tests deploy to
	// prove the device keeps running workloads across a base-OS change.
	appSSHUser     = "root"
	appSSHPassword = "testpassword"
	appSSHFwdPort  = 2222

	// appSSHTimeout bounds a single ssh command executed inside the deployed app.
	appSSHTimeout = 20 * time.Second

	// extensionHealthTimeout bounds how long we wait for extsloader to report a
	// Ready Extension after the device boots the split image.
	extensionHealthTimeout = 5 * time.Minute

	// deviceReachableTimeout bounds how long we wait for the device to answer
	// over SSH again after a reboot.
	deviceReachableTimeout = 5 * time.Minute

	// deviceReportingTimeout bounds how long we wait for the device to publish a
	// fresh info message to the controller.
	deviceReportingTimeout = 3 * time.Minute

	// extCleanupTimeout bounds how long we allow baseosmgr to remove the
	// Extension image of a slot that is no longer in use.
	extCleanupTimeout = 3 * time.Minute

	// extMarkerFile is the version marker baked into the Extension of a second
	// split image by tests/eden/prepare-split-v2-image.sh, read back through the
	// mount. Stock images do not carry it.
	extMarkerFile = "/persist/exts/etc/eve-ext-release"

	// appReachableTimeout bounds how long we wait for the deployed app to answer
	// over SSH. It is generous because after a reboot sshd is back long before
	// pillar is: the device answers shell commands while domainmgr is still
	// waiting for adapters and containerd, and the app cannot be re-created until
	// that finishes. Eventually returns as soon as the app answers, so a high
	// ceiling costs nothing when recovery is quick.
	appReachableTimeout = 8 * time.Minute
)

// newOTATestDeviceConfig builds the device configuration shared by the
// split-rootfs base-OS update tests: a shortened update test window, one
// DHCP-managed Ethernet port used for both management and apps, a local network
// instance, and one container app reachable over SSH through a port forward.
//
// testWindow sets timer.test.baseimage.update; pass updateTestWindow for updates
// expected to succeed and failedUpdateTestWindow for ones expected to roll back.
//
// The app is what proves the device is not degraded after a base-OS change: it
// must survive the reboot and stay reachable, which exercises the hypervisor,
// the container runtime and zedrouter on the newly booted image.
//
// It returns the config (not yet applied) and the UUID of the deployed app.
func newOTATestDeviceConfig(devName string,
	testWindow time.Duration) (*evetest.EdgeDeviceConfig, uuid.UUID) {
	devConfig := evetest.NewEdgeDeviceConfig(devName)

	cfgProps := types.NewConfigItemValueMap()
	cfgProps.SetGlobalValueInt(types.MintimeUpdateSuccess, uint32(testWindow.Seconds()))
	devConfig.SetConfigProperties(cfgProps)

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
	return devConfig, appUUID
}

// assertAppReachable waits until the deployed app is running and answers over
// SSH with its own UUID (the hostname the test image reports). phase names the
// point in the test this is checked at, so a failure says which side of the
// base-OS change broke.
//
// The SSH probe, not WaitUntilAppIsRunning, is the real gate: the latter is
// satisfied by any RUNNING record the controller has already stored, so after a
// reboot it returns on the app's pre-reboot state and proves nothing about now.
func assertAppReachable(t Gomega, device *evetest.EdgeDevice, appUUID uuid.UUID,
	phase string) {
	log := evetest.Logger()
	log.Infof("Verifying app is running and reachable %s", phase)

	device.WaitUntilAppIsRunning(appUUID, appReachableTimeout)
	appAuth := evetest.UsernamePasswordAuth{
		Username: appSSHUser,
		Password: appSSHPassword,
	}
	t.Eventually(func(t Gomega) {
		out, _, err := device.RunShellScriptInsideApp(
			appUUID, appAuth, "hostname", appSSHTimeout, 0)
		t.Expect(err).NotTo(HaveOccurred())
		t.Expect(strings.TrimSpace(out)).To(Equal(appUUID.String()))
	}, appReachableTimeout, 5*time.Second).Should(Succeed())
}

// updateBaseOS drives an EVE base-OS update and waits for the target to reach
// its terminal state. It returns the EVE short version the device reports for
// the target image, and whether the device was ever seen running it.
//
// The wait is done here rather than by UpgradeEVE so that a stalled update dumps
// Extension diagnostics instead of just timing out.
// When expectRevert is true, the update is expected to be rejected and the
// device to roll back to the previous version.
func updateBaseOS(t Gomega, device *evetest.EdgeDevice,
	targetVersion string, targetHypervisor evetest.Hypervisor,
	expectRevert bool, delivery evetest.UpgradeDelivery) (string, bool) {
	log := evetest.Logger()
	log.Infof("Updating base OS to %s (%s)", targetVersion, targetHypervisor)

	shortVersion := device.UpgradeEVE(targetVersion, targetHypervisor,
		false, expectRevert, evetest.WithUpgradeDelivery(delivery))
	log.Infof("Target image reports EVE short version %q", shortVersion)

	booted := waitForBaseOSUpdate(t, device, shortVersion, expectRevert,
		baseOSUpdateTimeout)
	return shortVersion, booted
}

// upgradeToSplitImage drives an EVE base-OS update to a split (universal) image.
//
// The update goes through a registry datastore rather than the framework's
// default HTTP rootfs delivery: flattening the image to a single rootfs.img keeps
// only the Core, dropping the Extension (disk-0) layer, and never populates the
// containerd content-addressable store (CAS) that baseosmgr extracts the
// Extension from and extsloader self-heals it from.
func upgradeToSplitImage(t Gomega, device *evetest.EdgeDevice,
	targetVersion string, targetHypervisor evetest.Hypervisor,
	expectRevert bool) (string, bool) {
	return updateBaseOS(t, device, targetVersion, targetHypervisor, expectRevert,
		evetest.UpgradeDeliveryOCIRegistry)
}

// waitForBaseOSUpdate blocks until the device reaches the terminal state of a
// base-OS update, or fails the test on timeout.
//
// It mirrors the logic of the framework's private waitForUpgrade/waitForRevert,
// but uses the public WatchDeviceInfo channel. It scans each device-info
// message's SwList (info.GetSwList()) for the entry matching targetShortVersion:
//   - success path: returns once that entry reports PartitionState=="active";
//     fails immediately if it reports UserStatus==FAILED.
//   - revert path: returns once that entry reports UserStatus==FAILED (the
//     target was rejected and the device rolled back to the previous version).
//
// It returns whether the target was ever observed in PartitionState "inprogress",
// i.e. whether the device actually booted it. On the revert path that is the
// difference between "the image booted and was then rejected" and "the image
// never got far enough to run", which are very different failures.
func waitForBaseOSUpdate(t Gomega, device *evetest.EdgeDevice,
	targetShortVersion string, expectRevert bool, timeout time.Duration) bool {
	log := evetest.Logger()
	updates, stop := device.WatchDeviceInfo()
	defer stop()

	deadline := time.After(timeout)
	var lastState, lastStatus string
	var sawInProgress bool
	var attemptStarted bool
	for {
		select {
		case info, ok := <-updates:
			if !ok {
				t.Expect(ok).To(BeTrue(),
					"device-info watch closed unexpectedly while waiting for %s",
					targetShortVersion)
				return sawInProgress
			}
			for _, sw := range info.GetSwList() {
				if sw.GetShortVersion() != targetShortVersion {
					continue
				}
				status := sw.GetUserStatus()
				partState := sw.GetPartitionState()
				if partState == "inprogress" {
					sawInProgress = true
				}
				if expectRevert {
					if status == eveinfo.BaseOsStatus_FAILED {
						log.Infof("Device reverted from image %s: %s",
							targetShortVersion, sw.GetSubStatusStr())
						return sawInProgress
					}
				} else {
					// A FAILED status only belongs to this attempt once the
					// attempt has visibly started. On a retry the PREVIOUS
					// attempt's FAILED is still what the device reports when the
					// wait begins, and reading it as this attempt's verdict
					// aborts before the retry has done anything. Waiting for any
					// non-FAILED status first distinguishes the two, and still
					// fails fast on a first attempt -- which starts out
					// DOWNLOADING or UPDATING, never FAILED.
					if status != eveinfo.BaseOsStatus_FAILED {
						attemptStarted = true
					}
					if attemptStarted {
						t.Expect(status).NotTo(Equal(eveinfo.BaseOsStatus_FAILED),
							"base-OS update to %s failed: %s",
							targetShortVersion, sw.GetSubStatusStr())
					}
					if partState == "active" {
						log.Infof("Device booted image %s on the active partition",
							targetShortVersion)
						return sawInProgress
					}
				}
				if partState != lastState || status.String() != lastStatus {
					log.Infof("Base-OS update in progress (state=%s, status=%s)",
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
				"timed out after %s waiting for device to %s image %s "+
					"(last state=%s, status=%s)",
				timeout, verb, targetShortVersion, lastState, lastStatus)
			return sawInProgress
		}
	}
}

// waitUntilTargetBooted blocks until the device reports the target image on a
// partition in the "inprogress" state, i.e. it has booted the new image and is
// inside nodeagent's test window, before that window has decided anything.
//
// Tests that interfere with an update mid-flight need this: they have to act
// once the new image is running but before it is committed, which neither
// UpgradeEVE nor waitForBaseOSUpdate exposes -- both wait for a terminal state.
func waitUntilTargetBooted(t Gomega, device *evetest.EdgeDevice,
	targetShortVersion string, timeout time.Duration) {
	log := evetest.Logger()
	log.Infof("Waiting for the device to boot %s (inprogress)", targetShortVersion)
	updates, stop := device.WatchDeviceInfo()
	defer stop()

	deadline := time.After(timeout)
	for {
		select {
		case info, ok := <-updates:
			if !ok {
				t.Expect(ok).To(BeTrue(),
					"device-info watch closed while waiting for %s to boot",
					targetShortVersion)
				return
			}
			for _, sw := range info.GetSwList() {
				if sw.GetShortVersion() != targetShortVersion {
					continue
				}
				if sw.GetUserStatus() == eveinfo.BaseOsStatus_FAILED {
					t.Expect(false).To(BeTrue(),
						"%s failed before it ever booted: %s",
						targetShortVersion, sw.GetSubStatusStr())
					return
				}
				if sw.GetPartitionState() == "inprogress" {
					log.Infof("Device booted %s and is in its test window",
						targetShortVersion)
					return
				}
			}
		case <-deadline:
			logExtensionDiagnostics(device)
			t.Expect(false).To(BeTrue(),
				"timed out after %s waiting for the device to boot %s",
				timeout, targetShortVersion)
			return
		}
	}
}

// setControllerReachable cuts or restores the device's access to the controller
// by re-applying the network model with (or without) a firewall rule dropping
// everything addressed to it.
//
// Only controller-bound traffic is affected, so the harness can still reach the
// device over SSH and watch it decide to roll back -- which matters, because
// with the controller unreachable the device cannot report that decision.
//
// The model is a deep copy: netmodels holds shared package-level templates that
// must not be mutated.
func setControllerReachable(reachable bool) {
	model := proto.Clone(netmodels.SingleEthWithDHCP).(*api.NetworkModel)
	if !reachable {
		model.Firewall = &api.Firewall{
			Rules: []*api.FwRule{
				{
					DstSubnet: evetest.GetControllerIPv4().String() + "/32",
					Action:    api.FwAction_FW_DROP,
				},
			},
		}
	}
	evetest.UpdateNetworkModel(model)
	if reachable {
		evetest.Logger().Infof("Controller access restored")
	} else {
		evetest.Logger().Infof("Controller access cut (firewall drop to %s)",
			evetest.GetControllerIPv4())
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
	raw, err := device.ReadFile(extsloaderStatusFile)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(json.Unmarshal(raw, &status)).To(Succeed())
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

// waitForDeviceReachable blocks until the device answers a trivial shell command.
//
// A rejected base-OS update ends with a reboot back onto the previous partition,
// but the signal the test waits on -- the target image reporting FAILED -- is
// published while that reboot is still ahead. Anything probing the device over
// SSH straight afterwards races the reboot and fails with "no reachable
// endpoint", so the rollback paths have to wait for the device to come back
// first.
func waitForDeviceReachable(t Gomega, device *evetest.EdgeDevice,
	timeout time.Duration) {
	evetest.Logger().Infof("Waiting for the device to become reachable again")
	t.Eventually(func(g Gomega) {
		out, _, err := device.RunShellScript("echo alive", shortSSHTimeout, 0)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(strings.TrimSpace(out)).To(Equal("alive"))
	}, timeout, 10*time.Second).Should(Succeed())
}

// assertExtImageCount asserts how many Extension images /persist is left
// holding, once baseosmgr has had time to clean up.
//
// The expected count is scenario-specific and worth stating at each call site:
// after activating one split image over another, the unused slot's copy is
// dropped and exactly one remains; after rolling back to a monolithic image,
// which pairs with no Extension at all, none should.
func assertExtImageCount(t Gomega, device *evetest.EdgeDevice, want int,
	timeout time.Duration) {
	t.Eventually(func(g Gomega) {
		out, _, err := device.RunShellScript(
			"ls /persist/ext-img*.img 2>/dev/null | wc -l", shortSSHTimeout, 0)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(strings.TrimSpace(out)).To(Equal(strconv.Itoa(want)),
			"expected %d Extension image(s) on /persist", want)
	}, timeout, 10*time.Second).Should(Succeed())
}

// extMarker returns the version marker of the Extension currently mounted, read
// through the mount, or "" when the mounted Extension carries none.
//
// This is what makes the A/B pairing observable: the marker must name the
// version now running, so it changes as the active slot changes, and is absent
// when the active slot's Extension is a stock build.
func extMarker(t Gomega, device *evetest.EdgeDevice) string {
	out, _, err := device.RunShellScript(
		"cat "+extMarkerFile+" 2>/dev/null || true", shortSSHTimeout, 0)
	t.Expect(err).NotTo(HaveOccurred())
	return strings.TrimSpace(out)
}

// assertDeviceStillReporting waits for a fresh device-info message, proving the
// device is still talking to the controller.
//
// This is deliberately read-only. The obvious alternative -- pushing a config
// and waiting for it to be confirmed -- would also work, but after a rejected
// base-OS update the config the test holds no longer requests the failed image,
// so applying it would withdraw that image as a side effect and change the very
// situation under test. WatchDeviceInfo delivers only messages published after
// the watch starts, so receiving one proves current liveness rather than
// replaying history.
func assertDeviceStillReporting(t Gomega, device *evetest.EdgeDevice,
	timeout time.Duration) {
	evetest.Logger().Infof("Verifying the device still reports to the controller")
	updates, stop := device.WatchDeviceInfo()
	defer stop()

	select {
	case info, ok := <-updates:
		t.Expect(ok).To(BeTrue(), "device-info watch closed unexpectedly")
		t.Expect(info).NotTo(BeNil(), "device published an empty info message")
	case <-time.After(timeout):
		t.Expect(false).To(BeTrue(),
			"device published no info within %s, so it is no longer manageable",
			timeout)
	}
}

// coreImageKind reports whether the running core rootfs is a split image
// ("split") or a monolithic one ("monolithic"), told apart by the
// ext-verity-roothash marker that makes the core expect and mount a separate
// Extension.
func coreImageKind(t Gomega, device *evetest.EdgeDevice) string {
	out, _, err := device.RunShellScript(
		"test -f /hostfs/etc/ext-verity-roothash && echo split || echo monolithic",
		shortSSHTimeout, 0)
	t.Expect(err).NotTo(HaveOccurred())
	return strings.TrimSpace(out)
}

// assertCoreExpectsExtension asserts that the running core rootfs is a split
// image.
func assertCoreExpectsExtension(t Gomega, device *evetest.EdgeDevice) {
	t.Expect(coreImageKind(t, device)).To(Equal("split"),
		"running core does not expect an Extension (not a split image)")
}

// assertCoreIsMonolithic asserts that the running core rootfs is a monolithic
// image, i.e. it carries every service itself and expects no Extension.
func assertCoreIsMonolithic(t Gomega, device *evetest.EdgeDevice) {
	t.Expect(coreImageKind(t, device)).To(Equal("monolithic"),
		"running core expects an Extension (not a monolithic image)")
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
	provenance, detail := extensionProvenance(t, device)
	t.Expect(provenance).To(Equal("self-healed"),
		"Extension predates this boot, so it was not self-healed from the CAS (%s)",
		detail)
}

// assertExtensionPreExtracted asserts the opposite of assertExtensionSelfHealed:
// the Extension was written by baseosmgr on the previous boot, before the reboot
// into the new image, so no CAS self-heal was needed.
//
// This is what a split-to-split update must do. The previous EVE is itself a
// split image, so its baseosmgr can extract the incoming Extension for the
// target slot ahead of the reboot -- unlike a monolithic predecessor, which
// cannot, leaving extsloader to self-heal from the CAS on first boot.
func assertExtensionPreExtracted(t Gomega, device *evetest.EdgeDevice) {
	provenance, detail := extensionProvenance(t, device)
	t.Expect(provenance).To(Equal("pre-existing"),
		"Extension postdates this boot, so it was self-healed from the CAS "+
			"rather than pre-extracted by baseosmgr (%s)", detail)
}

// extensionProvenance reports how the Extension extsloader loaded came to be,
// as "self-healed" (created after this boot) or "pre-existing" (created before
// it), along with the raw boot/mtime detail for failure messages.
func extensionProvenance(t Gomega, device *evetest.EdgeDevice) (string, string) {
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

	detail := strings.TrimSpace(out)
	evetest.Logger().Infof("Extension %s provenance: %s", status.ImagePath, detail)
	if strings.Contains(out, "self-healed") {
		return "self-healed", detail
	}
	return "pre-existing", detail
}
