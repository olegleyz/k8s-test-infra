// Copyright 2026 NVIDIA CORPORATION
// SPDX-License-Identifier: Apache-2.0

// Package ib implements the InfiniBand HCA simulator: a fake
// /sys/class/infiniband tree, the LD_PRELOAD shims and IB CLI tools that read
// it, and the mock-ib daemon serving UMAD and verbs over a Unix socket.
package ib

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/NVIDIA/k8s-test-infra/internal/fsutil"

	"github.com/NVIDIA/k8s-test-infra/internal/agent"
	"github.com/NVIDIA/k8s-test-infra/internal/agent/host"
	ibconfig "github.com/NVIDIA/k8s-test-infra/internal/ib/config"
	"github.com/NVIDIA/k8s-test-infra/internal/ib/daemon"
	"github.com/NVIDIA/k8s-test-infra/pkg/gpu/mockctl"
)

const (
	name = "ib"
	// socketName matches the chart's MOCK_IB_PING_SOCKET basename, which the
	// LD_PRELOAD shims in workload pods dial.
	socketName = "mock-ib.sock"
)

// Mode selects how much of the IB stack is simulated.
type Mode string

const (
	// ModeOff mocks nothing. The shims stay LD_PRELOAD'd and no-op.
	ModeOff Mode = "off"
	// ModeSysfs renders the sysfs tree only: ibstat and ibstatus work, and any
	// real host IB stays masked. No daemon, so no UMAD or verbs traffic.
	ModeSysfs Mode = "sysfs"
	// ModeFull adds the mock-ib daemon backing UMAD, verbs and ibping.
	ModeFull Mode = "full"
)

// ParseMode validates a MOCK_IB tier, case-insensitively. An unknown value is
// an error rather than a silent fallback: a typo must not disable IB quietly.
func ParseMode(s string) (Mode, error) {
	switch m := Mode(strings.ToLower(strings.TrimSpace(s))); m {
	case ModeOff, ModeSysfs, ModeFull:
		return m, nil
	case "":
		return ModeOff, nil
	default:
		return "", fmt.Errorf("invalid IB mode %q: expected off, sysfs or full", s)
	}
}

// Options configures the simulator.
type Options struct {
	Mode Mode
	// AfterRender runs after each run-time NIC change has been rendered, so
	// views built from the tree (the PCI bus simulator's /sys/class) follow.
	AfterRender func()
	// SocketPath overrides the derived socket location. Production leaves it
	// empty: the path must land on the host mount, which only Stage knows.
	SocketPath string
	TCPPort    int
	Fabric     bool
}

var (
	_ agent.Simulator = (*Simulator)(nil)
	_ agent.Daemon    = (*Simulator)(nil)
)

// Simulator fakes the InfiniBand HCAs (Host Channel Adapters) absent on
// CPU-only nodes. Consumers reach it through the LD_PRELOAD shims: sysfs reads
// via libibmocksys, verbs and UMAD via libibmockverbs and the daemon.
type Simulator struct {
	host *host.Host
	opts Options

	ready   atomic.Bool
	serving atomic.Bool

	// Captured by Stage because Run is called without a Host. Both must resolve
	// under the host mount so workloads and the agent address the same files.
	ibRootPath atomic.Pointer[string]
	socketPath atomic.Pointer[string]

	// lastStaged is the shape the tree holds; dirty says it changed since the
	// daemon was built. Stage does the comparison because it runs before Reload
	// in one reconcile — comparing in Reload would always see its own refresh.
	lastStaged atomic.Pointer[agent.NetworkShape]
	dirty      atomic.Bool

	// restart carries reload requests to the Run loop. Buffered depth 1: a
	// pending signal already covers any newer shape.
	restart chan struct{}

	// The run-time NIC faults: Stage and the runtime loop both render the tree,
	// one at a time, from the last staged state and the override document.
	renderMu  sync.Mutex
	staged    atomic.Pointer[stagedTree]
	overrides string
	lastDoc   []byte // the override document the tree was last rendered from
	growing   bool   // a counter grows on its own, so re-render every tick
	lastErr   string // deduplicates a persistent override read failure
}

// stagedTree is what the runtime loop re-renders from.
type stagedTree struct {
	state      *agent.State
	simulating bool
}

// New returns an infiniband Simulator.
func New(h *host.Host, opts Options) *Simulator {
	return &Simulator{host: h, opts: opts, restart: make(chan struct{}, 1)}
}

// Name returns the simulator's stable identifier.
func (s *Simulator) Name() string { return name }

// Ready reports whether Stage succeeded and, where a daemon is expected, it is
// serving.
func (s *Simulator) Ready() bool {
	if !s.ready.Load() {
		return false
	}
	return !s.daemonExpected() || s.serving.Load()
}

// daemonExpected reports whether this node should be running mock-ib. Full mode
// alone is not enough: the chart may select it on a profile that declares no
// InfiniBand, and readiness must not then wait on a daemon that never starts.
func (s *Simulator) daemonExpected() bool {
	if s.opts.Mode != ModeFull {
		return false
	}
	staged := s.lastStaged.Load()
	return staged != nil && staged.IBEnabled
}

// Stage converges the IB sysfs tree, tools and shims on what the profile
// declares. The empty root and the shims land on every node; the rendered HCAs
// and the tools that read them only where the tier simulates InfiniBand.
func (s *Simulator) Stage(_ context.Context, state *agent.State) error {
	s.ready.Store(false)
	zap.L().Info("staging simulator", zap.String("simulator", name))

	// The NRI plugin LD_PRELOADs the shims into every container it injects, so
	// a shim the loader cannot find is an ld.so error on every process the node
	// runs. Staging them and an empty root regardless of tier also masks any
	// real host InfiniBand.
	root := ibRoot(s.host)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	s.ibRootPath.Store(&root)

	if err := stageIBShims(s.host); err != nil {
		return err
	}

	socket := s.opts.SocketPath
	if socket == "" {
		socket = s.host.RootPath("run", socketName)
	}
	s.socketPath.Store(&socket)

	net := state.NodeShape.Network
	simulating := s.opts.Mode != ModeOff && net.IBEnabled

	zap.L().Debug("resolved ib stage decision",
		zap.String("mode", string(s.opts.Mode)), zap.Bool("ib_enabled", net.IBEnabled),
		zap.Bool("simulating", simulating))

	// A profile that retracts InfiniBand has to take its HCAs with it: the tree
	// lives on a host mount that outlives the edit, and the daemon keys its
	// lifecycle off the staged shape.
	s.overrides = s.host.RootPath("driver/config/overrides.yaml")
	if err := s.render(state, simulating, true); err != nil {
		return fmt.Errorf("render ib sysfs: %w", err)
	}

	s.recordShape(net)

	if !simulating {
		if err := retractTools(s.host); err != nil {
			return err
		}

		s.ready.Store(true)
		zap.L().Info("simulator staged; not simulating", zap.String("simulator", name))

		return nil
	}

	for _, stage := range []func(*host.Host) error{
		stageIBTools, stageVerbsConfig, stageCheckFabric,
	} {
		if err := stage(s.host); err != nil {
			return err
		}
	}

	s.ready.Store(true)
	zap.L().Info("simulator staged", zap.String("simulator", name))
	return nil
}

// recordShape stores the staged shape and flags a change for Reload.
func (s *Simulator) recordShape(net agent.NetworkShape) {
	if prev := s.lastStaged.Load(); prev != nil && !prev.Equal(net) {
		s.dirty.Store(true)
	}
	s.lastStaged.Store(&net)
}

// Discard removes the rendered tree and every file Stage copied into the
// overlay. It is a no-op when Stage never completed successfully.
func (s *Simulator) Discard(_ context.Context) error {
	if !s.ready.Load() {
		return nil
	}
	zap.L().Info("discarding simulator", zap.String("simulator", name))
	return errors.Join(
		// The whole ib/ subtree: infiniband is its only writer.
		removeTree(ibRoot(s.host)),
		retractTools(s.host),
		discardShims(s.host),
	)
}

// retractTools removes the binaries and the verbs config an earlier shape
// staged. The shims are left in place; the NRI plugin preloads them on every
// tier, so they have to resolve even where nothing is simulated.
func retractTools(h *host.Host) error {
	return errors.Join(
		discardTools(h),
		removeTree(h.RootPath("driver/etc/libibverbs.d")),
	)
}

// discardTools removes the binaries and libraries Stage copied in. Named files
// rather than directories: driver/usr/bin and driver/usr/lib64 are shared with
// gpudriver, whose nvidia-smi and libnvidia-ml must survive.
func discardTools(h *host.Host) error {
	errs := make([]error, 0, 3+len(fallbackTools))
	errs = append(errs,
		removeStaged(h, filepath.Join(toolBundleRoot, "bin"), "driver/usr/bin"),
		removeStaged(h, filepath.Join(toolBundleRoot, "lib64"), "driver/usr/lib64"),
		fsutil.Remove(h.RootPath("driver/usr/bin/check-fabric")),
	)
	for _, tool := range fallbackTools {
		errs = append(errs, fsutil.Remove(h.RootPath("driver/usr/bin", tool)))
	}
	return errors.Join(errs...)
}

func discardShims(h *host.Host) error {
	matches, _ := filepath.Glob(h.RootPath("driver/usr/local/lib/libibmock*.so*"))
	errs := make([]error, 0, len(matches))
	for _, p := range matches {
		errs = append(errs, fsutil.Remove(p))
	}
	return errors.Join(errs...)
}

func removeTree(path string) error {
	if err := os.RemoveAll(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

// removeStaged deletes from dstRel every file that srcDir contributed.
func removeStaged(h *host.Host, srcDir, dstRel string) error {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return nil // nothing was staged from here
	}
	var errs []error
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if err := fsutil.Remove(h.RootPath(dstRel, e.Name())); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// serverRestartBackoff throttles the Run loop when the daemon cannot be built,
// so a malformed tree cannot spin it hot.
const serverRestartBackoff = 2 * time.Second

// Run serves UMAD and verbs until ctx is cancelled, rebuilding the daemon
// whenever Reload reports a changed IB shape, and renders run-time NIC faults
// as they arrive. The agent calls this on the Stage barrier, so the tree it
// scans already exists.
func (s *Simulator) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	// Waited for before returning, so no fault is rendered after Discard.
	defer wg.Wait()
	wg.Go(func() { s.watchRuntime(ctx) })

	if s.opts.Mode != ModeFull {
		<-ctx.Done()
		return nil
	}

	for ctx.Err() == nil {
		// Park rather than return when the profile declares no IB: Run is
		// launched once, so returning would strand a later edit that turns it on.
		if !s.daemonExpected() {
			zap.L().Debug("mock-ib daemon parked; profile declares no InfiniBand", zap.String("simulator", name))
			s.awaitRestart(ctx)
			continue
		}
		if err := s.serveOnce(ctx); err != nil && ctx.Err() == nil {
			zap.L().Error("mock-ib daemon exited", zap.String("simulator", name), zap.Error(err))
			select {
			case <-ctx.Done():
			case <-time.After(serverRestartBackoff):
			}
		}
	}
	return nil
}

func (s *Simulator) awaitRestart(ctx context.Context) {
	select {
	case <-ctx.Done():
	case <-s.restart:
	}
}

// serveOnce runs one daemon generation, returning when ctx is cancelled or a
// restart is requested.
func (s *Simulator) serveOnce(ctx context.Context) error {
	root, socket := s.ibRootPath.Load(), s.socketPath.Load()
	if root == nil || socket == nil {
		return errors.New("ib root not staged")
	}

	genCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Rebuilding drops the peer registry, but peers re-register on
	// registerWithPeersLoop's 2s cadence, so the fabric re-converges.
	go func() {
		select {
		case <-genCtx.Done():
		case <-s.restart:
			cancel()
		}
	}()

	srv, err := daemon.NewServer(daemon.Config{
		SocketPath: *socket,
		IBRoot:     *root,
		TCPPort:    s.opts.TCPPort,
		Fabric:     s.opts.Fabric,
	})
	if err != nil {
		return err
	}

	// Bind before advertising. Ready() gates the agent's /readyz, and a consumer
	// told the simulator is ready dials the socket at once, so flipping serving
	// around ListenAndServe would advertise a socket that does not exist yet.
	// Binding here makes that impossible: serving can only be true between a
	// successful Listen and the Close below.
	ln, err := srv.Listen()
	if err != nil {
		return err
	}
	// Deferred in this order so they unwind the other way: readiness is
	// withdrawn first, and only then does the socket go away.
	defer func() { _ = ln.Close() }()

	s.serving.Store(true)
	defer s.serving.Store(false)

	if err := srv.Serve(genCtx, ln); err != nil && genCtx.Err() == nil {
		return err
	}
	return nil
}

// Reload rebuilds the daemon only when Stage saw the IB shape change, so an
// unrelated GPU-field edit never bounces the socket.
func (s *Simulator) Reload(_ context.Context, _ *agent.State) error {
	if s.opts.Mode != ModeFull || !s.dirty.Swap(false) {
		return nil
	}
	zap.L().Info("ib shape changed; restarting daemon", zap.String("simulator", name))
	select {
	case s.restart <- struct{}{}:
	default: // a restart is already pending
	}
	return nil
}

// runtimeInterval matches the NIC health monitors' own polling, so an injected
// fault is on disk before their next read.
const runtimeInterval = time.Second

// watchRuntime renders run-time NIC faults until ctx is cancelled.
func (s *Simulator) watchRuntime(ctx context.Context) {
	ticker := time.NewTicker(runtimeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			st := s.staged.Load()
			if st == nil || !st.simulating {
				continue
			}
			if err := s.render(st.state, st.simulating, false); err != nil {
				zap.L().Warn("cannot render run-time NIC state", zap.String("simulator", name), zap.Error(err))
			}
		}
	}
}

// render brings the tree onto state plus the override document's NIC faults.
// The runtime loop (force false) skips the work when neither the document nor
// a growing counter changed anything since the last pass.
func (s *Simulator) render(state *agent.State, simulating, force bool) error {
	s.renderMu.Lock()
	raw, _ := os.ReadFile(s.overrides) // absent: no faults
	if !force && bytes.Equal(raw, s.lastDoc) && !s.growing {
		s.renderMu.Unlock()
		return nil
	}

	runtime, err := s.loadRuntime()
	if err == nil {
		err = stageSysfs(s.host, state, simulating, runtime)
	}
	if err == nil {
		s.lastDoc, s.growing = raw, growing(runtime)
		s.staged.Store(&stagedTree{state: state, simulating: simulating})
	}
	s.renderMu.Unlock()

	if err == nil && !force && s.opts.AfterRender != nil {
		s.opts.AfterRender()
	}
	return err
}

// loadRuntime reads the NIC faults. An unreadable document renders none, the
// same answer the NVML engine gives for its GPU half, and is reported once.
func (s *Simulator) loadRuntime() (map[string]ibconfig.NICOverride, error) {
	doc, err := mockctl.Load(s.overrides)
	if err != nil {
		if msg := err.Error(); msg != s.lastErr {
			s.lastErr = msg
			zap.L().Warn("cannot read runtime overrides; NIC faults not applied",
				zap.String("simulator", name), zap.String("path", s.overrides), zap.Error(err))
		}
		return nil, nil
	}
	if s.lastErr != "" || len(doc.NICs) > 0 {
		zap.L().Debug("runtime NIC overrides", zap.String("simulator", name), zap.Int("nics", len(doc.NICs)))
	}
	s.lastErr = ""
	return doc.NICs, nil
}

func growing(runtime map[string]ibconfig.NICOverride) bool {
	for _, o := range runtime {
		if o.Growing() {
			return true
		}
	}
	return false
}
