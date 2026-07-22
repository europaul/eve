// Copyright (c) 2026 Zededa, Inc.
// SPDX-License-Identifier: Apache-2.0

package hypervisor

import (
	"os"
	"strings"
	"testing"

	"github.com/lf-edge/eve/pkg/pillar/types"
	uuid "github.com/satori/go.uuid"
)

// TestCreateDomConfigInjectionExtraArgs demonstrates that a controller-supplied
// string (VmConfig.ExtraArgs) containing a newline is rendered verbatim into
// the QEMU config file and can therefore introduce additional QEMU config
// sections/directives that were never intended by EVE (EV-2606).
//
// The QEMU config file consumed via -readconfig is line-oriented: every value
// lives on its own `key = "value"` line and a new `[section]` header starts a
// new device/object. A newline embedded in a rendered field lets a malicious
// controller close the current directive and inject brand-new ones, e.g. an
// extra vfio-pci passthrough of a different host device or a debug backend that
// exposes guest memory.
func TestCreateDomConfigInjectionExtraArgs(t *testing.T) {
	id, err := uuid.NewV4()
	if err != nil {
		t.Fatalf("NewV4 failed: %v", err)
	}

	// Malicious payload: break out of the append="..." line and inject an
	// extra vfio-pci host device passthrough section.
	payload := "console=hvc0\"\n\n[device \"injected\"]\n  driver = \"vfio-pci\"\n  host = \"00:1f.0\"\n"

	config := types.DomainConfig{
		UUIDandVersion: types.UUIDandVersion{UUID: id, Version: "1.0"},
		VmConfig: types.VmConfig{
			Kernel:    "/boot/kernel",
			ExtraArgs: payload,
			Memory:    1024 * 1024 * 10,
			VCpus:     2,
		},
	}

	conf, err := os.CreateTemp("/tmp", "config-injection")
	if err != nil {
		t.Fatalf("Can't create config file: %v", err)
	}
	defer os.Remove(conf.Name())

	err = kvmIntel.CreateDomConfig(DefaultDomainName, config, types.DomainStatus{},
		nil, &types.AssignableAdapters{Initialized: true}, nil, "", conf)

	result, readErr := os.ReadFile(conf.Name())
	if readErr != nil {
		t.Fatalf("reading conf file failed: %v", readErr)
	}
	rendered := string(result)

	// If the field is validated/rejected, CreateDomConfig returns an error and
	// the injected section never appears. Before the fix, err is nil and the
	// injected device section is present in the rendered config.
	if err != nil {
		if strings.Contains(rendered, "[device \"injected\"]") {
			t.Fatalf("field rejected but injected section still rendered:\n%s", rendered)
		}
		t.Logf("injection correctly rejected: %v", err)
		return
	}

	if strings.Contains(rendered, "[device \"injected\"]") {
		t.Fatalf("INJECTION SUCCEEDED: attacker-controlled ExtraArgs injected an "+
			"extra QEMU device section into the config:\n%s", rendered)
	}
	t.Logf("no injected section found; rendered config:\n%s", rendered)
}
