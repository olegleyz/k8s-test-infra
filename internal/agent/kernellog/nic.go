// Copyright 2026 NVIDIA CORPORATION
// SPDX-License-Identifier: Apache-2.0

package kernellog

import (
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/NVIDIA/k8s-test-infra/internal/agent"
	ibconfig "github.com/NVIDIA/k8s-test-infra/internal/ib/config"
	"github.com/NVIDIA/k8s-test-infra/pkg/gpu/mockctl"
)

// Network use case O4, PoC: NIC faults on the kernel log.
//
// The same contract as Xids: the agent watches the nics: block of the runtime
// override document, whoever writes it, and prints what the mlx5 driver prints
// as each NIC's faults change. Unlike an Xid, a NIC fault has a recovery the
// driver reports (a cable plugged back in, a link up, a recovered firmware), so
// the simulator compares states rather than announcing codes.

// stageNICs records the node's HCAs. A NIC whose address is unchanged keeps
// what was said about it; any other starts from its profile, so a fault already
// in the document is announced on the next poll.
func (s *Simulator) stageNICs(state *agent.State) {
	if s.listNICs == nil {
		return
	}
	nics, err := s.listNICs(state)
	if err != nil {
		// The InfiniBand simulator fails its own staging on the same error and
		// says why; here it only costs the announcements.
		zap.L().Warn("cannot place the node's NICs; NIC faults will not be announced",
			zap.String("simulator", name), zap.Error(err))
		nics = nil
	}

	previous := make(map[string]string, len(s.nics))
	for _, n := range s.nics {
		previous[n.Name] = n.Address
	}
	said := make(map[string]nicView, len(nics))
	for i := range nics {
		nics[i].Address = strings.ToLower(nics[i].Address)
		n := nics[i]
		// The address goes into the kernel log, which health agents read;
		// the same boundary the GPU bus IDs pass.
		if !agent.ValidBDF(n.Address) {
			zap.L().Warn("NIC has no usable PCI address; its faults cannot be announced",
				zap.String("simulator", name), zap.String("nic", n.Name), zap.String("pci", n.Address))
			continue
		}
		v, ok := s.nicSaid[n.Name]
		if !ok || previous[n.Name] != n.Address {
			v = viewOf(n, ibconfig.NICOverride{})
		}
		said[n.Name] = v
	}
	s.nics, s.nicSaid, s.nicWarned = nics, said, map[string]nicView{}
}

// pollNICs announces every NIC whose faults changed since the last call.
func (s *Simulator) pollNICs(doc *mockctl.Doc, now time.Time) {
	for _, n := range s.nics {
		was, ok := s.nicSaid[n.Name]
		if !ok {
			continue
		}
		o := doc.NICs[n.Name]
		v := viewOf(n, o)
		if v == was {
			delete(s.nicWarned, n.Name)
			continue
		}

		fw := n.FWVersion
		if o.FWVersion != "" {
			fw = o.FWVersion
		}
		records := mlx5Records(n, fw, was, v, now)
		if len(records) == 0 {
			s.nicSaid[n.Name] = v
			continue
		}

		wrote, err := emitRecords(s.path, records)
		if wrote {
			s.nicSaid[n.Name] = v
			delete(s.nicWarned, n.Name)
			zap.L().Info("announced NIC fault on the kernel log",
				zap.String("simulator", name), zap.String("nic", n.Name),
				zap.String("pci", n.Address), zap.Int("records", len(records)),
				zap.Bool("removed", v.removed), zap.String("health", v.health),
				zap.String("module", v.module), zap.Bool("carrier", v.carrier))
			continue
		}

		// As for an Xid: retried on the next tick, warned about once.
		if w, warned := s.nicWarned[n.Name]; warned && w == v {
			continue
		}
		s.nicWarned[n.Name] = v
		zap.L().Warn("could not announce NIC fault on the kernel log",
			zap.String("simulator", name), zap.String("path", s.path),
			zap.String("nic", n.Name), zap.Error(err))
	}
}
