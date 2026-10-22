// Copyright 2026 NVIDIA CORPORATION
// SPDX-License-Identifier: Apache-2.0

package pcibus

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/k8s-test-infra/internal/ib/config"
	ibsysfs "github.com/NVIDIA/k8s-test-infra/internal/ib/sysfs"
	"github.com/NVIDIA/k8s-test-infra/internal/pcisysfs"
)

// A NIC removed at run time leaves the class view and the PCI bus whole, and
// comes back the same way; a port change reaches the class view in place.
func TestClassView_RuntimeRemovalAndRestore(t *testing.T) {
	t.Parallel()
	h := testHost(t)
	render := func(rt map[string]config.NICOverride) {
		t.Helper()
		require.NoError(t, ibsysfs.Render(ibsysfs.Options{
			IB:      config.Infiniband{Enabled: true, HCACountOverride: 2},
			RootDir: h.RootPath("ib"),
			GPUs:    []ibsysfs.PCIFunction{{Address: "0000:0a:00.0"}},
			Runtime: rt,
		}))
	}
	// The PCI tree as the renderer leaves it: one function per HCA.
	for _, bdf := range []string{"0000:0b:00.0", "0000:0c:00.0"} {
		fn := h.RootPath("sys/devices/pci0000:00", bdf)
		require.NoError(t, os.MkdirAll(fn, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(fn, "vendor"), []byte("0x15b3\n"), 0o444))
		require.NoError(t, os.MkdirAll(h.RootPath(pcisysfs.PCIDevicesRelPath), 0o755))
		require.NoError(t, os.Symlink("../../../devices/pci0000:00/"+bdf, h.RootPath(pcisysfs.PCIDevicesRelPath, bdf)))
	}
	var v classView
	state := func() string {
		data, err := os.ReadFile(h.RootPath("sys/class/infiniband/mlx5_1/ports/1/state"))
		if err != nil {
			return "absent"
		}
		return string(data)
	}

	render(nil)
	require.NoError(t, v.build(h))
	require.Equal(t, "4: ACTIVE\n", state())

	render(map[string]config.NICOverride{"mlx5_1": {Ports: map[string]config.PortOverride{"1": {State: "DOWN"}}}})
	require.NoError(t, v.build(h))
	require.Equal(t, "1: DOWN\n", state())

	render(map[string]config.NICOverride{"mlx5_1": {Removed: true}})
	require.NoError(t, v.build(h))
	require.Equal(t, "absent", state())
	require.NoFileExists(t, h.RootPath("sys/class/infiniband_verbs/uverbs1"))
	require.NoDirExists(t, h.RootPath("sys/devices/pci0000:00/0000:0c:00.0"))
	_, err := os.Lstat(h.RootPath(pcisysfs.PCIDevicesRelPath, "0000:0c:00.0"))
	require.True(t, os.IsNotExist(err), "the removed NIC's bus link must go")
	require.DirExists(t, h.RootPath("sys/devices/pci0000:00/0000:0b:00.0/infiniband/mlx5_0"))

	render(nil)
	require.NoError(t, v.build(h))
	require.Equal(t, "4: ACTIVE\n", state())
	require.FileExists(t, h.RootPath(pcisysfs.PCIDevicesRelPath, "0000:0c:00.0", "vendor"))
}
