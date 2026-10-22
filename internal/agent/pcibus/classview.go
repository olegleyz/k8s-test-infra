// Copyright 2026 NVIDIA CORPORATION
// SPDX-License-Identifier: Apache-2.0

package pcibus

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/NVIDIA/k8s-test-infra/internal/agent/host"
	"github.com/NVIDIA/k8s-test-infra/internal/fsutil"
	"github.com/NVIDIA/k8s-test-infra/internal/pcisysfs"
)

// Network use cases D1 and OBSERVE #7, follow-up PoC 2.
//
// The kernel publishes a NIC's class devices under its PCI function and links
// them from /sys/class:
//
//	/sys/devices/pciDDDD:BB/<bdf>/infiniband/mlx5_0/     (device -> ../../../<bdf>)
//	/sys/devices/pciDDDD:BB/<bdf>/infiniband_mad/umad0/
//	/sys/devices/pciDDDD:BB/<bdf>/infiniband_verbs/uverbs0/
//	/sys/devices/pciDDDD:BB/<bdf>/net/enp12s0np0/         (RoCE)
//	/sys/class/infiniband/mlx5_0 -> ../../devices/pciDDDD:BB/<bdf>/infiniband/mlx5_0
//
// The InfiniBand simulator renders the same content in its own flat tree for
// the LD_PRELOAD shims. classView rebuilds it in kernel shape inside the PCI
// tree, so a consumer that reads a hostPath /sys finds class entries whose
// device links land on the PCI function with the shared identity. The host's
// own class entries are mirrored as links beside the mock ones; they resolve
// through the served-back devices tree. Mock entries win on a name clash.

// mockClasses are the class directories whose entries the class view writes.
var mockClasses = []string{"infiniband", "infiniband_mad", "infiniband_verbs", "net"}

type classView struct {
	// build runs on every reconcile and, through Sync, after every run-time
	// NIC change; one at a time.
	buildMu sync.Mutex

	mu    sync.Mutex
	owned map[string]bool // "net/enp12s0np0", "infiniband/mlx5_0", ...
	once  sync.Once
}

// classDirs are the class directories the kernel creates under a NIC's PCI
// function, and which the view therefore owns there.
var classDirs = []string{"infiniband", "infiniband_mad", "infiniband_verbs", "net"}

// removedDir holds the PCI functions of NICs that a fault took off the bus. It
// sits beside the served directories, never inside them, so a function moved
// there is gone for every consumer in one rename and comes back in one.
const removedDir = ".removed-pci"

// build brings the mock class entries in line with the InfiniBand tree, then
// mirrors the host's. sys/class itself is never replaced: it is a bind source
// in running containers, which would keep the old inode.
//
// Consumers poll these files every second, so nothing here may be observable
// half-done: a changed file is swapped in by rename, an unchanged one is left
// alone, a departing class device loses its /sys/class link before its
// directory goes, and a NIC that left the bus takes its PCI function with it in
// one rename.
//
//nolint:cyclop,gocognit // PoC
func (v *classView) build(h *host.Host) error {
	v.buildMu.Lock()
	defer v.buildMu.Unlock()

	classRoot := h.RootPath("sys", "class")
	if err := os.MkdirAll(classRoot, 0o755); err != nil {
		return err
	}

	owned := map[string]bool{}
	published := map[string]bool{} // "devices/<rc>/<bdf>/infiniband/mlx5_0", ...
	present := map[string]bool{}   // BDFs of the HCAs the InfiniBand tree holds
	var errs []error
	ibClass := h.RootPath("ib", "sys", "class")

	hcas, _ := os.ReadDir(filepath.Join(ibClass, "infiniband"))
	for _, hca := range hcas {
		name := hca.Name()
		src := filepath.Join(ibClass, "infiniband", name)
		bdf := slotName(filepath.Join(src, "device", "uevent"))
		if bdf != "" {
			present[bdf] = true
			errs = append(errs, restoreFunction(h, bdf))
		}
		pciRel := pciFunctionRel(h, bdf)
		if pciRel == "" {
			zap.L().Debug("class view: HCA has no PCI function in the tree", zap.String("hca", name), zap.String("bdf", bdf))
			continue
		}
		pciDir := h.RootPath("sys", pciRel)

		// The PCI function carries the HCA's identity and NUMA node, so a
		// reader that follows device/ gets what the InfiniBand tree says.
		for _, f := range []string{"uevent", "modalias", "numa_node"} {
			if data, err := os.ReadFile(filepath.Join(src, "device", f)); err == nil {
				errs = append(errs, writeIfChanged(filepath.Join(pciDir, f), data))
			}
		}
		if target, err := os.Readlink(filepath.Join(src, "device", "physfn")); err == nil {
			errs = append(errs, replaceSymlink(target, filepath.Join(pciDir, "physfn")))
		}

		errs = append(errs, v.publish(h, owned, published, "infiniband", name, src, pciRel, "infiniband", "../../../"+bdf, "device"))

		for _, sub := range []string{"infiniband_mad", "infiniband_verbs"} {
			entries, _ := os.ReadDir(filepath.Join(ibClass, sub))
			for _, e := range entries {
				esrc := filepath.Join(ibClass, sub, e.Name())
				if !e.IsDir() || readTrim(filepath.Join(esrc, "ibdev")) != name {
					continue
				}
				errs = append(errs, v.publish(h, owned, published, sub, e.Name(), esrc, pciRel, sub, "../../../"+bdf, "device"))
			}
		}

		netdevs, _ := os.ReadDir(filepath.Join(src, "device", "net"))
		for _, n := range netdevs {
			nsrc := filepath.Join(ibClass, "net", n.Name())
			if _, err := os.Stat(nsrc); err != nil {
				continue
			}
			errs = append(errs, v.publish(h, owned, published, "net", n.Name(), nsrc, pciRel, "net", "../../../"+bdf, "device"))
		}
	}

	for _, c := range []string{"infiniband_mad", "infiniband_verbs"} {
		if data, err := os.ReadFile(filepath.Join(ibClass, c, "abi_version")); err == nil {
			_ = os.MkdirAll(filepath.Join(classRoot, c), 0o755)
			errs = append(errs, writeIfChanged(filepath.Join(classRoot, c, "abi_version"), data))
			owned[c+"/abi_version"] = true
		}
	}

	// Retract what this pass did not publish: the /sys/class links first, so
	// no reader follows one into a directory that is going, then the class
	// devices, then the PCI functions of NICs no longer on the bus.
	errs = append(errs, dropStaleClassLinks(h, owned))
	errs = append(errs, dropStaleClassDevices(h, published))
	errs = append(errs, removeAbsentNICs(h, present))

	v.mu.Lock()
	v.owned = owned
	v.mu.Unlock()

	errs = append(errs, v.mirrorHost(h))
	return errors.Join(errs...)
}

// publish brings one class device in the PCI function's directory in line
// with src, points its link attribute at the function, and links it from
// /sys/class/<class>.
func (v *classView) publish(h *host.Host, owned, published map[string]bool, class, name, src, pciRel, sub, linkTarget, linkName string) error {
	dstRel := filepath.Join(pciRel, sub, name)
	dst := h.RootPath("sys", dstRel)
	if err := syncTree(src, dst, linkName); err != nil {
		return fmt.Errorf("class view %s/%s: %w", class, name, err)
	}
	if err := replaceSymlink(linkTarget, filepath.Join(dst, linkName)); err != nil {
		return err
	}
	published[dstRel] = true
	owned[class+"/"+name] = true
	return replaceSymlink(filepath.Join("..", "..", dstRel), h.RootPath("sys", "class", class, name))
}

// dropStaleClassLinks removes the /sys/class links into the PCI tree that this
// pass did not publish. Links to the host's entries are mirrorHost's.
func dropStaleClassLinks(h *host.Host, owned map[string]bool) error {
	var errs []error
	for _, class := range mockClasses {
		entries, _ := os.ReadDir(h.RootPath("sys", "class", class))
		for _, e := range entries {
			rel := class + "/" + e.Name()
			link := h.RootPath("sys", "class", class, e.Name())
			target, err := os.Readlink(link)
			if err != nil || owned[rel] || !isMockClassLink(h, target) {
				continue
			}
			if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// isMockClassLink reports whether a /sys/class link lands in a class device
// the view wrote, as opposed to one the host serves back.
func isMockClassLink(h *host.Host, target string) bool {
	rel := strings.TrimPrefix(filepath.Clean(filepath.Join("class/x", target)), "/")
	parts := strings.Split(rel, string(filepath.Separator))
	// devices/<rc>/<bdf>/<class>/<name>
	if len(parts) != 5 || parts[0] != "devices" {
		return false
	}
	_, err := os.Stat(h.RootPath("sys", parts[0], parts[1], parts[2], "vendor"))
	return err == nil
}

// dropStaleClassDevices removes, each in one rename, the class devices under
// Mokka's NIC functions that this pass did not publish.
func dropStaleClassDevices(h *host.Host, published map[string]bool) error {
	var errs []error
	for _, fn := range mockNICFunctions(h) {
		for _, sub := range classDirs {
			entries, _ := os.ReadDir(h.RootPath("sys", fn, sub))
			for _, e := range entries {
				rel := filepath.Join(fn, sub, e.Name())
				if published[rel] {
					continue
				}
				errs = append(errs, retire(h, rel))
			}
		}
	}
	return errors.Join(errs...)
}

// mockNICFunctions lists "devices/<rc>/<bdf>" for every NIC function the PCI
// tree renders. Served-back host functions are empty mount points on disk and
// carry no vendor file, so they never match.
func mockNICFunctions(h *host.Host) []string {
	var out []string
	rcs, _ := os.ReadDir(h.RootPath("sys", "devices"))
	for _, rc := range rcs {
		if !strings.HasPrefix(rc.Name(), "pci") {
			continue
		}
		fns, _ := os.ReadDir(h.RootPath("sys", "devices", rc.Name()))
		for _, fn := range fns {
			rel := filepath.Join("devices", rc.Name(), fn.Name())
			if readTrim(h.RootPath("sys", rel, "vendor")) == "0x15b3" {
				out = append(out, rel)
			}
		}
	}
	return out
}

// removeAbsentNICs moves the PCI function of every NIC the InfiniBand tree no
// longer holds out of the tree, and drops its bus/pci/devices link: the NIC
// fell off the bus.
func removeAbsentNICs(h *host.Host, present map[string]bool) error {
	var errs []error
	for _, fn := range mockNICFunctions(h) {
		bdf := filepath.Base(fn)
		if present[bdf] {
			continue
		}
		link := h.RootPath(pcisysfs.PCIDevicesRelPath, bdf)
		if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
		dst := h.RootPath("sys", removedDir, strings.ReplaceAll(fn, string(filepath.Separator), "__"))
		_ = os.RemoveAll(dst)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := os.Rename(h.RootPath("sys", fn), dst); err != nil {
			errs = append(errs, fmt.Errorf("remove NIC function %s: %w", fn, err))
			continue
		}
		zap.L().Info("NIC removed from the PCI bus", zap.String("bdf", bdf))
	}
	return errors.Join(errs...)
}

// restoreFunction puts back a NIC function removeAbsentNICs took off the bus.
func restoreFunction(h *host.Host, bdf string) error {
	stash, _ := os.ReadDir(h.RootPath("sys", removedDir))
	for _, e := range stash {
		fn := strings.ReplaceAll(e.Name(), "__", string(filepath.Separator))
		if filepath.Base(fn) != bdf {
			continue
		}
		dst := h.RootPath("sys", fn)
		if _, err := os.Stat(dst); err == nil {
			// The PCI tree rendered the function afresh; the copy is stale.
			return os.RemoveAll(h.RootPath("sys", removedDir, e.Name()))
		}
		if err := os.Rename(h.RootPath("sys", removedDir, e.Name()), dst); err != nil {
			return fmt.Errorf("restore NIC function %s: %w", fn, err)
		}
		rel, err := filepath.Rel(filepath.Dir(h.RootPath(pcisysfs.PCIDevicesRelPath, bdf)), dst)
		if err != nil {
			return err
		}
		zap.L().Info("NIC back on the PCI bus", zap.String("bdf", bdf))
		return replaceSymlink(rel, h.RootPath(pcisysfs.PCIDevicesRelPath, bdf))
	}
	return nil
}

// retire removes sys/<rel> in one rename, then frees it.
func retire(h *host.Host, rel string) error {
	dst := h.RootPath("sys", removedDir, ".retired-"+strings.ReplaceAll(rel, string(filepath.Separator), "__"))
	_ = os.RemoveAll(dst)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.Rename(h.RootPath("sys", rel), dst); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.RemoveAll(dst)
}

// replaceSymlink points link at target in one rename, leaving a link that
// already does untouched.
func replaceSymlink(target, link string) error {
	if cur, err := os.Readlink(link); err == nil && cur == target {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		return err
	}
	tmp := link + ".tmp-link"
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, link); err != nil {
		_ = os.Remove(tmp)
		// A directory where the link belongs (an older layout): replace it.
		if rmErr := os.RemoveAll(link); rmErr != nil {
			return err
		}
		return os.Symlink(target, link)
	}
	return nil
}

// writeIfChanged swaps data in by rename unless the file already holds it.
func writeIfChanged(path string, data []byte) error {
	if cur, err := os.ReadFile(path); err == nil && string(cur) == string(data) {
		return nil
	}
	return fsutil.Write(path, data, 0o444)
}

// mirrorHost links the host's class entries beside the mock ones, and drops
// entries that are neither mock nor on the host any more.
func (v *classView) mirrorHost(h *host.Host) error {
	v.mu.Lock()
	owned := v.owned
	v.mu.Unlock()

	classRoot := h.RootPath("sys", "class")
	want := map[string]string{} // rel -> link target, or "" for a copied file
	hostClasses, err := os.ReadDir(h.SysPath("class"))
	if err != nil {
		return nil // no host /sys to mirror
	}
	var errs []error
	for _, c := range hostClasses {
		entries, _ := os.ReadDir(h.SysPath("class", c.Name()))
		for _, e := range entries {
			rel := c.Name() + "/" + e.Name()
			if owned[rel] {
				continue
			}
			hostPath := h.SysPath("class", c.Name(), e.Name())
			if target, err := os.Readlink(hostPath); err == nil {
				want[rel] = target
				continue
			}
			if e.Type().IsRegular() {
				want[rel] = ""
				if data, err := os.ReadFile(hostPath); err == nil {
					_ = os.MkdirAll(filepath.Join(classRoot, c.Name()), 0o755)
					_ = fsutil.Write(filepath.Join(classRoot, rel), data, 0o444)
				}
			}
		}
		_ = os.MkdirAll(filepath.Join(classRoot, c.Name()), 0o755)
	}

	for rel, target := range want {
		if target == "" {
			continue
		}
		link := filepath.Join(classRoot, rel)
		if cur, err := os.Readlink(link); err == nil && cur == target {
			continue
		}
		_ = os.RemoveAll(link)
		if err := os.Symlink(target, link); err != nil {
			errs = append(errs, err)
		}
	}

	// Drop what neither side wants any more.
	classes, _ := os.ReadDir(classRoot)
	for _, c := range classes {
		entries, _ := os.ReadDir(filepath.Join(classRoot, c.Name()))
		for _, e := range entries {
			rel := c.Name() + "/" + e.Name()
			if _, ok := want[rel]; ok || owned[rel] {
				continue
			}
			_ = os.RemoveAll(filepath.Join(classRoot, rel))
		}
	}
	return errors.Join(errs...)
}

// refresh keeps the host mirror current: pod interfaces come and go in
// /sys/class/net long after the node agent stages.
func (v *classView) refresh(h *host.Host) {
	v.once.Do(func() {
		go func() {
			t := time.NewTicker(10 * time.Second)
			defer t.Stop()
			for range t.C {
				if err := v.mirrorHost(h); err != nil {
					zap.L().Debug("class view host mirror", zap.Error(err))
				}
			}
		}()
	})
}

// clear removes the class view's entries but keeps sys/class itself.
func (v *classView) clear(_ context.Context, h *host.Host) error {
	classRoot := h.RootPath("sys", "class")
	classes, _ := os.ReadDir(classRoot)
	var errs []error
	for _, c := range classes {
		errs = append(errs, os.RemoveAll(filepath.Join(classRoot, c.Name())))
	}
	return errors.Join(errs...)
}

// pciFunctionRel returns "devices/<rc>/<bdf>" for a BDF in the rendered tree.
func pciFunctionRel(h *host.Host, bdf string) string {
	if bdf == "" {
		return ""
	}
	target, err := os.Readlink(h.RootPath(pcisysfs.PCIDevicesRelPath, bdf))
	if err != nil {
		return ""
	}
	rel := filepath.Clean(filepath.Join("bus/pci/devices", target))
	if !strings.HasPrefix(rel, "devices/") {
		return ""
	}
	return rel
}

func slotName(uevent string) string {
	f, err := os.Open(uevent)
	if err != nil {
		return ""
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		if v, ok := strings.CutPrefix(s.Text(), "PCI_SLOT_NAME="); ok {
			return strings.ToLower(strings.TrimSpace(v))
		}
	}
	return ""
}

func readTrim(path string) string {
	data, _ := os.ReadFile(path)
	return strings.TrimSpace(string(data))
}

// syncTree brings dst in line with src: changed files swapped in by rename,
// unchanged ones left alone, symlinks kept as links, and whatever src no
// longer has removed. The top-level entry named skip is the caller's (the
// link to the PCI function) and is neither copied nor removed.
func syncTree(src, dst, skip string) error {
	want := map[string]bool{}
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		if rel == skip {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		want[rel] = true
		target := filepath.Join(dst, rel)
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return replaceSymlink(link, target)
		case d.IsDir():
			return os.MkdirAll(target, 0o755)
		default:
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			return writeIfChanged(target, data)
		}
	})
	if err != nil {
		return err
	}

	var stale []string
	_ = filepath.WalkDir(dst, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // a vanished entry is already gone
		}
		rel, _ := filepath.Rel(dst, path)
		if rel == "." {
			return nil
		}
		if rel == skip || want[rel] {
			if d.IsDir() && rel == skip {
				return filepath.SkipDir
			}
			return nil
		}
		stale = append(stale, path)
		if d.IsDir() {
			return filepath.SkipDir
		}
		return nil
	})
	var errs []error
	for _, p := range stale {
		if err := os.RemoveAll(p); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
