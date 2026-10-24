// Copyright 2026 NVIDIA CORPORATION
// SPDX-License-Identifier: Apache-2.0

package mockctl

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	ibconfig "github.com/NVIDIA/k8s-test-infra/internal/ib/config"
)

// The NIC half of the runtime override document. Every mutation is
// authoritative for the field it sets and an empty override is pruned, so
// reverting a fault leaves no trace and `status` shows only what is in force.

var nicName = regexp.MustCompile(`^mlx5_\d+$`)

// Port and physical states a fault may set, in the spelling the kernel's
// "<n>: <NAME>" files use. The renderer maps anything else to ACTIVE/LinkUp,
// so an unknown value is refused here rather than silently healing the port.
var (
	portStates = []string{"DOWN", "INIT", "ARMED", "ACTIVE", "ACTIVE_DEFER"}
	physStates = []string{"Sleep", "Polling", "Disabled", "Training", "LinkUp", "LinkErrorRecovery", "PhyTest"}
	operstates = []string{"up", "down", "dormant", "lowerlayerdown", "notpresent", "testing", "unknown"}
)

// ValidateNIC reports whether name is an HCA the renderer can produce.
func ValidateNIC(name string) error {
	if !nicName.MatchString(name) {
		return fmt.Errorf("--nic %q: want an RDMA device name such as mlx5_0", name)
	}
	return nil
}

// nic returns the mutable override for name, creating it.
func (d *Doc) nic(name string) *ibconfig.NICOverride {
	if d.NICs == nil {
		d.NICs = map[string]ibconfig.NICOverride{}
	}
	o := d.NICs[name]
	return &o
}

// putNIC stores o for name, dropping it when nothing is left in force.
func (d *Doc) putNIC(name string, o *ibconfig.NICOverride) {
	for p, port := range o.Ports {
		if port.State == "" && port.PhysState == "" && port.Module == "" && len(port.Counters) == 0 {
			delete(o.Ports, p)
		}
	}
	if !o.Removed && !o.CharDevicesRemoved && o.FWVersion == "" && o.Operstate == "" && o.Health == "" &&
		len(o.NetdevCounters) == 0 && len(o.Ports) == 0 {
		delete(d.NICs, name)
		return
	}
	d.NICs[name] = *o
}

// SetNICPortState sets (or, with empty values, clears) a port's state and
// physical state.
func (d *Doc) SetNICPortState(name, port, state, phys string) error {
	if state != "" {
		s, ok := canonical(portStates, state)
		if !ok {
			return fmt.Errorf("--state %q: want one of %s", state, strings.Join(portStates, ", "))
		}
		state = s
	}
	if phys != "" {
		s, ok := canonical(physStates, phys)
		if !ok {
			return fmt.Errorf("--phys-state %q: want one of %s", phys, strings.Join(physStates, ", "))
		}
		phys = s
	}
	o := d.nic(name)
	p := o.Ports[port]
	p.State, p.PhysState = state, phys
	setPort(o, port, p)
	d.putNIC(name, o)
	return nil
}

// SetNICModule faults (or, with an empty fault, clears) the cable or
// transceiver of a port. Network use case O4, PoC.
func (d *Doc) SetNICModule(name, port, fault string) error {
	if fault != "" && !slices.Contains(ibconfig.ModuleFaults, fault) {
		return fmt.Errorf("module fault %q: want one of %s", fault, strings.Join(ibconfig.ModuleFaults, ", "))
	}
	o := d.nic(name)
	p := o.Ports[port]
	p.Module = fault
	setPort(o, port, p)
	d.putNIC(name, o)
	return nil
}

// SetNICHealth fails (or, with an empty fault, recovers) the HCA's firmware.
// Network use case O4, PoC.
func (d *Doc) SetNICHealth(name, fault string) error {
	if fault != "" && !slices.Contains(ibconfig.HealthFaults, fault) {
		return fmt.Errorf("health fault %q: want one of %s", fault, strings.Join(ibconfig.HealthFaults, ", "))
	}
	o := d.nic(name)
	o.Health = fault
	d.putNIC(name, o)
	return nil
}

// SetNICRemoved takes the HCA off the bus, or puts it back.
func (d *Doc) SetNICRemoved(name string, removed bool) {
	o := d.nic(name)
	o.Removed = removed
	d.putNIC(name, o)
}

// SetNICCharDevicesRemoved drops or restores the HCA's umad, issm and uverbs
// entries.
func (d *Doc) SetNICCharDevicesRemoved(name string, removed bool) {
	o := d.nic(name)
	o.CharDevicesRemoved = removed
	d.putNIC(name, o)
}

// SetNICFirmware replaces the HCA's fw_ver; empty restores the profile's.
func (d *Doc) SetNICFirmware(name, version string) {
	o := d.nic(name)
	o.FWVersion = version
	d.putNIC(name, o)
}

// SetNICOperstate sets a RoCE netdev's operstate; empty lets it follow the
// port again.
func (d *Doc) SetNICOperstate(name, state string) error {
	if state != "" {
		s, ok := canonical(operstates, state)
		if !ok {
			return fmt.Errorf("operstate %q: want one of %s", state, strings.Join(operstates, ", "))
		}
		state = s
	}
	o := d.nic(name)
	o.Operstate = state
	d.putNIC(name, o)
	return nil
}

// CounterChange is one counter command. Exactly one of its fields is set.
type CounterChange struct {
	Set   *uint64
	Add   *uint64
	Rate  *float64 // per second; 0 stops the growth where it is
	Reset bool     // back to 0, growth stopped, as `perfquery -x` does
	Clear bool     // drop the override: the counter reads its profile value
}

// ChangeNICCounter applies one counter command at now. path names the counter
// below its port ("counters/symbol_error", "hw_counters/rnr_nak_retry_err") or
// below the RoCE netdev ("carrier_changes", "statistics/rx_errors").
func (d *Doc) ChangeNICCounter(name, port, path string, c CounterChange, now time.Time) error {
	netdev, err := counterScope(path)
	if err != nil {
		return err
	}
	o := d.nic(name)
	counters := o.NetdevCounters
	if !netdev {
		counters = o.Ports[port].Counters
	}
	if counters == nil {
		counters = map[string]ibconfig.Counter{}
	}

	cur := counters[path]
	next := ibconfig.Counter{Value: cur.At(now), Rate: cur.Rate, Since: now}
	switch {
	case c.Clear:
		delete(counters, path)
	case c.Reset:
		counters[path] = ibconfig.Counter{}
	case c.Set != nil:
		next.Value = *c.Set
		counters[path] = next
	case c.Add != nil:
		next.Value += *c.Add
		counters[path] = next
	case c.Rate != nil:
		next.Rate = *c.Rate
		counters[path] = next
	default:
		return fmt.Errorf("counter %s: nothing to change", path)
	}
	for k, v := range counters {
		if v.Rate <= 0 {
			v.Rate, v.Since = 0, time.Time{}
			counters[k] = v
		}
	}

	if netdev {
		o.NetdevCounters = counters
	} else {
		p := o.Ports[port]
		p.Counters = counters
		setPort(o, port, p)
	}
	d.putNIC(name, o)
	return nil
}

// ResetNIC drops every override of name, or of every NIC when name is empty.
func (d *Doc) ResetNIC(name string) {
	if name == "" {
		d.NICs = nil
		return
	}
	delete(d.NICs, name)
}

func setPort(o *ibconfig.NICOverride, port string, p ibconfig.PortOverride) {
	if o.Ports == nil {
		o.Ports = map[string]ibconfig.PortOverride{}
	}
	o.Ports[port] = p
}

// counterScope reports whether path names a netdev counter rather than a port
// counter, refusing anything that is neither: the renderer writes a counter
// file wherever the path points.
func counterScope(path string) (netdev bool, err error) {
	dir, file, nested := strings.Cut(path, "/")
	switch {
	case !nested && dir == "carrier_changes":
		return true, nil
	case nested && dir == "statistics" && validCounterName(file):
		return true, nil
	case nested && (dir == "counters" || dir == "hw_counters") && validCounterName(file):
		return false, nil
	}
	return false, fmt.Errorf("counter %q: want counters/<name>, hw_counters/<name>, statistics/<name> or carrier_changes", path)
}

var counterFile = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

func validCounterName(s string) bool { return counterFile.MatchString(s) }

// canonical returns the entry of allowed that s names, ignoring case.
func canonical(allowed []string, s string) (string, bool) {
	i := slices.IndexFunc(allowed, func(a string) bool { return strings.EqualFold(a, s) })
	if i < 0 {
		return "", false
	}
	return allowed[i], true
}
