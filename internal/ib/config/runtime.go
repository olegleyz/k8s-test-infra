// Copyright 2026 NVIDIA CORPORATION
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"math"
	"time"
)

// NICOverride is one HCA's run-time state on top of its profile: the NIC
// faults nvml-mock-ctl injects. It lives in the runtime override document
// under nics.<mlx5_N>, beside the GPU overrides, and the node agent renders it
// into the HCA's sysfs files. Every field reverts by being removed.
type NICOverride struct {
	// Removed takes the HCA off the node, as if it fell off the PCIe bus: its
	// class devices and its PCI function disappear.
	Removed bool `json:"removed,omitempty"`
	// CharDevicesRemoved drops the HCA's umad, issm and uverbs class entries
	// while its ports stay up: the state in which pods cannot open the device.
	CharDevicesRemoved bool `json:"char_devices_removed,omitempty"`
	// FWVersion replaces fw_ver, as a firmware update does.
	FWVersion string `json:"fw_version,omitempty"`
	// Operstate sets a RoCE netdev's operstate ("up", "down", ...).
	Operstate string `json:"operstate,omitempty"`
	// NetdevCounters overrides the RoCE netdev's counters by path below its
	// directory: "carrier_changes", "statistics/rx_errors".
	NetdevCounters map[string]Counter `json:"netdev_counters,omitempty"`
	// Ports holds per-port overrides keyed by port number ("1").
	Ports map[string]PortOverride `json:"ports,omitempty"`
}

// PortOverride is one port's run-time state.
type PortOverride struct {
	State     string `json:"state,omitempty"`      // "DOWN", "INIT", "ACTIVE", ...
	PhysState string `json:"phys_state,omitempty"` // "Disabled", "Polling", "LinkUp", ...
	// Counters overrides port counters by path below the port directory:
	// "counters/symbol_error", "hw_counters/rnr_nak_retry_err".
	Counters map[string]Counter `json:"counters,omitempty"`
}

// Counter is a counter's value, optionally growing at a constant rate. Value
// is what it read at Since; the stored form stays fixed while the rendered
// value grows, so the document is rewritten only when someone changes it.
type Counter struct {
	Value uint64    `json:"value"`
	Rate  float64   `json:"rate,omitempty"` // per second
	Since time.Time `json:"since,omitzero"`
}

// At is the counter's value at now. A clock that runs backwards reads Value.
func (c Counter) At(now time.Time) uint64 {
	if c.Rate <= 0 || c.Since.IsZero() || !now.After(c.Since) {
		return c.Value
	}
	grown := math.Floor(c.Rate * now.Sub(c.Since).Seconds())
	if grown >= math.MaxUint64-float64(c.Value) {
		return math.MaxUint64
	}
	return c.Value + uint64(grown)
}

// Growing reports whether any counter of o grows on its own, which is what
// tells the renderer to keep re-rendering with nobody touching the document.
func (o NICOverride) Growing() bool {
	for _, c := range o.NetdevCounters {
		if c.Rate > 0 {
			return true
		}
	}
	for _, p := range o.Ports {
		for _, c := range p.Counters {
			if c.Rate > 0 {
				return true
			}
		}
	}
	return false
}
