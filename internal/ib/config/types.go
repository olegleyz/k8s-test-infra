// Copyright 2026 NVIDIA CORPORATION
// SPDX-License-Identifier: Apache-2.0

// Package config defines the InfiniBand block embedded in mock-nvml profile
// YAML. The node agent parses it into its own compiled state; the renderer
// takes the resolved Infiniband value and writes the sysfs tree from it.
package config

// Profile is the slice of the mock-nvml profile YAML this package cares about,
// so the same file can be unmarshalled a second time without modelling the GPU
// fields the NVML engine owns.
type Profile struct {
	Infiniband Infiniband `json:"infiniband" yaml:"infiniband"`
}

// Infiniband describes the simulated HCA topology rendered into sysfs. All
// fields have sensible zero-value defaults applied by ApplyDefaults so that
// minimal profile blocks (e.g. {enabled: true}) still produce realistic
// output.
type Infiniband struct {
	Enabled bool `json:"enabled" yaml:"enabled"`

	// Per-CA static metadata.
	HCAType   string `json:"hca_type"   yaml:"hca_type"`   // e.g. "MT4129" (ConnectX-7)
	FWVersion string `json:"fw_version" yaml:"fw_version"` // e.g. "28.39.2048"
	HWRev     string `json:"hw_rev"     yaml:"hw_rev"`     // e.g. "0x0"
	BoardID   string `json:"board_id"   yaml:"board_id"`   // e.g. "MT_0000000838"

	// NodeDescTemplate supports placeholders {node_name} and {idx}.
	NodeDescTemplate string `json:"node_desc_template" yaml:"node_desc_template"`

	// Per-port settings.
	LinkLayer string `json:"link_layer" yaml:"link_layer"` // "InfiniBand" | "Ethernet"
	RateGbps  int    `json:"rate_gbps"  yaml:"rate_gbps"`  // 100, 200, 400, ...
	PortState string `json:"port_state" yaml:"port_state"` // "ACTIVE" | "INIT" | "DOWN"
	PhysState string `json:"phys_state" yaml:"phys_state"` // "LinkUp" | "Disabled" | ...

	// Topology shape.
	HCAsPerGPU       int `json:"hcas_per_gpu" yaml:"hcas_per_gpu"` // total = gpu_count * hcas_per_gpu
	HCACountOverride int `json:"hca_count" yaml:"hca_count"`       // if >0, used instead of gpu_count*hcas_per_gpu

	// GUIDPrefix supplies the fixed upper bytes of every node/port GUID, in
	// hex (with optional ':' separators). The renderer uses the first 4 bytes
	// as the fixed prefix, then packs a 23-bit node hash, 8-bit HCA index, and
	// the EUI-64 U/L bit into the lower 4 bytes. Example:
	// "a088c2:0300:ab" -> fixed prefix "a088:c203" -> per-HCA node GUID
	// "a088:c203:<node+idx>" with the matching port GUID one greater.
	GUIDPrefix string `json:"guid_prefix" yaml:"guid_prefix"`

	// HCAs overrides the block's values for individual HCAs, by index: entry i
	// describes mlx5_i. A real node mixes NICs (an InfiniBand compute fabric
	// next to RoCE storage ports, a card cabled on fewer ports than its peers),
	// which one block-wide value cannot express. Unset fields keep the block's
	// value. A list longer than the resolved HCA count raises the count.
	HCAs []HCA `json:"hcas,omitempty" yaml:"hcas,omitempty"`
}

// HCA is one HCA's departures from the block-wide values. Empty strings and a
// nil NUMANode keep the block's value.
type HCA struct {
	HCAType   string `json:"hca_type,omitempty"   yaml:"hca_type,omitempty"`
	FWVersion string `json:"fw_version,omitempty" yaml:"fw_version,omitempty"`
	LinkLayer string `json:"link_layer,omitempty" yaml:"link_layer,omitempty"`
	PortState string `json:"port_state,omitempty" yaml:"port_state,omitempty"`
	PhysState string `json:"phys_state,omitempty" yaml:"phys_state,omitempty"`

	// PCIAddress pins the HCA's PCI function (DDDD:BB:DD.F) instead of
	// placing it beside its GPU. Two ports of one card share bus and device
	// and differ in function.
	PCIAddress string `json:"pci_address,omitempty" yaml:"pci_address,omitempty"`
	// NUMANode replaces the NUMA node the HCA inherits from its GPU's root
	// complex, for NICs outside the compute fabric (storage, management).
	NUMANode *int `json:"numa_node,omitempty" yaml:"numa_node,omitempty"`
	// Netdev names a RoCE HCA's Ethernet interface instead of the name
	// systemd's predictable naming derives from its address.
	Netdev string `json:"netdev,omitempty" yaml:"netdev,omitempty"`
	// PhysFn marks the HCA an SR-IOV virtual function of the PF at this PCI
	// address, which is what device/physfn tells a consumer.
	PhysFn string `json:"physfn,omitempty" yaml:"physfn,omitempty"`
}

// ForHCA returns the block as HCA idx sees it: the per-HCA entry's fields over
// the block's values. Only the fields a block can carry are merged; placement
// (address, NUMA node, netdev, PF) is read from the entry by its consumers.
func (ib Infiniband) ForHCA(idx int) Infiniband {
	out := ib
	out.HCAs = nil
	if idx < 0 || idx >= len(ib.HCAs) {
		return out
	}
	h := ib.HCAs[idx]
	for _, f := range []struct {
		dst *string
		src string
	}{
		{&out.HCAType, h.HCAType}, {&out.FWVersion, h.FWVersion}, {&out.LinkLayer, h.LinkLayer},
		{&out.PortState, h.PortState}, {&out.PhysState, h.PhysState},
	} {
		if f.src != "" {
			*f.dst = f.src
		}
	}
	return out
}

// HCAEntry returns the per-HCA entry for idx, or the zero entry.
func (ib Infiniband) HCAEntry(idx int) HCA {
	if idx < 0 || idx >= len(ib.HCAs) {
		return HCA{}
	}
	return ib.HCAs[idx]
}

// Defaults returns a copy of the InfiniBand block with reasonable fallback
// values applied for any unset string/int fields. The returned value is safe
// to render directly.
//
//nolint:cyclop // existing complexity; refactor deferred
func (ib Infiniband) Defaults() Infiniband {
	out := ib
	if out.HCAType == "" {
		out.HCAType = "MT4129" // ConnectX-7 NDR
	}
	if out.FWVersion == "" {
		out.FWVersion = "28.39.2048"
	}
	if out.HWRev == "" {
		out.HWRev = "0x0"
	}
	if out.BoardID == "" {
		out.BoardID = "MT_0000000838"
	}
	if out.NodeDescTemplate == "" {
		out.NodeDescTemplate = "{node_name} mlx5_{idx}"
	}
	if out.LinkLayer == "" {
		out.LinkLayer = "InfiniBand"
	}
	if out.RateGbps == 0 {
		out.RateGbps = 400
	}
	if out.PortState == "" {
		out.PortState = "ACTIVE"
	}
	if out.PhysState == "" {
		out.PhysState = "LinkUp"
	}
	if out.HCAsPerGPU == 0 {
		out.HCAsPerGPU = 1
	}
	if out.GUIDPrefix == "" {
		out.GUIDPrefix = "a088c20300ab"
	}
	return out
}
