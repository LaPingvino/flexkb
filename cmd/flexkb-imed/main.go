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
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/lapingvino/flexkb/internal/compose"
	"github.com/lapingvino/flexkb/internal/ibus"
	"github.com/lapingvino/flexkb/internal/ibusengines"
	"github.com/lapingvino/flexkb/internal/imsession"
	"github.com/lapingvino/flexkb/internal/inputmethod"
	"github.com/lapingvino/flexkb/internal/model"
	"github.com/lapingvino/flexkb/internal/runtime"
	"github.com/lapingvino/flexkb/internal/wlclient"
	"github.com/lapingvino/flexkb/internal/wlim"
	"github.com/lapingvino/flexkb/internal/wlwire"
)

func main() {
	verbose := flag.Bool("v", false, "verbose logging")
	layoutFile := flag.String("layout", "us", "data/layouts/<name>.yaml layout file to use as the static-layer stack")
	variantName := flag.String("variant", "basic", "variant within the layout file")
	imFile := flag.String("im", "", "data/inputmethods/<name>.yaml input method to load (optional; empty = passthrough)")
	enableWayland := flag.Bool("wayland", true, "enable the Wayland input-method-v2 backend")
	ibusMode := flag.String("ibus", "off", "ibus backend: off | alongside | replace. "+
		"alongside fails if ibus-daemon already owns the name; replace takes it over.")
	listEngines := flag.Bool("list-engines", false, "print every ibus engine installed on the system and exit. "+
		"Useful to see what other IMEs flexkb-imed could host in future versions.")
	flag.Parse()

	if *listEngines {
		printEngineInventory()
		return
	}
	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	// One factory shared across both backends so each
	// freshly-created input context gets an independent Session
	// but identical configuration. ibus calls this per
	// CreateInputContext (one per app); Wayland reuses a single
	// session for the whole connection (one per compositor).
	factory := func() (*imsession.Session, error) {
		return loadSession(log, *layoutFile, *variantName, *imFile)
	}

	// Control socket: lets the GUI (and ad-hoc tooling) probe
	// the daemon's status. Non-fatal if it fails — the daemon
	// remains useful for actually processing input.
	state := newControlState(*layoutFile, *variantName, *imFile)
	ctrl, err := startControlServer(log, state)
	if err != nil {
		log.Warn("control socket disabled", "err", err)
	} else {
		defer ctrl.close()
	}

	if err := runMulti(log, *enableWayland, *ibusMode, factory, state); err != nil {
		log.Error("daemon exited", "err", err)
		os.Exit(1)
	}
}

// runMulti starts every enabled backend and waits for any of
// them to error or for a signal. Each backend is independent —
// failing to start one doesn't tear down the others; if
// EVERY backend fails to start, that's a fatal error reported
// to the caller.
//
// state is the shared control-socket state object; backends
// register themselves as they come up so the GUI sees "wayland,
// ibus" or "wayland only" etc.
func runMulti(log *slog.Logger, enableWayland bool, ibusMode string, factory func() (*imsession.Session, error), state *controlState) error {
	backendErr := make(chan error, 2)
	started := 0

	if enableWayland {
		sess, err := factory()
		if err != nil {
			log.Error("Wayland: load session", "err", err)
		} else {
			go func() { backendErr <- run(log, sess) }()
			state.addBackend("wayland-v2")
			started++
		}
	}

	if ibusMode != "off" {
		srv, err := ibus.New(log, factory)
		if err != nil {
			log.Error("ibus: connect session bus", "err", err)
		} else {
			replace := ibusMode == "replace"
			if err := srv.Start(replace); err != nil {
				log.Error("ibus: start", "err", err, "mode", ibusMode)
			} else {
				log.Info("ibus backend started", "mode", ibusMode)
				state.addBackend("ibus-" + ibusMode)
				go func() {
					// ibus runs as long as the connection lives.
					// Block here so the goroutine doesn't exit;
					// the dbus conn will surface errors via its
					// own loop when it disconnects.
					select {}
				}()
				started++
			}
		}
	}

	if started == 0 {
		return fmt.Errorf("no backends started — set --wayland=true or --ibus=alongside|replace")
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-backendErr:
		return err
	case sig := <-sigs:
		log.Info("signal received", "sig", sig)
		return nil
	}
}

// printEngineInventory dumps every installed ibus engine to
// stdout in a human-readable table. Run as `flexkb-imed
// --list-engines` to inventory what's hostable. Pure-Latin
// xkb-wrapper entries (xkb:*) are listed separately at the end
// since they're not "real" IMEs in the IM-tier sense.
func printEngineInventory() {
	components, warnings := ibusengines.Discover()
	for _, w := range warnings {
		fmt.Fprintln(os.Stderr, "warn:", w)
	}
	engines := ibusengines.AllEngines(components)
	real := ibusengines.RealEngines(engines)
	wrappers := len(engines) - len(real)

	fmt.Printf("== ibus engines installed ==\n")
	fmt.Printf("(parsing /usr/share/ibus/component/*.xml)\n\n")
	fmt.Printf("Real input methods: %d\n", len(real))
	fmt.Printf("xkb layout wrappers: %d (redundant with flexkb's static-layer)\n\n", wrappers)
	if len(real) == 0 {
		fmt.Printf("No real IM engines found. Install ibus-libpinyin, ibus-anthy,\n")
		fmt.Printf("ibus-mozc, ibus-hangul, etc. for engines flexkb-imed could host.\n")
		return
	}
	fmt.Printf("%-30s  %-30s  %s\n", "name", "long name", "language")
	fmt.Printf("%-30s  %-30s  %s\n", strings.Repeat("-", 30), strings.Repeat("-", 30), strings.Repeat("-", 8))
	for _, e := range real {
		fmt.Printf("%-30s  %-30s  %s\n", truncate(e.Name, 30), truncate(e.LongName, 30), e.Language)
	}
	fmt.Println()
	fmt.Println("Hosting these engines (routing flexkb-imed → engine subprocess) is")
	fmt.Println("phase 3 of step 5 per docs/DESIGN-IME.md. Enumeration today, full")
	fmt.Println("multiplexing in a follow-up. Until then, these engines remain")
	fmt.Println("usable via the system ibus-daemon if it's running.")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return "…"
	}
	return s[:n-1] + "…"
}

// loadSession builds the runtime stack the daemon will run for
// every keystroke: static layers (Physical→Substitutions baked
// into a ComposedLayout via internal/compose) plus an optional
// IM file. Picks data root from the standard flexkb search path.
func loadSession(log *slog.Logger, layoutFile, variantName, imPath string) (*imsession.Session, error) {
	root := model.DataRoot{Paths: defaultDataPaths()}
	lf, err := root.LayoutFile(layoutFile)
	if err != nil {
		return nil, fmt.Errorf("load layout %s: %w", layoutFile, err)
	}
	var spec model.LayoutSpec
	for _, v := range lf.Variants {
		if v.Name == variantName {
			spec = v
			break
		}
	}
	if spec.Name == "" {
		return nil, fmt.Errorf("variant %q not found in %s.yaml", variantName, layoutFile)
	}
	res, err := compose.Compose(root, spec)
	if err != nil {
		return nil, fmt.Errorf("compose %s(%s): %w", layoutFile, variantName, err)
	}
	for _, w := range res.Warnings {
		log.Debug("compose warning", "msg", w)
	}
	resolver := runtime.NewResolver(res.Layout)
	log.Info("loaded static stack", "layout", layoutFile, "variant", variantName,
		"keys", len(res.Layout.Symbols))

	var im *inputmethod.InputMethod
	if imPath != "" {
		// Two forms accepted: bare name → looks up
		// data/inputmethods/<name>.yaml in the search path;
		// absolute or contains-slash → loaded directly.
		if isPath(imPath) {
			im, err = inputmethod.Load(imPath)
		} else {
			// Use the data-root loader, mirroring how
			// substitutions/additions are loaded.
			path, ferr := root.Find("inputmethods", imPath)
			if ferr != nil {
				return nil, fmt.Errorf("find input method %s: %w", imPath, ferr)
			}
			im, err = inputmethod.Load(path)
		}
		if err != nil {
			return nil, fmt.Errorf("load input method %s: %w", imPath, err)
		}
		log.Info("loaded input method", "name", im.Name, "states", len(im.States))
	}
	return imsession.New(resolver, im)
}

// defaultDataPaths mirrors model.DataRoot's standard XDG-aware
// search path. The daemon doesn't go through cmd/flexkb's
// makeRoot helper because it's a separate binary.
func defaultDataPaths() []string {
	var paths []string
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".config", "flexkb", "data"))
	}
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		paths = append(paths, filepath.Join(xdg, "flexkb", "data"))
	}
	if wd, err := os.Getwd(); err == nil {
		paths = append(paths, filepath.Join(wd, "data"))
	}
	paths = append(paths, "/usr/share/flexkb/data")
	return paths
}

// isPath reports whether s looks like a filesystem path. Bare
// names like "zh-pinyin" go through the data root; "./test.yaml"
// or "/tmp/foo.yaml" are loaded directly.
func isPath(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r == '/' || r == '\\' {
			return true
		}
	}
	return s[0] == '.' || (len(s) > 1 && s[1] == ':')
}

func run(log *slog.Logger, sess *imsession.Session) error {
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

	// Grab the keyboard so we receive raw keycodes (the
	// compositor stops delivering them to focused apps until the
	// grab is released or the IM commits / leaves preedit). The
	// grab object's callbacks feed every key event through the
	// session and emit the resulting actions back via the IM.
	grab := &wlim.KeyboardGrab{}
	grab.OnKeymap = func(format uint32, fd int, size uint32) {
		log.Debug("keymap", "format", format, "size", size, "fd", fd)
		// We resolve via our own ComposedLayout; the compositor-
		// supplied keymap is informational. Mark the session
		// keymap-pending until the next done() event so we don't
		// process keystrokes against a stale layout.
		sess.MarkKeymapPending()
		// Close the fd — we don't read it. (A future version may
		// read it to sanity-check the active layout matches what
		// we composed against.)
		syscall.Close(fd)
	}
	grab.OnModifiers = func(serial, depressed, latched, locked, group uint32) {
		log.Debug("modifiers", "depressed", depressed, "latched", latched, "locked", locked, "group", group)
		sess.HandleModifiers(depressed, latched, locked)
	}
	grab.OnKey = func(serial, time, key, state uint32) {
		log.Debug("key", "scancode", key, "state", state)
		actions := sess.HandleKey(key, state)
		for _, a := range actions {
			switch act := a.(type) {
			case imsession.CommitText:
				if err := im.CommitString(act.Text); err != nil {
					log.Error("commit_string", "err", err)
				}
			case imsession.SetPreedit:
				if err := im.SetPreeditString(act.Text, act.CursorBegin, act.CursorEnd); err != nil {
					log.Error("set_preedit_string", "err", err)
				}
			case imsession.FinishCommit:
				if err := im.Commit(act.Serial); err != nil {
					log.Error("commit", "err", err)
				}
			case imsession.PassThrough:
				// Wayland input-method-v2 routes unconsumed key
				// events back to the focused client automatically
				// when we don't commit. Nothing to do here beyond
				// the log line below.
				log.Debug("passthrough", "symbol", act.Symbol)
			}
		}
	}
	grab.OnRepeatInfo = func(rate, delay int32) {
		log.Debug("repeat_info", "rate", rate, "delay", delay)
	}
	if _, err := im.GrabKeyboard(grab); err != nil {
		return fmt.Errorf("grab_keyboard: %w", err)
	}

	// done() closes the keymap-pending window. The IM emits done
	// after a batch; for keymap purposes we just clear the flag.
	prevOnDone := im.OnDone
	im.OnDone = func() {
		if prevOnDone != nil {
			prevOnDone()
		}
		sess.MarkKeymapApplied()
	}

	log.Info("ready: keyboard grabbed, awaiting text-input focus")

	// runMulti owns signal handling now — we just surface the
	// dispatch loop's outcome back to it.
	return <-dispatchErr
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
