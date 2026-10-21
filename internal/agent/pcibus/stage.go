// Copyright 2026 NVIDIA CORPORATION
// SPDX-License-Identifier: Apache-2.0

package pcibus

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.uber.org/zap"

	"github.com/NVIDIA/k8s-test-infra/internal/cpulist"
	"github.com/NVIDIA/k8s-test-infra/internal/fsutil"

	"github.com/NVIDIA/k8s-test-infra/internal/agent"
	"github.com/NVIDIA/k8s-test-infra/internal/agent/cpulocality"
	"github.com/NVIDIA/k8s-test-infra/internal/agent/host"
	ibsysfs "github.com/NVIDIA/k8s-test-infra/internal/ib/sysfs"
	"github.com/NVIDIA/k8s-test-infra/internal/pcisysfs"
)

const (
	// kernelDMIRelPath is the kernel's DMI directory relative to /sys;
	// /sys/class/dmi/id is a symlink to it.
	kernelDMIRelPath = "devices/virtual/dmi/id"
	// mockDMIRelPath is the same directory inside the rendered tree.
	mockDMIRelPath = pcisysfs.SysDevicesRelPath + "/virtual/dmi/id"
)

// dmiMountTargets are the DMI attributes a container reads through
// /sys/class/dmi/id. product_uuid identifies the node and the kernel shows it
// to root alone, so it is staged empty — a target to mount over, not a value.
var dmiMountTargets = []struct {
	name     string
	byValue  bool
	fileMode os.FileMode
}{
	{name: "product_name", byValue: true, fileMode: 0o444},
	{name: "product_uuid", byValue: false, fileMode: 0o400},
}

// stageDMI reproduces the node's DMI attributes inside the rendered tree.
// Serving the tree at /sys/devices replaces the directory /sys/class/dmi/id
// resolves into, so on any cluster a served container would otherwise read
// ENOENT where the node has values.
//
// Under kind it is worse than a missing value: the node image bind-mounts its
// own product files into every container, mount(8) cannot create a target on a
// read-only sysfs, and every served pod dies on "mount point does not exist".
//
// Deliberately not a machine-type mock — that is writeMachineType's job (#681).
func stageDMI(h *host.Host) error {
	kernelDir := h.SysPath(kernelDMIRelPath)
	if _, err := os.Stat(kernelDir); err != nil {
		// No kernel DMI means no hook to satisfy either: both test the host.
		return nil
	}

	mockDir := h.RootPath(mockDMIRelPath)
	if err := os.MkdirAll(mockDir, 0o755); err != nil {
		return err
	}

	for _, attr := range dmiMountTargets {
		var value []byte
		if attr.byValue {
			// Unreadable is not fatal: a mount target need not carry a value.
			value, _ = os.ReadFile(filepath.Join(kernelDir, attr.name))
		}
		if err := fsutil.Write(filepath.Join(mockDir, attr.name), value, attr.fileMode); err != nil {
			return fmt.Errorf("stage dmi %s: %w", attr.name, err)
		}
	}

	return nil
}

// stageSysfs renders the PCI sysfs tree under h.Root, plus the DMI mount
// targets a container served that tree needs. A state with no PCI topology
// empties the tree and stages no DMI, since nothing is served.
func stageSysfs(h *host.Host, state *agent.State) error {
	topo := buildTopology(state, cpulocality.Online(h))
	ids := buildIdentities(state)
	addNICs(state, topo, ids)
	plan := planServeBack(h, topo)
	if err := pcisysfs.Render(pcisysfs.Options{
		Topology:    topo,
		Identities:  ids,
		OverlayRoot: h.Root,
		Preserve:    plan.preserves,
	}); err != nil {
		return err
	}

	if !state.HasPCITopology() {
		return clearServeBack(h)
	}
	if err := plan.stage(h); err != nil {
		return err
	}

	return stageDMI(h)
}

// shimGlob locates the shim in the container image. A package var so tests can
// exercise both branches without depending on what the host has installed.
var shimGlob = "/usr/local/lib/libmockfs.so*"

// stagePCIShim copies libmockfs.so* from /usr/local/lib into the driver
// lib directory so lspci inside a workload can be LD_PRELOAD-ed by the NRI
// plugin. Non-fatal when the shim is not built into the container image.
func stagePCIShim(h *host.Host) error {
	matches, _ := filepath.Glob(shimGlob)

	if len(matches) == 0 {
		zap.L().Debug("no libmockfs shim in image; skipping PCI shim staging")
		return nil
	}

	libDir := h.RootPath("driver/usr/local/lib")

	if err := os.MkdirAll(libDir, 0o755); err != nil {
		return err
	}

	for _, src := range matches {
		dst := filepath.Join(libDir, filepath.Base(src))
		if err := fsutil.Copy(src, dst, 0o755); err != nil {
			return fmt.Errorf("stage %s: %w", filepath.Base(src), err)
		}
	}

	return nil
}

// buildTopology maps the state's reconciled layout onto the renderer's type.
// Returns nil when there is nothing to render, which Render treats as a no-op.
// online is the host's online CPUs, or nil when unknown; see hostLocal.
func buildTopology(state *agent.State, online []int) *pcisysfs.PCIeTopology {
	rcs := state.PCITopology()
	if len(rcs) == 0 {
		return nil
	}

	declared := len(state.NodeShape.Topology.RootComplexes) > 0
	topo := &pcisysfs.PCIeTopology{RootComplexes: make([]pcisysfs.RootComplex, 0, len(rcs))}
	for _, rc := range rcs {
		var cpus []int
		if declared {
			cpus = cpulocality.Local(rc, state.NodeShape.Topology.CoresPerNUMA, online)
		}
		topo.RootComplexes = append(topo.RootComplexes, pcisysfs.RootComplex{
			ID:       rc.ID,
			NUMANode: rc.NUMANode,
			Devices:  rc.DeviceBDFs,
			CPUList:  cpulist.Format(cpus),
		})
	}

	return topo
}

// buildIdentities maps each PCI function's lowercased BDF to its identity for
// the renderer's attribute files (vendor, device, class, config space).
//
// This is the one place that knows which kind of hardware a BDF names, so it is
// where the class is decided: a GPU enumerates as a 3D controller, an NVSwitch
// as a bridge. Getting that wrong is not cosmetic — GPU Feature Discovery reads
// the class out of this tree to derive nvidia.com/gpu.mode.
func buildIdentities(state *agent.State) map[string]pcisysfs.PCI {
	ids := make(map[string]pcisysfs.PCI, len(state.Devices)+len(state.Switches))

	for _, d := range state.Devices {
		if d.PCIBusID == "" {
			continue
		}

		id := pcisysfs.PCI{
			BusID:       d.PCIBusID,
			DeviceID:    d.PCIDeviceID,
			SubsystemID: d.PCISubsystemID,
			Class:       pcisysfs.PCIClass3DController,
		}
		if g := d.VGPUGuest; g != nil {
			id.VGPUHostDriver = &pcisysfs.VGPUHostDriver{Version: g.HostDriverVersion, Branch: g.HostDriverBranch}
		}
		ids[strings.ToLower(d.PCIBusID)] = id
	}

	for _, sw := range state.Switches {
		if sw.PCIBusID == "" {
			continue
		}

		ids[strings.ToLower(sw.PCIBusID)] = pcisysfs.PCI{
			BusID:       sw.PCIBusID,
			DeviceID:    sw.PCIDeviceID,
			SubsystemID: sw.PCISubsystemID,
			Class:       pcisysfs.PCIClassBridge,
		}
	}

	return ids
}

// PoC (network use case D1): put the HCAs the InfiniBand tree renders on the
// PCI bus too, at the same addresses, so a consumer that scans the bus (NFD)
// finds them. Placement repeats ib.hostPCI's inputs exactly; each HCA joins the
// root complex of the GPU it pairs with.
func addNICs(state *agent.State, topo *pcisysfs.PCIeTopology, ids map[string]pcisysfs.PCI) {
	n := state.NodeShape.Network
	if topo == nil || !n.IBEnabled || n.HCACount <= 0 {
		return
	}

	numa := map[string]int{}
	rcOf := map[string]int{}
	var occupied []string
	for i, rc := range state.PCITopology() {
		for _, bdf := range rc.DeviceBDFs {
			numa[bdf] = rc.NUMANode
			rcOf[bdf] = i
			occupied = append(occupied, bdf)
		}
	}

	var gpus []ibsysfs.PCIFunction
	var gpuRC []int
	for _, d := range state.Devices {
		bdf := strings.ToLower(d.PCIBusID)
		if nn, ok := numa[bdf]; ok {
			gpus = append(gpus, ibsysfs.PCIFunction{Address: bdf, NUMANode: nn})
			gpuRC = append(gpuRC, rcOf[bdf])
		}
	}

	placements, err := ibsysfs.PlaceHCAs(n.HCACount, gpus, occupied)
	if err != nil {
		zap.L().Warn("PoC: cannot place HCAs on the PCI bus", zap.Error(err))
		return
	}

	// The identity the InfiniBand tree renders, from the profile's HCA type.
	nic := ibsysfs.NICIdentity(n.HCAType, n.LinkLayer)
	for i, pf := range placements {
		rc := 0
		if len(gpus) > 0 {
			rc = gpuRC[i*len(gpus)/n.HCACount]
		}
		topo.RootComplexes[rc].Devices = append(topo.RootComplexes[rc].Devices, pf.Address)
		ids[pf.Address] = pcisysfs.PCI{
			BusID:       pf.Address,
			DeviceID:    uint32(nic.Device)<<16 | 0x15b3,
			SubsystemID: 0x0008<<16 | 0x15b3,
			Class:       nic.Class,
		}
	}
}
