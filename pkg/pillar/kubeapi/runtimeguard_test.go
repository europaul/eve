// Copyright (c) 2026 Zededa, Inc.
// SPDX-License-Identifier: Apache-2.0

//go:build k

package kubeapi

import (
	"os"
	"strings"
	"testing"
)

// TestMain satisfies the kube-runtime guard for the whole package. Every entry
// point here is kube-only and refuses to run off a kube image, a verdict the
// real check takes from a file only a device has. TestEnsureKubeRuntime drives
// the guard itself.
func TestMain(m *testing.M) {
	isHVTypeKube = func() bool { return true }
	os.Exit(m.Run())
}

func TestEnsureKubeRuntime(t *testing.T) {
	orig := isHVTypeKube
	defer func() { isHVTypeKube = orig }()

	isHVTypeKube = func() bool { return true }
	if err := ensureKubeRuntime("SomeOp"); err != nil {
		t.Errorf("ensureKubeRuntime on a kube runtime = %v, want nil", err)
	}

	isHVTypeKube = func() bool { return false }
	err := ensureKubeRuntime("SomeOp")
	if err == nil {
		t.Fatal("ensureKubeRuntime off a kube runtime = nil, want an error")
	}
	if !strings.Contains(err.Error(), "SomeOp") {
		t.Errorf("error %q does not name the operation", err)
	}
}
