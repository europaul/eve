// Copyright (c) 2026 Zededa, Inc.
// SPDX-License-Identifier: Apache-2.0

package hypervisor

import (
	"fmt"
	"strings"

	"github.com/lf-edge/eve/pkg/pillar/types"
)

// injectionChars are the characters that must never appear in a
// controller-supplied string that is rendered verbatim into a hypervisor
// config file. Both the QEMU config consumed via -readconfig and the Xen xl
// config are line-oriented: every value lives on its own line and a new
// section/directive begins on a fresh line. A newline or carriage return is
// therefore the decisive injection vector - it terminates the current
// `key = "value"` directive and lets a malicious value smuggle in whole new
// config lines (e.g. an extra vfio-pci host passthrough or a debug backend
// that exposes guest memory). A NUL byte is rejected as well because it
// truncates the config string once it reaches the C-level hypervisor tooling.
const injectionChars = "\n\r\x00"

// validateConfigField rejects a controller-supplied value that would let it
// break out of its config directive and inject additional lines. It does not
// reject quotes or commas: those are legitimate in some of the fields checked
// here (kernel command lines, file paths) and, being confined to a single
// line, cannot introduce a new directive on their own.
func validateConfigField(name, value string) error {
	if i := strings.IndexAny(value, injectionChars); i >= 0 {
		return fmt.Errorf("invalid character %q in %s: control characters "+
			"are not allowed (possible hypervisor config injection)",
			value[i], name)
	}
	return nil
}

// validateDomainConfig checks every controller-supplied string that gets
// rendered verbatim into the generated hypervisor (QEMU/Xen) config file so a
// value containing a newline cannot inject additional device/config directives.
// It is called at the top of each hypervisor's CreateDomConfig, before any
// template is executed, so the check is a single choke point shared by KVM and
// Xen. See EVE security fix for hypervisor config template injection.
//
// The intent is to cover the complete set of strings that reach the config
// file, not a hand-picked subset, so the check does not rot as the templates
// evolve. Fields that are not rendered into the config file are deliberately
// left out - most importantly the free-form cloud-init payload
// (CloudInitUserData, CipherBlockStatus), which is base64/binary, legitimately
// contains newlines and is written to a separate cloud-init image rather than
// this config file. Numeric and enum fields (Memory, VCpus, disk Format, ...)
// and net.HardwareAddr (Mac) cannot carry the injection characters once parsed,
// so they need no check. DisplayName is overwritten with the already-validated
// domainName before it is rendered, so validating domainName covers it.
func validateDomainConfig(domainName string, config types.DomainConfig,
	diskStatusList []types.DiskStatus, aa *types.AssignableAdapters) error {

	fields := []struct {
		name, value string
	}{
		{"domain name", domainName},
		{"kernel path", config.Kernel},
		{"ramdisk path", config.Ramdisk},
		{"device tree path", config.DeviceTree},
		{"boot loader path", config.BootLoader},
		{"root device", config.RootDev},
		{"extra boot args", config.ExtraArgs},
		{"VNC password", config.VncPasswd},
	}
	for _, f := range fields {
		if err := validateConfigField(f.name, f.value); err != nil {
			return err
		}
	}

	// Xen dtdev= and iomem= list entries.
	for _, dt := range config.DtDev {
		if err := validateConfigField("device tree device", dt); err != nil {
			return err
		}
	}
	for _, im := range config.IOMem {
		if err := validateConfigField("iomem range", im); err != nil {
			return err
		}
	}

	for i := range diskStatusList {
		ds := &diskStatusList[i]
		if err := validateConfigField("disk file location", ds.FileLocation); err != nil {
			return err
		}
		if err := validateConfigField("disk WWN", ds.WWN); err != nil {
			return err
		}
		if err := validateConfigField("disk vdev", ds.Vdev); err != nil {
			return err
		}
	}

	for i := range config.VifList {
		vif := &config.VifList[i]
		if err := validateConfigField("bridge name", vif.Bridge); err != nil {
			return err
		}
		if err := validateConfigField("vif name", vif.Vif); err != nil {
			return err
		}
	}

	// Passthrough (I/O adapter) attributes the controller supplies and that are
	// rendered verbatim into the KVM vfio-pci host address and the Xen
	// pci=/irqs=/ioports=/serial=/usb= lines. Only the bundles reserved for this
	// domain are checked, so a malformed bundle belonging to another app cannot
	// block this one.
	if aa != nil {
		for i := range aa.IoBundleList {
			ib := &aa.IoBundleList[i]
			if ib.UsedByUUID != config.UUIDandVersion.UUID {
				continue
			}
			ioFields := []struct {
				name, value string
			}{
				{"PCI address", ib.PciLong},
				{"IRQ", ib.Irq},
				{"I/O ports", ib.Ioports},
				{"serial device", ib.Serial},
				{"USB address", ib.UsbAddr},
			}
			for _, f := range ioFields {
				if err := validateConfigField(f.name, f.value); err != nil {
					return err
				}
			}
		}
	}

	return nil
}
