// Copyright 2026 NVIDIA CORPORATION
// SPDX-License-Identifier: Apache-2.0

package sysfs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/k8s-test-infra/internal/ib/config"
	"github.com/stretchr/testify/require"
)

func readTree(t *testing.T, dir, rel string) string {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(dir, rel))
	require.NoError(t, err, "read %s", rel)
	return strings.TrimSpace(string(got))
}

// One node can mix NICs: a RoCE storage port on another NUMA node, pinned to
// the second function of a card, beside InfiniBand compute HCAs.
func TestRender_PerHCASettings(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	numa := 3
	err := Render(Options{
		IB: config.Infiniband{Enabled: true, HCACountOverride: 2, HCAs: []config.HCA{
			{},
			{LinkLayer: "Ethernet", PCIAddress: "0000:0C:00.1", NUMANode: &numa, Netdev: "ens1f1np1",
				HCAType: "MT4125", PortState: "DOWN", PhysState: "Disabled", PhysFn: "0000:0c:00.0"},
		}},
		RootDir: dir,
		GPUs:    []PCIFunction{{Address: "0000:0a:00.0", NUMANode: 0}},
	})
	require.NoError(t, err)

	require.Equal(t, "InfiniBand", readTree(t, dir, "sys/class/infiniband/mlx5_0/ports/1/link_layer"))
	require.Equal(t, "Ethernet", readTree(t, dir, "sys/class/infiniband/mlx5_1/ports/1/link_layer"))
	require.Equal(t, "MT4125", readTree(t, dir, "sys/class/infiniband/mlx5_1/hca_type"))
	require.Equal(t, "1: DOWN", readTree(t, dir, "sys/class/infiniband/mlx5_1/ports/1/state"))
	require.Equal(t, "3", readTree(t, dir, "sys/class/infiniband/mlx5_1/device/numa_node"))
	require.Contains(t, readTree(t, dir, "sys/class/infiniband/mlx5_1/device/uevent"), "PCI_SLOT_NAME=0000:0c:00.1")
	require.Equal(t, "down", readTree(t, dir, "sys/class/net/ens1f1np1/operstate"))
	link, err := os.Readlink(filepath.Join(dir, "sys/class/infiniband/mlx5_1/device/physfn"))
	require.NoError(t, err)
	require.Equal(t, "../0000:0c:00.0", link)
	// The pinned card's bus is not handed to the auto-placed HCA.
	require.NotContains(t, readTree(t, dir, "sys/class/infiniband/mlx5_0/device/uevent"), "0000:0c:")
}

func TestRender_RuntimeFaults(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	start := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	opts := Options{
		IB:      config.Infiniband{Enabled: true, HCACountOverride: 3},
		RootDir: dir,
		Now:     start.Add(10 * time.Second),
		Runtime: map[string]config.NICOverride{
			"mlx5_0": {FWVersion: "28.44.1000", Ports: map[string]config.PortOverride{"1": {
				State: "DOWN", PhysState: "Disabled",
				Counters: map[string]config.Counter{
					"counters/symbol_error":         {Value: 5, Rate: 2, Since: start},
					"hw_counters/rnr_nak_retry_err": {Value: 7},
					"counters/port_rcv_discards":    {Value: 1}, // not in the default set
				},
			}}},
			"mlx5_1": {CharDevicesRemoved: true},
			"mlx5_2": {Removed: true},
		},
	}
	require.NoError(t, Render(opts))

	require.Equal(t, "1: DOWN", readTree(t, dir, "sys/class/infiniband/mlx5_0/ports/1/state"))
	require.Equal(t, "3: Disabled", readTree(t, dir, "sys/class/infiniband/mlx5_0/ports/1/phys_state"))
	require.Equal(t, "28.44.1000", readTree(t, dir, "sys/class/infiniband/mlx5_0/fw_ver"))
	require.Equal(t, "25", readTree(t, dir, "sys/class/infiniband/mlx5_0/ports/1/counters/symbol_error"))
	require.Equal(t, "7", readTree(t, dir, "sys/class/infiniband/mlx5_0/ports/1/hw_counters/rnr_nak_retry_err"))
	require.Equal(t, "1", readTree(t, dir, "sys/class/infiniband/mlx5_0/ports/1/counters/port_rcv_discards"))

	require.DirExists(t, filepath.Join(dir, "sys/class/infiniband/mlx5_1"))
	require.NoDirExists(t, filepath.Join(dir, "sys/class/infiniband_verbs/uverbs1"))
	require.NoDirExists(t, filepath.Join(dir, "sys/class/infiniband_mad/umad1"))
	require.NoFileExists(t, filepath.Join(dir, "dev/infiniband/uverbs1"))

	require.NoDirExists(t, filepath.Join(dir, "sys/class/infiniband/mlx5_2"))
	require.NoDirExists(t, filepath.Join(dir, "sys/class/infiniband_verbs/uverbs2"))

	// Reverting is rendering without the override.
	opts.Runtime = nil
	require.NoError(t, Render(opts))
	require.Equal(t, "4: ACTIVE", readTree(t, dir, "sys/class/infiniband/mlx5_0/ports/1/state"))
	require.Equal(t, "0", readTree(t, dir, "sys/class/infiniband/mlx5_0/ports/1/counters/symbol_error"))
	require.NoFileExists(t, filepath.Join(dir, "sys/class/infiniband/mlx5_0/ports/1/counters/port_rcv_discards"))
	require.DirExists(t, filepath.Join(dir, "sys/class/infiniband_verbs/uverbs1"))
	require.DirExists(t, filepath.Join(dir, "sys/class/infiniband/mlx5_2"))
}

func TestCounterAt(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	c := config.Counter{Value: 10, Rate: 0.5, Since: start}
	require.Equal(t, uint64(10), c.At(start))
	require.Equal(t, uint64(10), c.At(start.Add(-time.Hour)))
	require.Equal(t, uint64(40), c.At(start.Add(time.Minute)))
	require.Equal(t, uint64(3), config.Counter{Value: 3}.At(start.Add(time.Hour)))
}
