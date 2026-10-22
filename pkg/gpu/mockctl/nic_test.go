// Copyright 2026 NVIDIA CORPORATION
// SPDX-License-Identifier: Apache-2.0

package mockctl

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNICCounterCommands(t *testing.T) {
	t.Parallel()
	d := &Doc{}
	t0 := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	five, rate := uint64(5), 2.0

	require.NoError(t, d.ChangeNICCounter("mlx5_0", "1", "counters/symbol_error", CounterChange{Set: &five}, t0))
	require.NoError(t, d.ChangeNICCounter("mlx5_0", "1", "counters/symbol_error", CounterChange{Rate: &rate}, t0))
	// Adding once keeps the growth and starts it from the grown value.
	require.NoError(t, d.ChangeNICCounter("mlx5_0", "1", "counters/symbol_error", CounterChange{Add: &five}, t0.Add(10*time.Second)))
	c := d.NICs["mlx5_0"].Ports["1"].Counters["counters/symbol_error"]
	require.Equal(t, uint64(30), c.At(t0.Add(10*time.Second)))
	require.Equal(t, uint64(32), c.At(t0.Add(11*time.Second)))

	require.NoError(t, d.ChangeNICCounter("mlx5_0", "1", "counters/symbol_error", CounterChange{Reset: true}, t0))
	require.Equal(t, uint64(0), d.NICs["mlx5_0"].Ports["1"].Counters["counters/symbol_error"].At(t0.Add(time.Hour)))

	require.NoError(t, d.ChangeNICCounter("mlx5_0", "1", "counters/symbol_error", CounterChange{Clear: true}, t0))
	require.Empty(t, d.NICs, "a cleared counter leaves no override behind")

	require.Error(t, d.ChangeNICCounter("mlx5_0", "1", "../../fw_ver", CounterChange{Set: &five}, t0))
	require.NoError(t, d.ChangeNICCounter("mlx5_0", "1", "carrier_changes", CounterChange{Add: &five}, t0))
	require.Equal(t, uint64(5), d.NICs["mlx5_0"].NetdevCounters["carrier_changes"].Value)
}

func TestNICFaultsRevert(t *testing.T) {
	t.Parallel()
	d := &Doc{}
	require.NoError(t, d.SetNICPortState("mlx5_1", "1", "down", "disabled"))
	require.Equal(t, "DOWN", d.NICs["mlx5_1"].Ports["1"].State)
	require.Equal(t, "Disabled", d.NICs["mlx5_1"].Ports["1"].PhysState)
	require.Error(t, d.SetNICPortState("mlx5_1", "1", "broken", ""))

	d.SetNICRemoved("mlx5_2", true)
	d.SetNICCharDevicesRemoved("mlx5_3", true)
	require.NoError(t, d.SetNICPortState("mlx5_1", "1", "", ""))
	d.SetNICRemoved("mlx5_2", false)
	d.SetNICCharDevicesRemoved("mlx5_3", false)
	require.Empty(t, d.NICs)

	require.Error(t, ValidateNIC("eth0"))
	require.NoError(t, ValidateNIC("mlx5_12"))
}
