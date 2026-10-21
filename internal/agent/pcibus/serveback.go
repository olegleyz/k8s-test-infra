// Copyright 2026 NVIDIA CORPORATION
// SPDX-License-Identifier: Apache-2.0

package pcibus

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/NVIDIA/k8s-test-infra/internal/agent/host"
	"github.com/NVIDIA/k8s-test-infra/internal/fsutil"
	"github.com/NVIDIA/k8s-test-infra/internal/pcisysfs"
)

// ServeBackManifestRelPath lists, one per line, the paths relative to /sys
// whose host subtree a consumer of the mock tree gets back: the NRI plugin binds
// /sys/<line> over <consumer's sys>/<line>. Relative to the overlay root.
const ServeBackManifestRelPath = "sys/serve-back"

// mockVirtualChildren are the entries Mokka stages under devices/virtual; every
// other host child of devices/virtual is served back.
var mockVirtualChildren = map[string]bool{"dmi": true}

// serveBack is the plan for showing a consumer of the mock tree the host's own
// devices next to the simulated ones. The mock tree wins on any name clash.
//
// The host's /sys/devices is served back entry by entry, as bind mounts onto
// empty directories staged in the mock tree: a whole subtree where Mokka has no
// entry of that name (devices/system, devices/platform, a host root complex the
// profile does not declare), and child by child inside an entry both have (a
// root complex the profile reuses, devices/virtual). Binding rather than
// copying keeps the host's files live (CPU online state, block statistics) and
// costs nothing to keep fresh.
//
// The host's PCI functions also get lookup links in bus/pci/devices, so a
// consumer that walks that directory finds them beside the mock ones.
type serveBack struct {
	mounts []string          // relative to /sys
	links  map[string]string // BDF -> the host's own link target
}

func (p serveBack) preserves(rel string) bool {
	if slices.Contains(p.mounts, rel) {
		return true
	}
	if bdf, ok := strings.CutPrefix(rel, "bus/pci/devices/"); ok {
		_, linked := p.links[bdf]
		return linked
	}
	return false
}

// planServeBack compares the host's /sys with the tree about to be rendered.
// An unreadable host /sys yields an empty plan: the consumer then sees only
// the mock tree, as before serve-back existed.
func planServeBack(h *host.Host, topo *pcisysfs.PCIeTopology) serveBack {
	plan := serveBack{links: map[string]string{}}
	if topo == nil {
		return plan
	}

	mockRC := map[string]map[string]bool{}
	mockBDF := map[string]bool{}
	for _, rc := range topo.RootComplexes {
		devs := map[string]bool{}
		for _, bdf := range rc.Devices {
			devs[strings.ToLower(bdf)] = true
			mockBDF[strings.ToLower(bdf)] = true
		}
		mockRC[rc.ID] = devs
	}

	entries, _ := os.ReadDir(h.SysPath("devices"))
	for _, e := range entries {
		name := e.Name()
		switch {
		case mockRC[name] != nil:
			plan.mounts = append(plan.mounts, childMounts(h, name, mockRC[name])...)
		case name == "virtual":
			plan.mounts = append(plan.mounts, childMounts(h, name, mockVirtualChildren)...)
		case e.IsDir():
			plan.mounts = append(plan.mounts, "devices/"+name)
		}
	}

	links, _ := os.ReadDir(h.SysPath("bus/pci/devices"))
	for _, l := range links {
		bdf := l.Name()
		if mockBDF[bdf] {
			continue
		}
		if target, err := os.Readlink(h.SysPath("bus/pci/devices", bdf)); err == nil {
			plan.links[bdf] = target
		}
	}
	return plan
}

func childMounts(h *host.Host, parent string, mock map[string]bool) []string {
	var out []string
	children, _ := os.ReadDir(h.SysPath("devices", parent))
	for _, c := range children {
		if c.IsDir() && !mock[c.Name()] {
			out = append(out, "devices/"+parent+"/"+c.Name())
		}
	}
	return out
}

// stage creates the mount points and lookup links in the rendered tree and
// writes the manifest the NRI plugin reads. Run after the renderer, which
// leaves these entries alone because of preserves.
func (p serveBack) stage(h *host.Host) error {
	var errs []error
	for _, rel := range p.mounts {
		if err := os.MkdirAll(h.RootPath("sys", rel), 0o755); err != nil {
			errs = append(errs, fmt.Errorf("serve-back mount point %s: %w", rel, err))
		}
	}
	for bdf, target := range p.links {
		link := h.RootPath(pcisysfs.PCIDevicesRelPath, bdf)
		if current, err := os.Readlink(link); err == nil && current == target {
			continue
		}
		_ = os.Remove(link)
		if err := os.Symlink(target, link); err != nil {
			errs = append(errs, fmt.Errorf("serve-back link %s: %w", bdf, err))
		}
	}
	manifest := strings.Join(p.mounts, "\n")
	if manifest != "" {
		manifest += "\n"
	}
	if err := fsutil.Write(h.RootPath(ServeBackManifestRelPath), []byte(manifest), 0o644); err != nil {
		errs = append(errs, fmt.Errorf("serve-back manifest: %w", err))
	}
	return errors.Join(errs...)
}

// clearServeBack removes the manifest, so no consumer gets binds onto a tree
// that is gone.
func clearServeBack(h *host.Host) error {
	err := os.Remove(filepath.Join(h.Root, ServeBackManifestRelPath))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
