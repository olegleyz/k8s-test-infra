// Copyright 2026 NVIDIA CORPORATION
// SPDX-License-Identifier: Apache-2.0

package inject

import (
	"os"
	"path/filepath"
	"strings"

	"go.uber.org/zap"

	"github.com/NVIDIA/k8s-test-infra/internal/pcisysfs"
)

// Network use case D1, follow-up PoC.
//
// A node-level consumer such as NFD, node_exporter or NVSentinel reads the
// node's /sys through a hostPath, mounted wherever its chart chooses
// (/host-sys, /host/sys). On real hardware nothing names these consumers, so
// neither does Mokka: any container with a hostPath of /sys gets the mock PCI
// tree layered over that mount, plus the host's own devices served back so it
// loses nothing it would read on a real node.

const (
	nodeSysfs = "/sys"
	// serveBackManifestRelPath mirrors pcibus.ServeBackManifestRelPath: the
	// list of host subtrees, relative to /sys, to bind back over the mock tree.
	serveBackManifestRelPath = "sys/serve-back"
)

// hostSysfsMounts returns where the container mounts the node's /sys, or nil.
// Mokka's own containers mount the overlay and are left alone: they read the
// node's real /sys on purpose.
func hostSysfsMounts(cfg Config, container Container) []string {
	var dests []string
	for _, m := range container.Mounts {
		if strings.HasPrefix(filepath.Clean(m.Source), filepath.Clean(cfg.HostOverlayPath)) {
			return nil
		}
		// A hostPath has the node path as its source; the container's own
		// sysfs mount has source "sysfs".
		if filepath.Clean(m.Source) == nodeSysfs {
			dests = append(dests, filepath.Clean(m.Destination))
		}
	}
	return dests
}

// Network use case D3, PoC. Some NIC consumers read the container's own /sys,
// not a hostPath: the RDMA shared device plugin runs privileged with host
// networking and reads /sys/bus/pci and /sys/class/infiniband directly. Serving
// every privileged container's /sys would also reach CNI plugins and Multus,
// which would then find interfaces in sysfs that netlink does not know, so the
// PoC names the consumer by its pod labels instead.
var ownSysfsConsumers = []map[string]string{
	{"app": "rdma-shared-dp"}, // Network Operator's k8s-rdma-shared-dev-plugin DaemonSet
}

// ownSysfsMount returns /sys when the container is a named own-/sys consumer
// and the runtime mounts sysfs there, or nil.
func ownSysfsMount(container Container) []string {
	named := false
	for _, labels := range ownSysfsConsumers {
		match := true
		for k, v := range labels {
			if container.PodLabels[k] != v {
				match = false
				break
			}
		}
		named = named || match
	}
	if !named {
		return nil
	}
	for _, m := range container.Mounts {
		if filepath.Clean(m.Destination) == nodeSysfs && (m.Type == "sysfs" || m.Source == "sysfs") {
			return []string{nodeSysfs}
		}
	}
	return nil
}

// sysfsTargets lists every /sys the container reads that Mokka serves: its
// hostPath mounts of the node's /sys and, for a named consumer, its own.
func sysfsTargets(cfg Config, container Container) []string {
	for _, m := range container.Mounts {
		if strings.HasPrefix(filepath.Clean(m.Source), filepath.Clean(cfg.HostOverlayPath)) {
			return nil
		}
	}
	return append(hostSysfsMounts(cfg, container), ownSysfsMount(container)...)
}

// serveHostSysfs layers the rendered PCI tree over each of the container's
// hostPath /sys mounts, then binds the host's own subtrees back onto the mount
// points the node agent staged. Fails open: without a rendered tree the
// container keeps the node's real /sys.
func serveHostSysfs(cfg Config, container Container, adjustment *Adjustment) {
	var mock []string
	for _, rel := range []string{pcisysfs.SysDevicesRelPath, pcisysfs.PCIDevicesRelPath} {
		src := filepath.Join(cfg.HostOverlayPath, rel)
		if info, err := os.Stat(src); err != nil || !info.IsDir() {
			zap.L().Debug("no PCI tree to serve at the container's host /sys",
				zap.String("namespace", container.Namespace), zap.String("container", container.Name))
			return
		}
		mock = append(mock, rel)
	}
	serveBack := readServeBack(cfg)
	// The class view (follow-up PoC 2): InfiniBand, RoCE and the host's own
	// class entries, linked into the served devices tree. Optional: without
	// it the consumer keeps the node's real /sys/class.
	classSrc := filepath.Join(cfg.HostOverlayPath, "sys/class")
	if info, err := os.Stat(classSrc); err == nil && info.IsDir() {
		mock = append(mock, "sys/class")
	}

	for _, dest := range sysfsTargets(cfg, container) {
		for _, rel := range mock {
			adjustment.Mounts = append(adjustment.Mounts, roBind(
				filepath.Join(cfg.HostOverlayPath, rel),
				filepath.Join(dest, strings.TrimPrefix(rel, "sys/"))))
		}
		for _, rel := range serveBack {
			adjustment.Mounts = append(adjustment.Mounts, roBind(
				filepath.Join(nodeSysfs, rel),
				filepath.Join(dest, rel)))
		}
		zap.L().Info("serving the mock PCI tree at the container's host /sys",
			zap.String("namespace", container.Namespace), zap.String("container", container.Name),
			zap.String("mount", dest), zap.Int("servedBack", len(serveBack)))
	}
}

func readServeBack(cfg Config) []string {
	data, err := os.ReadFile(filepath.Join(cfg.HostOverlayPath, serveBackManifestRelPath))
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		// Only plain relative paths under devices/: the manifest is the node
		// agent's, but a bind source outside /sys/devices is never intended.
		if strings.HasPrefix(line, "devices/") && !strings.Contains(line, "..") {
			out = append(out, line)
		}
	}
	return out
}

func roBind(source, destination string) Mount {
	return Mount{
		Source:      source,
		Destination: destination,
		Type:        "bind",
		Options:     []string{"rbind", "rprivate", "ro", "nosuid", "nodev"},
	}
}
