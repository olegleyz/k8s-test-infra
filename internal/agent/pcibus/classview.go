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
	mu    sync.Mutex
	owned map[string]bool // "net/enp12s0np0", "infiniband/mlx5_0", ...
	once  sync.Once
}

// build rebuilds the mock class entries from the InfiniBand tree, then
// mirrors the host's. sys/class itself is never replaced: it is a bind source
// in running containers, which would keep the old inode.
func (v *classView) build(h *host.Host) error {
	classRoot := h.RootPath("sys", "class")
	if err := os.MkdirAll(classRoot, 0o755); err != nil {
		return err
	}

	owned := map[string]bool{}
	var errs []error
	ibClass := h.RootPath("ib", "sys", "class")

	hcas, _ := os.ReadDir(filepath.Join(ibClass, "infiniband"))
	for _, hca := range hcas {
		name := hca.Name()
		src := filepath.Join(ibClass, "infiniband", name)
		bdf := slotName(filepath.Join(src, "device", "uevent"))
		pciRel := pciFunctionRel(h, bdf)
		if pciRel == "" {
			zap.L().Debug("class view: HCA has no PCI function in the tree", zap.String("hca", name), zap.String("bdf", bdf))
			continue
		}
		pciDir := h.RootPath("sys", pciRel)

		// The PCI function carries the HCA's uevent and modalias, so a reader
		// that follows device/ gets the same identity as the InfiniBand tree.
		for _, f := range []string{"uevent", "modalias"} {
			if data, err := os.ReadFile(filepath.Join(src, "device", f)); err == nil {
				errs = append(errs, fsutil.Write(filepath.Join(pciDir, f), data, 0o444))
			}
		}

		errs = append(errs, v.publish(h, owned, "infiniband", name, src, pciRel, "infiniband", "../../../"+bdf, "device"))

		for _, sub := range []string{"infiniband_mad", "infiniband_verbs"} {
			entries, _ := os.ReadDir(filepath.Join(ibClass, sub))
			for _, e := range entries {
				esrc := filepath.Join(ibClass, sub, e.Name())
				if !e.IsDir() || readTrim(filepath.Join(esrc, "ibdev")) != name {
					continue
				}
				errs = append(errs, v.publish(h, owned, sub, e.Name(), esrc, pciRel, sub, "../../../"+bdf, "device"))
			}
		}

		netdevs, _ := os.ReadDir(filepath.Join(src, "device", "net"))
		for _, n := range netdevs {
			nsrc := filepath.Join(ibClass, "net", n.Name())
			if _, err := os.Stat(nsrc); err != nil {
				continue
			}
			errs = append(errs, v.publish(h, owned, "net", n.Name(), nsrc, pciRel, "net", "../../../"+bdf, "device"))
		}
	}

	for _, c := range []string{"infiniband_mad", "infiniband_verbs"} {
		if data, err := os.ReadFile(filepath.Join(ibClass, c, "abi_version")); err == nil {
			_ = os.MkdirAll(filepath.Join(classRoot, c), 0o755)
			errs = append(errs, fsutil.Write(filepath.Join(classRoot, c, "abi_version"), data, 0o444))
			owned[c+"/abi_version"] = true
		}
	}

	v.mu.Lock()
	v.owned = owned
	v.mu.Unlock()

	errs = append(errs, v.mirrorHost(h))
	return errors.Join(errs...)
}

// publish copies one class device into the PCI function's directory, points
// its link attribute at the function, and links it from /sys/class/<class>.
func (v *classView) publish(h *host.Host, owned map[string]bool, class, name, src, pciRel, sub, linkTarget, linkName string) error {
	dstRel := filepath.Join(pciRel, sub, name)
	dst := h.RootPath("sys", dstRel)
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	if err := copyTree(src, dst, linkName); err != nil {
		return fmt.Errorf("class view %s/%s: %w", class, name, err)
	}
	if err := os.Symlink(linkTarget, filepath.Join(dst, linkName)); err != nil {
		return err
	}
	if err := os.MkdirAll(h.RootPath("sys", "class", class), 0o755); err != nil {
		return err
	}
	link := h.RootPath("sys", "class", class, name)
	_ = os.RemoveAll(link)
	owned[class+"/"+name] = true
	return os.Symlink(filepath.Join("..", "..", dstRel), link)
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

// netdevs returns the simulated netdevs the view published, each true when
// its rendered operstate is up.
func (v *classView) netdevs(h *host.Host) map[string]bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	up := map[string]bool{}
	for rel := range v.owned {
		if name, ok := strings.CutPrefix(rel, "net/"); ok {
			up[name] = readTrim(h.RootPath("ib", "sys", "class", "net", name, "operstate")) == "up"
		}
	}
	return up
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

// copyTree copies src to dst, keeping symlinks as links and skipping the top
// level entry named skip (replaced by a link to the PCI function).
func copyTree(src, dst, skip string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
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
		target := filepath.Join(dst, rel)
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case d.IsDir():
			return os.MkdirAll(target, 0o755)
		default:
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			return fsutil.Write(target, data, 0o444)
		}
	})
}
