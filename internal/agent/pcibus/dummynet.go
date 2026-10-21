// Copyright 2026 NVIDIA CORPORATION
// SPDX-License-Identifier: Apache-2.0

package pcibus

import (
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"go.uber.org/zap"

	"github.com/NVIDIA/k8s-test-infra/internal/agent/host"
)

// Network use case D3 for RoCE, PoC.
//
// A consumer that finds a netdev under a NIC's PCI function asks the kernel for
// it by name over netlink (the RDMA shared device plugin's LinkByName). Mokka's
// RoCE netdevs exist only in its sysfs tree, so the lookup fails. The node
// agent gives each one a real dummy link of the same name in the node's network
// namespace. A dummy has no data path: everything sent on it is dropped.
//
// The links carry dummyAlias, so the agent only ever touches its own: a link of
// that name without it is genuine and left alone.

const dummyAlias = "nvml-mock: simulated RoCE port"

// syncDummies makes the node's dummy links match the simulated RoCE netdevs:
// creates missing ones, sets each up or down as the simulated port is, and
// deletes the agent's own links that no simulated netdev names any more.
func syncDummies(h *host.Host, up map[string]bool) error {
	ns, err := netns.GetFromPath(h.ProcPath("1", "ns", "net"))
	if err != nil {
		return fmt.Errorf("node network namespace: %w", err)
	}
	defer ns.Close()
	nl, err := netlink.NewHandleAt(ns)
	if err != nil {
		return fmt.Errorf("netlink in the node namespace: %w", err)
	}
	defer nl.Close()

	var errs []error
	for name, wantUp := range up {
		link, err := nl.LinkByName(name)
		if err == nil && !ours(link) {
			zap.L().Warn("PoC: a genuine link has the simulated netdev's name; leaving it alone",
				zap.String("link", name), zap.String("type", link.Type()))
			continue
		}
		if err != nil {
			attrs := netlink.NewLinkAttrs()
			attrs.Name = name
			if err := nl.LinkAdd(&netlink.Dummy{LinkAttrs: attrs}); err != nil {
				errs = append(errs, fmt.Errorf("add dummy %s: %w", name, err))
				continue
			}
			if link, err = nl.LinkByName(name); err != nil {
				errs = append(errs, err)
				continue
			}
			if err := nl.LinkSetAlias(link, dummyAlias); err != nil {
				errs = append(errs, fmt.Errorf("alias %s: %w", name, err))
			}
			zap.L().Info("PoC: created dummy link for a simulated RoCE netdev", zap.String("link", name))
		}
		isUp := link.Attrs().Flags&net.FlagUp != 0
		switch {
		case wantUp && !isUp:
			errs = append(errs, nl.LinkSetUp(link))
		case !wantUp && isUp:
			errs = append(errs, nl.LinkSetDown(link))
		}
	}

	links, err := nl.LinkList()
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, link := range links {
		if _, want := up[link.Attrs().Name]; want || !ours(link) {
			continue
		}
		zap.L().Info("PoC: deleting dummy link no simulated netdev names", zap.String("link", link.Attrs().Name))
		errs = append(errs, nl.LinkDel(link))
	}
	return errors.Join(errs...)
}

func ours(link netlink.Link) bool {
	return link.Type() == "dummy" && strings.HasPrefix(link.Attrs().Alias, dummyAlias)
}
