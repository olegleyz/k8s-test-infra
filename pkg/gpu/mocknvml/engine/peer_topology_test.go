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
	"os"
	"path/filepath"
	"testing"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/stretchr/testify/require"
)

func TestPairLevel(t *testing.T) {
	t.Parallel()
	rc0 := pciLocality{root: "pci0000:00", numa: 0}
	rc1 := pciLocality{root: "pci0000:20", numa: 0}
	far := pciLocality{root: "pci0000:40", numa: 1}

	require.Equal(t, nvml.TOPOLOGY_SINGLE, pairLevel(rc0, rc0))
	require.Equal(t, nvml.TOPOLOGY_HOSTBRIDGE, pairLevel(rc0, rc1))
	require.Equal(t, nvml.TOPOLOGY_SYSTEM, pairLevel(rc0, far))
}

func TestKernelBDF(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"00000000:0A:00.0": "0000:0a:00.0", // NVML's form
		"0000:0c:00.1":     "0000:0c:00.1", // hwloc's form
	} {
		got, err := kernelBDF(in)
		require.NoError(t, err)
		require.Equal(t, want, got, in)
	}
	_, err := kernelBDF("mlx5_0")
	require.Error(t, err)
}

// A NIC is placed from the PCI tree: its root complex from the bus/pci/devices
// link, its NUMA node from numa_node.
func TestSysfsLocality(t *testing.T) {
	root := t.TempDir()
	fn := filepath.Join(root, "sys/devices/pci0000:40/0000:4c:00.0")
	require.NoError(t, os.MkdirAll(fn, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(fn, "numa_node"), []byte("1\n"), 0o644))
	devices := filepath.Join(root, "sys/bus/pci/devices")
	require.NoError(t, os.MkdirAll(devices, 0o755))
	require.NoError(t, os.Symlink("../../../devices/pci0000:40/0000:4c:00.0", filepath.Join(devices, "0000:4c:00.0")))
	t.Setenv("MOCK_PCI_ROOT", root)

	loc, ok := sysfsLocality("0000:4c:00.0")
	require.True(t, ok)
	require.Equal(t, pciLocality{root: "pci0000:40", numa: 1}, loc)

	_, ok = sysfsLocality("0000:7f:00.0")
	require.False(t, ok)
}
