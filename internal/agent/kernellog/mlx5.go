// Copyright 2026 NVIDIA CORPORATION
// SPDX-License-Identifier: Apache-2.0

package kernellog

import (
	"fmt"
	"strings"
	"time"

	ibconfig "github.com/NVIDIA/k8s-test-infra/internal/ib/config"
	"github.com/NVIDIA/k8s-test-infra/internal/ib/sysfs"
)

// Network use case O4, PoC.
//
// What the mlx5 driver prints when a NIC fault happens, as Linux v6.8 prints
// it (drivers/net/ethernet/mellanox/mlx5/core: health.c, events.c, cmd.c,
// en_main.c, main.c). The function names and line numbers are that kernel's:
// mlx5_core_err and mlx5_log prefix "<func>:<line>:(pid <pid>): ", dev_info
// does not. Health agents match on the text, so the text is what has to be
// right; the line numbers only make a record read like a real one.
//
// Each record carries its printk level and the "kernel: " prefix the Xid record
// explains: journald strips it into SYSLOG_IDENTIFIER=kernel, which is what a
// record printed by the driver itself carries.

// Printk levels the driver logs at.
const (
	levelCrit = 2
	levelErr  = 3
	levelWarn = 4
	levelInfo = 6
)

// Process IDs in the "(pid N)" prefix: the health poller runs from a timer and
// usually interrupts the idle task; recovery runs from a kernel worker.
const (
	pidTimer  = 0
	pidWorker = 1180
)

// nicView is what the kernel log has said about one NIC: the faults a reader
// of it would take to be in force.
type nicView struct {
	removed bool
	health  string // firmware health fault
	module  string // port 1's module fault
	carrier bool   // RoCE netdev carrier
}

// viewOf is the view the faults in o give NIC n.
func viewOf(n sysfs.HCAPlacement, o ibconfig.NICOverride) nicView {
	port := o.Ports["1"]
	state, _ := port.LinkState(n.PortState, n.PhysState)
	// The carrier follows the port, as the renderer's netdev does, unless a
	// fault sets the netdev's operstate on its own.
	carrier := strings.EqualFold(state, "ACTIVE")
	if o.Operstate != "" {
		carrier = strings.EqualFold(o.Operstate, "up")
	}
	return nicView{removed: o.Removed, health: o.Health, module: port.Module, carrier: carrier}
}

// mlx5Records returns the records the driver prints taking NIC n from was to
// now, in the order it prints them; none when nothing it reports changed.
func mlx5Records(n sysfs.HCAPlacement, fw string, was, now nicView, at time.Time) []string {
	p := mlx5Printer{bdf: strings.ToLower(n.Address), netdev: n.Netdev}

	switch {
	case !was.removed && now.removed:
		return p.offTheBus()
	case was.removed && !now.removed:
		// A NIC that returns is probed afresh. Whatever else is in force is
		// its starting state, which the driver does not report as a change.
		return p.probe(fw, now.module == "")
	case now.removed:
		return nil
	}

	var out []string
	if was.health != now.health {
		if now.health == "" {
			out = append(out, p.recovered(was.health)...)
		} else {
			out = append(out, p.healthFault(now.health, fw, at)...)
		}
	}
	// The module event comes first: it is what takes the link down.
	if was.module != now.module {
		out = append(out, p.moduleEvent(now.module))
	}
	if n.Netdev != "" && was.carrier != now.carrier {
		out = append(out, p.carrier(now.carrier))
	}
	return out
}

type mlx5Printer struct {
	bdf    string
	netdev string
}

// dev is dev_printk: "mlx5_core <bdf>: <msg>".
func (p mlx5Printer) dev(level int, msg string) string {
	return fmt.Sprintf("<%d>kernel: mlx5_core %s: %s", level, p.bdf, msg)
}

// core is mlx5_core_err, mlx5_core_warn and mlx5_log, which add the caller.
func (p mlx5Printer) core(level int, fn string, line, pid int, msg string) string {
	return p.dev(level, fmt.Sprintf("%s:%d:(pid %d): %s", fn, line, pid, msg))
}

// offTheBus is what the health poller prints when PCI reads start returning
// all ones: the fatal sensor trips, the health buffer is unreadable, and the
// recovery flow gives up waiting for PCI. It is a slot without hotplug, the
// usual case for a NIC behind a GPU baseboard's PCIe switch; a hotplug slot
// would add pciehp's own lines.
func (p mlx5Printer) offTheBus() []string {
	return []string{
		p.core(levelErr, "poll_health", 762, pidTimer, "Fatal error 1 detected"),
		p.core(levelErr, "print_health_info", 417, pidTimer, "PCI slot is unavailable"),
		p.core(levelWarn, "mlx5_health_try_recover", 327, pidWorker, "handling bad device here"),
		p.core(levelErr, "mlx5_health_try_recover", 330, pidWorker,
			"health recovery flow aborted, PCI reads still not working"),
	}
}

// probe is what a NIC returning to the bus prints as the driver binds it.
func (p mlx5Printer) probe(fw string, cabled bool) []string {
	out := []string{
		p.dev(levelInfo, "firmware version: "+fw),
		p.dev(levelInfo, "252.048 Gb/s available PCIe bandwidth (16.0 GT/s PCIe x16 link)"),
	}
	if cabled {
		out = append(out, p.moduleEvent(""))
	}
	return out
}

// healthFault is what the health poller and the command interface print when
// the firmware fails while the NIC stays on the bus.
func (p mlx5Printer) healthFault(fault, fw string, at time.Time) []string {
	switch fault {
	case ibconfig.HealthHWFatal:
		// The firmware raised syndrome 0x8 with the reset-for-recovery bit,
		// which the poller reads as fatal sensor 5 and dumps the health buffer.
		return append([]string{
			p.core(levelErr, "poll_health", 762, pidTimer, "Fatal error 5 detected"),
		}, p.healthBuffer(0x8, "unrecoverable hardware error", fw, at)...)
	case ibconfig.HealthHeartbeatLost:
		// Three polls without the counter moving. The syndrome is still 0, so
		// the buffer dump that follows prints nothing.
		return []string{
			p.core(levelErr, "poll_health", 778, pidTimer, "device's health compromised - reached miss count"),
		}
	case ibconfig.HealthCmdTimeout:
		return []string{
			p.core(levelWarn, "wait_func", 1155, pidWorker,
				"ACCESS_REG(0x805) timeout. Will cause a leak of a command resource"),
		}
	}
	return nil
}

// healthBuffer is print_health_info's dump at critical severity.
func (p mlx5Printer) healthBuffer(synd int, desc, fw string, at time.Time) []string {
	log := func(line int, msg string) string { return p.core(levelCrit, "print_health_info", line, pidTimer, msg) }
	out := []string{log(423, fmt.Sprintf("Health issue observed, %s, severity(%d) CRITICAL:", desc, levelCrit))}
	for i := range 6 {
		out = append(out, log(427, fmt.Sprintf("assert_var[%d] 0x00000000", i)))
	}
	return append(out,
		log(430, "assert_exit_ptr 0x00000000"),
		log(431, "assert_callra 0x00000000"),
		log(432, "fw_ver "+fw),
		log(434, fmt.Sprintf("time %d", at.Unix())),
		log(435, "hw_id 0x00000221"),
		log(436, "rfr 1"),
		log(437, fmt.Sprintf("severity %d (CRITICAL)", levelCrit)),
		log(438, "irisc_index 0"),
		log(439, fmt.Sprintf("synd 0x%x: %s", synd, desc)),
		log(441, "ext_synd 0x0000"),
		log(442, fmt.Sprintf("raw fw_ver 0x%08x", rawFWVersion(fw))),
	)
}

// rawFWVersion packs a "major.minor.subminor" version into one register word.
// Illustrative: no consumer decodes it.
func rawFWVersion(fw string) uint32 {
	var maj, minor, sub uint32
	_, _ = fmt.Sscanf(fw, "%d.%d.%d", &maj, &minor, &sub)
	return maj<<24 | (minor&0xff)<<16 | sub&0xffff
}

// recovered is what the driver prints once the firmware works again. Only a
// fatal error runs the recovery flow; the poller says nothing when a stalled
// health counter moves again, nor the command interface after a timeout.
func (p mlx5Printer) recovered(fault string) []string {
	if fault != ibconfig.HealthHWFatal {
		return nil
	}
	return []string{
		p.core(levelWarn, "mlx5_health_try_recover", 327, pidWorker, "handling bad device here"),
		p.core(levelErr, "mlx5_health_try_recover", 333, pidWorker, "starting health recovery flow"),
		p.dev(levelInfo, "health recovery succeeded"),
	}
}

// moduleEvent is the port module event the firmware raises when the cable or
// transceiver of port 1 (module 0) changes. Empty is a module plugged in and
// working again.
func (p mlx5Printer) moduleEvent(fault string) string {
	switch fault {
	case "":
		return p.dev(levelInfo, "Port module event: module 0, Cable plugged")
	case ibconfig.ModuleUnplugged:
		return p.dev(levelInfo, "Port module event: module 0, Cable unplugged")
	}
	errors := map[string]string{
		ibconfig.ModuleHighTemperature: "High Temperature",
		ibconfig.ModuleBadCable:        "Bad or shorted cable/module",
	}
	return p.core(levelErr, "port_module", 253, pidTimer,
		"Port module event[error]: module 0, Cable error, "+errors[fault])
}

// carrier is mlx5e's netdev_info as the RoCE port's carrier changes:
// "mlx5_core <bdf> <netdev>: Link up".
func (p mlx5Printer) carrier(up bool) string {
	state := "down"
	if up {
		state = "up"
	}
	return fmt.Sprintf("<%d>kernel: mlx5_core %s %s: Link %s", levelInfo, p.bdf, p.netdev, state)
}
