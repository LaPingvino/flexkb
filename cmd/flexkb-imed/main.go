// Command flexkb-imed is the Wayland input-method-v2 daemon for
// flexkb. It binds the compositor's zwp_input_method_manager_v2
// global, grabs the keyboard, and runs every keystroke through the
// runtime resolver and the IM tier in-process.
//
// Scope of this v1: phase 1 from docs/DESIGN-IME.md — Wayland v2
// backend, native engines only. Phases 2 (ibus dbus) and 3 (host
// external IMEs) land later. The single-binary plan stays: each
// new backend becomes another transport on the same engine.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/lapingvino/flexkb/internal/wlclient"
	"github.com/lapingvino/flexkb/internal/wlim"
	"github.com/lapingvino/flexkb/internal/wlwire"
)

func main() {
	verbose := flag.Bool("v", false, "verbose logging")
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	if err := run(log); err != nil {
		log.Error("daemon exited", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	conn, err := wlwire.Dial()
	if err != nil {
		return fmt.Errorf("connect to Wayland: %w", err)
	}
	defer conn.Close()
	log.Info("connected to Wayland compositor")

	d := wlclient.NewDispatcher(conn, log)
	disp, err := wlclient.ConnectDisplay(d)
	if err != nil {
		return fmt.Errorf("bootstrap wl_display: %w", err)
	}

	// Walk the registry to find the seat and the IM manager.
	// Globals arrive asynchronously; we collect them, then sync
	// to know the initial enumeration is done.
	collector := newRegistryCollector(log)
	_, err = disp.GetRegistry(collector.onGlobal, collector.onGlobalRemove)
	if err != nil {
		return fmt.Errorf("get_registry: %w", err)
	}

	// Run the dispatch loop in the background. Any protocol error
	// terminates the daemon.
	dispatchErr := make(chan error, 1)
	go func() { dispatchErr <- d.Run() }()

	// Synchronous round-trip: when the done callback fires, every
	// global advertised before our request has been delivered.
	enumerationDone := make(chan struct{})
	if err := disp.Sync(0, func(uint32) { close(enumerationDone) }); err != nil {
		return fmt.Errorf("sync: %w", err)
	}

	select {
	case <-enumerationDone:
	case err := <-dispatchErr:
		return fmt.Errorf("dispatch loop exited before sync: %w", err)
	}

	// Diagnose: did the compositor advertise what we need?
	managerName, managerVersion := collector.imManager()
	if managerName == 0 {
		return diagnoseMissingManager(collector)
	}
	seatName, seatVersion := collector.firstSeat()
	if seatName == 0 {
		return fmt.Errorf("compositor advertises no wl_seat — cannot bind an input method")
	}

	// Get the registry handle back so we can bind the globals we
	// found. (The collector's onGlobal callback captured the
	// names; we need the Registry object itself to issue Bind.)
	reg := collector.registry
	_ = reg // satisfied below when we re-architect; for now bind via the dispatcher

	// Re-acquire the registry so we have the binder. Less wasteful
	// approach: stash the Registry inside the collector at
	// GetRegistry time. We'll do that fix next, but for the
	// moment, issue another GetRegistry — Wayland tolerates
	// multiple registries per client. (See follow-up commit.)
	binder, err := disp.GetRegistry(nil, nil)
	if err != nil {
		return fmt.Errorf("re-acquire registry for binding: %w", err)
	}

	seat, err := wlclient.BindSeat(binder, seatName, seatVersion, d)
	if err != nil {
		return fmt.Errorf("bind wl_seat: %w", err)
	}
	log.Info("bound wl_seat", "version", seatVersion)

	mgrVersion := managerVersion
	if mgrVersion > wlim.ManagerVersion {
		mgrVersion = wlim.ManagerVersion
	}
	mgr, err := wlim.BindManager(binder, managerName, mgrVersion, d)
	if err != nil {
		return fmt.Errorf("bind input-method-manager: %w", err)
	}
	log.Info("bound input-method-manager", "version", mgrVersion)

	// Build the IM handle with logging callbacks; the real engine
	// wiring lands in the next commit once the daemon has a
	// session-state struct to hang the resolver and IM engine
	// off of.
	im := wlim.NewInputMethod(log)
	im.OnActivate = func() { log.Info("activate") }
	im.OnDeactivate = func() { log.Info("deactivate") }
	im.OnSurroundingText = func(text string, cursor, anchor uint32) {
		log.Debug("surrounding_text", "text", text, "cursor", cursor, "anchor", anchor)
	}
	im.OnContentType = func(hint, purpose uint32) {
		log.Debug("content_type", "hint", hint, "purpose", purpose)
	}
	im.OnDone = func() { log.Debug("done") }
	im.OnUnavailable = func() {
		log.Error("compositor refused IM grab — another input method already has the seat")
	}
	if _, err := mgr.GetInputMethod(seat.ID(), im); err != nil {
		return fmt.Errorf("get_input_method: %w", err)
	}
	log.Info("ready: awaiting text-input focus")

	// Wait for either dispatch error or a signal.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-dispatchErr:
		return err
	case sig := <-sigs:
		log.Info("signal received", "sig", sig)
		return nil
	}
}

// diagnoseMissingManager produces a helpful error when the
// compositor doesn't expose input-method-v2 — the dominant
// "GNOME on Wayland" case. We list what we did see so the user
// can confirm we're talking to the right session.
func diagnoseMissingManager(c *registryCollector) error {
	socketPath, _ := wlwire.SocketPath()
	c.mu.Lock()
	advertised := make([]string, 0, len(c.globals))
	for _, g := range c.globals {
		advertised = append(advertised, fmt.Sprintf("%s v%d", g.iface, g.version))
	}
	c.mu.Unlock()
	return fmt.Errorf(
		"compositor at %s does not advertise zwp_input_method_manager_v2.\n"+
			"  Saw %d globals; this protocol comes from wlroots-protocols and is supported by KDE Plasma,\n"+
			"  Sway, Hyprland, river, labwc, and other wlroots-based compositors.\n"+
			"  Mutter (GNOME) and Mir do not implement it. ibus-backend support is on the roadmap\n"+
			"  (docs/DESIGN-IME.md phase 2) for those desktops.",
		socketPath, len(advertised))
}

// registryCollector accumulates wl_registry global events so the
// main loop can answer "what does the compositor expose?" after
// the initial sync completes.
type registryCollector struct {
	log *slog.Logger

	mu       sync.Mutex
	globals  map[uint32]registryGlobal
	registry *wlclient.Registry // re-binding workaround
}

type registryGlobal struct {
	iface   string
	version uint32
}

func newRegistryCollector(log *slog.Logger) *registryCollector {
	return &registryCollector{log: log, globals: map[uint32]registryGlobal{}}
}

func (c *registryCollector) onGlobal(name uint32, iface string, version uint32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.globals[name] = registryGlobal{iface: iface, version: version}
	c.log.Debug("global", "name", name, "iface", iface, "version", version)
}

func (c *registryCollector) onGlobalRemove(name uint32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.globals, name)
}

func (c *registryCollector) imManager() (uint32, uint32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for name, g := range c.globals {
		if g.iface == wlim.InterfaceManager {
			return name, g.version
		}
	}
	return 0, 0
}

func (c *registryCollector) firstSeat() (uint32, uint32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for name, g := range c.globals {
		if g.iface == wlclient.InterfaceSeat {
			return name, g.version
		}
	}
	return 0, 0
}
