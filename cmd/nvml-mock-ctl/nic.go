// Copyright 2026 NVIDIA CORPORATION
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/NVIDIA/k8s-test-infra/pkg/gpu/mockctl"
)

// The NIC commands inject faults into one simulated HCA. They write the nics
// block of the same runtime override document the GPU commands write, and the
// node agent renders it into the HCA's sysfs files within a second. Each
// fault reverts on its own (restore, --clear, follow, profile) or with reset.

func nicCommand() *cli.Command {
	return &cli.Command{
		Name:  "nic",
		Usage: "inject or clear a fault on one simulated NIC (an mlx5 HCA)",
		Commands: []*cli.Command{
			{
				Name:  "state",
				Usage: "set a port's state and physical state (e.g. --state DOWN --phys-state Disabled)",
				Flags: []cli.Flag{nicFlag(), portFlag(),
					&cli.StringFlag{Name: "state", Usage: "DOWN | INIT | ARMED | ACTIVE | ACTIVE_DEFER"},
					&cli.StringFlag{Name: "phys-state", Usage: "Disabled | Polling | Training | LinkUp | LinkErrorRecovery | ..."},
					&cli.BoolFlag{Name: "clear", Usage: "return the port to the profile's state"},
				},
				Action: func(_ context.Context, cmd *cli.Command) error {
					state, phys := cmd.String("state"), cmd.String("phys-state")
					if !cmd.Bool("clear") && state == "" && phys == "" {
						return errors.New("give --state, --phys-state or --clear")
					}
					return mutateNIC(cmd, func(doc *mockctl.Doc, nic string) error {
						return doc.SetNICPortState(nic, portOf(cmd), state, phys)
					})
				},
			},
			{
				Name:  "remove",
				Usage: "take the NIC off the node, as if it fell off the PCIe bus",
				Flags: []cli.Flag{nicFlag()},
				Action: func(_ context.Context, cmd *cli.Command) error {
					return mutateNIC(cmd, func(doc *mockctl.Doc, nic string) error {
						doc.SetNICRemoved(nic, true)
						return nil
					})
				},
			},
			{
				Name:  "restore",
				Usage: "put a removed NIC back",
				Flags: []cli.Flag{nicFlag()},
				Action: func(_ context.Context, cmd *cli.Command) error {
					return mutateNIC(cmd, func(doc *mockctl.Doc, nic string) error {
						doc.SetNICRemoved(nic, false)
						return nil
					})
				},
			},
			{
				Name:  "char-devices",
				Usage: "remove the NIC's umad, issm and uverbs entries (--restore puts them back)",
				Flags: []cli.Flag{nicFlag(), &cli.BoolFlag{Name: "restore"}},
				Action: func(_ context.Context, cmd *cli.Command) error {
					return mutateNIC(cmd, func(doc *mockctl.Doc, nic string) error {
						doc.SetNICCharDevicesRemoved(nic, !cmd.Bool("restore"))
						return nil
					})
				},
			},
			{
				Name: "counter",
				Usage: "set, add to, grow or reset a counter: counters/<name>, hw_counters/<name>,\n" +
					"   statistics/<name> or carrier_changes",
				Flags: []cli.Flag{nicFlag(), portFlag(),
					&cli.Uint64Flag{Name: "set", Usage: "set the counter to N"},
					&cli.Uint64Flag{Name: "add", Usage: "add N once"},
					&cli.StringFlag{Name: "rate", Usage: "grow at N per second (N, N/s, N/m, N/h); 0 stops"},
					&cli.BoolFlag{Name: "reset", Usage: "reset to 0, as a counter clear does"},
					&cli.BoolFlag{Name: "clear", Usage: "drop the override: back to the profile's value"},
				},
				Arguments: []cli.Argument{requiredStringArgs("counter")},
				Action: func(_ context.Context, cmd *cli.Command) error {
					change, err := counterChange(cmd)
					if err != nil {
						return err
					}
					paths := cmd.StringArgs("counter")
					return mutateNIC(cmd, func(doc *mockctl.Doc, nic string) error {
						now := time.Now().UTC()
						for _, p := range paths {
							if err := doc.ChangeNICCounter(nic, portOf(cmd), p, change, now); err != nil {
								return err
							}
						}
						return nil
					})
				},
			},
			{
				Name:      "operstate",
				Usage:     "set a RoCE netdev's operstate (up, down, ...); 'follow' tracks the port again",
				Flags:     []cli.Flag{nicFlag()},
				Arguments: []cli.Argument{requiredStringArgs("state")},
				Action: func(_ context.Context, cmd *cli.Command) error {
					state := cmd.StringArgs("state")[0]
					if state == "follow" {
						state = ""
					}
					return mutateNIC(cmd, func(doc *mockctl.Doc, nic string) error {
						return doc.SetNICOperstate(nic, state)
					})
				},
			},
			{
				Name:      "firmware",
				Usage:     "replace the NIC's fw_ver, as a firmware update does; 'profile' restores it",
				Flags:     []cli.Flag{nicFlag()},
				Arguments: []cli.Argument{requiredStringArgs("version")},
				Action: func(_ context.Context, cmd *cli.Command) error {
					version := cmd.StringArgs("version")[0]
					if version == "profile" {
						version = ""
					}
					return mutateNIC(cmd, func(doc *mockctl.Doc, nic string) error {
						doc.SetNICFirmware(nic, version)
						return nil
					})
				},
			},
			{
				Name:  "reset",
				Usage: "clear every fault on the NIC, or on every NIC without --nic",
				Flags: []cli.Flag{&cli.StringFlag{Name: "nic", Usage: "RDMA device name, e.g. mlx5_0"}},
				Action: func(_ context.Context, cmd *cli.Command) error {
					return mutateNIC(cmd, func(doc *mockctl.Doc, nic string) error {
						doc.ResetNIC(nic)
						return nil
					})
				},
			},
		},
	}
}

func nicFlag() cli.Flag {
	return &cli.StringFlag{Name: "nic", Usage: "RDMA device name, e.g. mlx5_0", Required: true}
}

func portFlag() cli.Flag {
	return &cli.IntFlag{Name: "port", Usage: "port number", Value: 1}
}

func portOf(cmd *cli.Command) string { return strconv.Itoa(int(cmd.Int("port"))) }

// mutateNIC is mutate for the NIC commands: the same lock, load and atomic
// write, keyed by NIC rather than GPU.
func mutateNIC(cmd *cli.Command, apply func(doc *mockctl.Doc, nic string) error) error {
	nic := cmd.String("nic")
	if nic != "" {
		if err := mockctl.ValidateNIC(nic); err != nil {
			return err
		}
	}
	path := configOverridePath(cmd)

	unlock, err := mockctl.LockOverride(path)
	if err != nil {
		return failf("lock: %v", err)
	}
	defer unlock()

	doc, err := mockctl.Load(path)
	if err != nil {
		return failf("load: %v", err)
	}
	if err := apply(doc, nic); err != nil {
		return err
	}
	if err := mockctl.WriteAtomic(path, doc); err != nil {
		return failf("write: %v", err)
	}

	if nic == "" {
		nic = "every NIC"
	}
	fprintf(cmd.Root().Writer, "ok: nic %s applied to %s\n", cmd.Name, nic)
	return nil
}

// counterChange reads the one counter operation the flags ask for.
func counterChange(cmd *cli.Command) (mockctl.CounterChange, error) {
	var c mockctl.CounterChange
	n := 0
	if cmd.IsSet("set") {
		v := cmd.Uint64("set")
		c.Set, n = &v, n+1
	}
	if cmd.IsSet("add") {
		v := cmd.Uint64("add")
		c.Add, n = &v, n+1
	}
	if cmd.IsSet("rate") {
		r, err := parseRate(cmd.String("rate"))
		if err != nil {
			return c, err
		}
		c.Rate, n = &r, n+1
	}
	if cmd.Bool("reset") {
		c.Reset, n = true, n+1
	}
	if cmd.Bool("clear") {
		c.Clear, n = true, n+1
	}
	if n != 1 {
		return c, errors.New("give exactly one of --set, --add, --rate, --reset or --clear")
	}
	return c, nil
}

// parseRate reads N, N/s, N/m or N/h as a per-second rate.
func parseRate(s string) (float64, error) {
	num, unit, _ := strings.Cut(s, "/")
	v, err := strconv.ParseFloat(strings.TrimSpace(num), 64)
	if err != nil || v < 0 || v > maxNvlinkErrorRate {
		return 0, fmt.Errorf("--rate %q: want a non-negative number, optionally /s, /m or /h", s)
	}
	switch strings.TrimSpace(unit) {
	case "", "s":
		return v, nil
	case "m":
		return v / 60, nil
	case "h":
		return v / 3600, nil
	}
	return 0, fmt.Errorf("--rate %q: unit must be s, m or h", s)
}
