// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
)

// PeerTopology is the relationship nvidia-smi's `topo -m` prints for one pair
// of PCI devices: a PCIe level and, between two GPUs, their NVLink count.
type PeerTopology struct {
	Level   nvml.GpuTopologyLevel
	NVLinks int
}

// pciLocality is where a PCI function hangs: its root complex and NUMA node.
type pciLocality struct {
	root string // "pci0000:00"; empty when unknown
	numa int    // -1 when unknown
}

// pairLevel is the one rule every pairwise PCIe level comes from, GPU or not.
// The rendered tree hangs every function straight off its root complex, the
// PCIe switch a DGX/HGX board puts a GPU and its NIC under included, so a
// shared root is the single-bridge case.
func pairLevel(a, b pciLocality) nvml.GpuTopologyLevel {
	switch {
	case a.numa != b.numa:
		return nvml.TOPOLOGY_SYSTEM
	case a.root != "" && a.root == b.root:
		return nvml.TOPOLOGY_SINGLE
	default:
		return nvml.TOPOLOGY_HOSTBRIDGE
	}
}

// PeerTopology answers for two PCI bus IDs, in either the NVML form
// (00000000:0A:00.0) or the kernel's (0000:0a:00.0). A GPU's locality comes
// from the profile's pcie_topology; anything else -- a NIC -- from the PCI
// tree the process sees, which is where nvidia-smi found it in the first
// place. ok is false when either device cannot be placed.
func (e *Engine) PeerTopology(busA, busB string) (PeerTopology, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	if e.initCount == 0 || e.server == nil {
		return PeerTopology{}, false
	}

	devA, devB := e.server.deviceByBusID(busA), e.server.deviceByBusID(busB)
	if devA != nil && devB != nil && devA.fabric != nil && devA.fabric.HasPCIeTopology() {
		return PeerTopology{
			Level:   devA.fabric.TopoLevel(devA.index, devB.index),
			NVLinks: devA.fabric.NVLinkCount(devA.index, devB.index),
		}, true
	}

	locA, okA := e.server.locality(devA, busA)
	locB, okB := e.server.locality(devB, busB)
	if !okA || !okB {
		debugLog("[NVML] peer topology %s <-> %s: unplaced (a=%v b=%v)\n", busA, busB, okA, okB)
		return PeerTopology{}, false
	}
	return PeerTopology{Level: pairLevel(locA, locB)}, true
}

// deviceByBusID returns the GPU at busID, or nil when no GPU has it.
func (s *MockServer) deviceByBusID(busID string) *ConfigurableDevice {
	want := canonicalPCIBusID(busID)
	for _, dev := range s.configurableDevices {
		if dev != nil && canonicalPCIBusID(dev.PciBusID) == want {
			return dev
		}
	}
	return nil
}

// locality places a device: a GPU from the declared topology, everything else
// (and a GPU the profile places nowhere) from the PCI tree.
func (s *MockServer) locality(dev *ConfigurableDevice, busID string) (pciLocality, bool) {
	if dev != nil && dev.fabric != nil && dev.fabric.HasPCIeTopology() {
		root := dev.fabric.rootOf[dev.index]
		if root != "" {
			return pciLocality{root: root, numa: dev.fabric.numaOf[dev.index]}, true
		}
	}
	return sysfsLocality(busID)
}

// pciTreeRoots are the PCI trees a device is looked up in, in order: the
// process's own /sys, which a hostPath consumer sees Mokka's tree layered over
// and which holds any genuine device, then the overlay's copy of Mokka's tree.
func pciTreeRoots() []string {
	roots := []string{"/sys"}
	if r := os.Getenv("MOCK_PCI_ROOT"); r != "" {
		roots = append(roots, filepath.Join(r, "sys"))
	}
	return roots
}

// sysfsLocality reads a function's root complex from its bus/pci/devices link
// (../../../devices/pciDDDD:BB/...) and its NUMA node from numa_node.
func sysfsLocality(busID string) (pciLocality, bool) {
	bdf, err := kernelBDF(busID)
	if err != nil {
		return pciLocality{}, false
	}
	for _, root := range pciTreeRoots() {
		link := filepath.Join(root, "bus/pci/devices", bdf)
		target, err := os.Readlink(link)
		if err != nil {
			continue
		}
		loc := pciLocality{numa: -1}
		for _, part := range strings.Split(filepath.ToSlash(target), "/") {
			if strings.HasPrefix(part, "pci") {
				loc.root = part
				break
			}
		}
		if data, err := os.ReadFile(filepath.Join(link, "numa_node")); err == nil {
			if n, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				loc.numa = n
			}
		}
		return loc, true
	}
	return pciLocality{}, false
}

// kernelBDF turns either bus ID form into the kernel's DDDD:BB:DD.F.
func kernelBDF(busID string) (string, error) {
	domain, bus, device, function, err := ParsePCIBusID(busID)
	if err != nil {
		return "", err
	}
	if domain > 0xffff {
		return "", fmt.Errorf("PCI domain %x out of range", domain)
	}
	return fmt.Sprintf("%04x:%02x:%02x.%x", domain, bus, device, function), nil
}
