// Copyright 2026 NVIDIA CORPORATION
// SPDX-License-Identifier: Apache-2.0

package kernellog

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/k8s-test-infra/internal/agent"
	"github.com/NVIDIA/k8s-test-infra/internal/agent/host"
	ibconfig "github.com/NVIDIA/k8s-test-infra/internal/ib/config"
	"github.com/NVIDIA/k8s-test-infra/internal/ib/sysfs"
	"github.com/NVIDIA/k8s-test-infra/pkg/gpu/mockctl"
)

// NVSentinel v1.28.0's NIC driver patterns
// (health-monitors/syslog-health-monitor/pkg/nicdriver/config.go), the
// single-line ones, by the error code its events carry.
var nvsentinelPatterns = map[string]*regexp.Regexp{
	"cmd_exec_timeout":         regexp.MustCompile(`mlx5_core.*timeout\. Will cause a leak of a command resource`),
	"health_poll_failed":       regexp.MustCompile(`mlx5_core.*device's health compromised.*reached miss count`),
	"unrecoverable_err":        regexp.MustCompile(`mlx5_core.*unrecoverable hardware error`),
	"netdev_watchdog":          regexp.MustCompile(`(NETDEV WATCHDOG.*mlx5_core|mlx5_core.*NETDEV WATCHDOG).*transmit queue.*timed out`),
	"mlx5_tx_timeout_detected": regexp.MustCompile(`mlx5_core.*TX timeout detected`),
	"mlx5_rx_timeout_detected": regexp.MustCompile(`mlx5_core.*RX timeout on channel`),
	"pci_power_insufficient":   regexp.MustCompile(`mlx5_core.*Detected insufficient power on the PCIe slot`),
	"port_module_high_temp":    regexp.MustCompile(`mlx5_core.*Port module event.*High Temperature`),
	"access_reg_failed":        regexp.MustCompile(`mlx5_cmd_out_err.*ACCESS_REG.*failed`),
	"module_unplugged":         regexp.MustCompile(`mlx5_core.*Port module event.*Cable unplugged`),
}

// matched returns the NVSentinel error code each record raises, first match
// wins as in its handler, as the journal hands it over: without the level and
// with the "kernel: " identifier stripped.
func matched(records []string) []string {
	var codes []string
	for _, r := range records {
		_, msg, _ := strings.Cut(r, "kernel: ")
		for _, code := range []string{
			"cmd_exec_timeout", "health_poll_failed", "unrecoverable_err", "netdev_watchdog",
			"mlx5_tx_timeout_detected", "mlx5_rx_timeout_detected", "pci_power_insufficient",
			"port_module_high_temp", "access_reg_failed", "module_unplugged",
		} {
			if nvsentinelPatterns[code].MatchString(msg) {
				codes = append(codes, code)
				break
			}
		}
	}
	return codes
}

var (
	ibNIC   = sysfs.HCAPlacement{Name: "mlx5_0", Address: "0000:0C:00.0", LinkLayer: "InfiniBand", FWVersion: "28.39.2048", PortState: "ACTIVE", PhysState: "LinkUp"}
	roceNIC = sysfs.HCAPlacement{Name: "mlx5_3", Address: "0000:9a:00.0", LinkLayer: "Ethernet", Netdev: "enp154s0np0", FWVersion: "28.39.2048", PortState: "ACTIVE", PhysState: "LinkUp"}
)

func records(n sysfs.HCAPlacement, from, to ibconfig.NICOverride) []string {
	return mlx5Records(n, n.FWVersion, viewOf(n, from), viewOf(n, to), testTime)
}

func port(p ibconfig.PortOverride) ibconfig.NICOverride {
	return ibconfig.NICOverride{Ports: map[string]ibconfig.PortOverride{"1": p}}
}

var testTime = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)

// Each fault prints what the driver prints, and NVSentinel raises from it
// exactly what it would raise from the real driver: the fatal codes for a
// firmware failure, a non-fatal one for a module event, nothing for a link that
// goes down or a NIC that leaves the bus (its NIC monitor's job, not the log's).
func TestMLX5Records_MatchWhatNVSentinelMatchesOnHardware(t *testing.T) {
	t.Parallel()

	none := ibconfig.NICOverride{}
	cases := []struct {
		name     string
		nic      sysfs.HCAPlacement
		from, to ibconfig.NICOverride
		want     []string // NVSentinel error codes, in order
		records  int
	}{
		{"cable pulled", ibNIC, none, port(ibconfig.PortOverride{Module: ibconfig.ModuleUnplugged}), []string{"module_unplugged"}, 1},
		{"cable back", ibNIC, port(ibconfig.PortOverride{Module: ibconfig.ModuleUnplugged}), none, nil, 1},
		{"module too hot", ibNIC, none, port(ibconfig.PortOverride{Module: ibconfig.ModuleHighTemperature}), []string{"port_module_high_temp"}, 1},
		{"IB port down says nothing", ibNIC, none, port(ibconfig.PortOverride{State: "DOWN", PhysState: "Disabled"}), nil, 0},
		{"RoCE port down", roceNIC, none, port(ibconfig.PortOverride{State: "DOWN"}), nil, 1},
		{"RoCE cable pulled", roceNIC, none, port(ibconfig.PortOverride{Module: ibconfig.ModuleUnplugged}), []string{"module_unplugged"}, 2},
		{"off the bus", ibNIC, none, ibconfig.NICOverride{Removed: true}, nil, 4},
		{"back on the bus", ibNIC, ibconfig.NICOverride{Removed: true}, none, nil, 3},
		{"unrecoverable", ibNIC, none, ibconfig.NICOverride{Health: ibconfig.HealthHWFatal}, []string{"unrecoverable_err", "unrecoverable_err"}, 19},
		{"recovered", ibNIC, ibconfig.NICOverride{Health: ibconfig.HealthHWFatal}, none, nil, 3},
		{"heartbeat", ibNIC, none, ibconfig.NICOverride{Health: ibconfig.HealthHeartbeatLost}, []string{"health_poll_failed"}, 1},
		{"command timeout", ibNIC, none, ibconfig.NICOverride{Health: ibconfig.HealthCmdTimeout}, []string{"cmd_exec_timeout"}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := records(tc.nic, tc.from, tc.to)
			require.Len(t, got, tc.records, strings.Join(got, "\n"))
			require.Equal(t, tc.want, matched(got))
			for _, r := range got {
				// Lower-case hex, the way the kernel prints a device name, and
				// the address NVSentinel resolves through /sys/bus/pci.
				require.Contains(t, r, "mlx5_core "+strings.ToLower(tc.nic.Address))
				require.Regexp(t, `^<[0-7]>kernel: mlx5_core `, r)
			}
		})
	}
}

func TestMLX5Records_ExamplesReadLikeTheDriver(t *testing.T) {
	t.Parallel()

	require.Equal(t, []string{"<6>kernel: mlx5_core 0000:0c:00.0: Port module event: module 0, Cable unplugged"},
		records(ibNIC, ibconfig.NICOverride{}, port(ibconfig.PortOverride{Module: ibconfig.ModuleUnplugged})))
	require.Equal(t, []string{
		"<6>kernel: mlx5_core 0000:9a:00.0: Port module event: module 0, Cable unplugged",
		"<6>kernel: mlx5_core 0000:9a:00.0 enp154s0np0: Link down",
	}, records(roceNIC, ibconfig.NICOverride{}, port(ibconfig.PortOverride{Module: ibconfig.ModuleUnplugged})))
	require.Equal(t, "<3>kernel: mlx5_core 0000:0c:00.0: poll_health:762:(pid 0): Fatal error 1 detected",
		records(ibNIC, ibconfig.NICOverride{}, ibconfig.NICOverride{Removed: true})[0])
}

// A NIC that starts uncabled in its profile was never up, so the kernel log has
// nothing to say about it until its state changes.
func TestMLX5Records_ProfileStateIsNoChange(t *testing.T) {
	t.Parallel()

	uncabled := roceNIC
	uncabled.PortState, uncabled.PhysState = "DOWN", "Disabled"
	require.Empty(t, records(uncabled, ibconfig.NICOverride{}, ibconfig.NICOverride{}))
	require.Equal(t, []string{"<6>kernel: mlx5_core 0000:9a:00.0 enp154s0np0: Link up"},
		records(uncabled, ibconfig.NICOverride{}, port(ibconfig.PortOverride{State: "ACTIVE"})))
}

// The simulator announces a NIC fault once, from the document nvml-mock-ctl
// writes, and its recovery once when it is cleared.
func TestSimulator_AnnouncesNICFaultsAndRecovery(t *testing.T) {
	t.Parallel()

	h, state, kmsg := node(t)
	s := New(h, Options{Path: kmsg, NICs: func(*agent.State) ([]sysfs.HCAPlacement, error) {
		return []sysfs.HCAPlacement{ibNIC, roceNIC}, nil
	}})
	require.NoError(t, s.Stage(t.Context(), state))

	s.Poll(t.Context())
	require.Empty(t, kernelLog(t, kmsg), "a healthy node prints nothing")

	mutate(t, h, func(d *mockctl.Doc) error { return d.SetNICModule("mlx5_0", "1", ibconfig.ModuleUnplugged) })
	s.Poll(t.Context())
	s.Poll(t.Context())
	require.Equal(t, "<6>kernel: mlx5_core 0000:0c:00.0: Port module event: module 0, Cable unplugged\n", kernelLog(t, kmsg))

	mutate(t, h, func(d *mockctl.Doc) error { return d.SetNICModule("mlx5_0", "1", "") })
	s.Poll(t.Context())
	require.Contains(t, kernelLog(t, kmsg), "Cable plugged")
	require.Equal(t, 2, strings.Count(kernelLog(t, kmsg), "\n"))
}

func mutate(t *testing.T, h *host.Host, change func(*mockctl.Doc) error) {
	t.Helper()

	path := h.RootPath("driver/config/overrides.yaml")
	doc, err := mockctl.Load(path)
	require.NoError(t, err)
	require.NoError(t, change(doc))
	require.NoError(t, mockctl.WriteAtomic(path, doc))
}
