// Copyright (c) 2026 Zededa, Inc.
// SPDX-License-Identifier: Apache-2.0

package kubeapi

import (
	"fmt"

	"github.com/lf-edge/eve/pkg/pillar/base"
)

// isHVTypeKube is a var so tests can establish the kube runtime: the real
// check reads a file only a device running a kube image has.
var isHVTypeKube = base.IsHVTypeKube

func ensureKubeRuntime(op string) error {
	if isHVTypeKube() {
		return nil
	}
	return fmt.Errorf("%s: kube runtime is not enabled", op)
}
