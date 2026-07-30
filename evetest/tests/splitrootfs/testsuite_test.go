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
// TODO: expand coverage with additional tests and variants as the split-rootfs
// feature matures:
//   - Split->split update (Extension already present, no self-heal expected).
//   - Broken/corrupted Extension causing a rejected update and rollback
//     (expectRevert=true).
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
