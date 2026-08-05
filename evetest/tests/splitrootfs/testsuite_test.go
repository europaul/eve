// Copyright (c) 2026 Zededa, Inc.
// SPDX-License-Identifier: Apache-2.0

package splitrootfs_test

import (
	"testing"

	"github.com/lf-edge/eve/evetest"
)

// TestSplitRootfsSuite runs the split-rootfs integration tests.
//
// It currently runs:
//   - TestSplitFreshInstall: install a split image from scratch and verify it
//     boots healthy (Extension verity-mounted, extsloader Ready).
//   - TestSplitUpgradeFromMonolith: OTA from a monolithic image to a split image
//     (Extension CAS-self-healed). Requires SPLIT_IMAGE_TAG.
//   - TestSplitRevertToMonolith: the same OTA followed by a controller-initiated
//     revert back to the monolithic image.
//
// Four tests are deliberately NOT part of this suite, because each needs a
// purpose-built image that no registry publishes; requiring them here would make
// the whole suite unrunnable for anyone who has not built those artifacts by
// hand. Run them on their own:
//
//   - TestSplitBrokenExtensionRollback and
//     TestSplitBrokenExtensionRollbackFromSplit, with BROKEN_EVE_VERSION set to
//     an image from tests/eden/prepare-broken-split-image.sh. The first rolls
//     back to a monolith, the second to the previous split version.
//   - TestSplitUpdateSplitToSplit and TestSplitRevertToSplit, with
//     INITIAL_EVE_VERSION and EVE_VERSION set to two split versions, the second
//     from tests/eden/prepare-split-v2-image.sh (whose Extension carries the
//     version marker the A/B pairing assertions rely on).
//
// Once those images become build artifacts they belong in a "full" suite
// alongside the other scenarios that need extra images, mirroring how Eden split
// split-rootfs.tests.txt from split-rootfs-full.tests.txt.
//
// TODO: expand coverage with additional tests and variants as the split-rootfs
// feature matures:
//   - Controller disconnect during Extension download/extraction.
//   - PCR/measured-boot enforcement of the Extension roothash.
//   - Additional hypervisor/filesystem variants: kubevirt, xen, and ZFS.
func TestSplitRootfsSuite(test *testing.T) {
	evetest.Init(test)
	defer evetest.Close()

	// Define configurable parameters available for the test suite.
	evetest.DefineTestParameters(
		evetest.TPMParameter(),
	)

	evetest.RunTestSuite(
		evetest.TestCase{
			Test: TestSplitFreshInstall,
			Variants: []evetest.TestVariant{
				{
					Name: "FreshInstallKVM",
					Parameters: []evetest.TestParameterValue{
						{Key: evetest.HypervisorParameterKey, Value: evetest.HypervisorKVM},
					},
				},
			},
		},
		evetest.TestCase{
			Test: TestSplitUpgradeFromMonolith,
			Variants: []evetest.TestVariant{
				{
					Name: "MonolithToSplitKVM",
					Parameters: []evetest.TestParameterValue{
						{Key: initialHypervisorParamKey, Value: evetest.HypervisorKVM},
						{Key: evetest.HypervisorParameterKey, Value: evetest.HypervisorKVM},
					},
				},
			},
		},
		evetest.TestCase{
			Test: TestSplitRevertToMonolith,
			Variants: []evetest.TestVariant{
				{
					Name: "RevertToMonolithKVM",
					Parameters: []evetest.TestParameterValue{
						{Key: initialHypervisorParamKey, Value: evetest.HypervisorKVM},
						{Key: evetest.HypervisorParameterKey, Value: evetest.HypervisorKVM},
					},
				},
			},
		},
	)
}
