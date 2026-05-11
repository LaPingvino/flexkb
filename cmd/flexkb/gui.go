// flexkb gui — open the live preview in a dedicated window.
//
// Three rendering engines are supported, gated by build tags so default
// `go build` stays CGO-free and has no extra system deps:
//
//   - "webview" (//go:build webview): native GTK/webkit2gtk window via
//     github.com/webview/webview_go. Needs CGO + webkit2gtk-4.1 at
//     runtime. Best "settings app" feel on Linux desktops.
//   - "lorca"   (//go:build lorca):  open the URL in a Chromium-based
//     browser launched in --app= mode (chromeless window). No CGO; needs
//     `google-chrome`, `chromium`, or `microsoft-edge` on $PATH.
//   - "browser" (always available): xdg-open the URL in the user's
//     default browser. Fallback when neither of the above is built in.
//
// Each engine registers itself into `guiEngines` via init(). The auto
// resolver prefers webview > lorca > browser. Override with --engine=.
package main

import (
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
)

// guiEngine renders the running flexkb server in some window. It blocks
// until the window is closed (or, for the browser fallback, until the
// process receives SIGINT/SIGTERM). On clean exit it returns nil.
type guiEngine func(url string) error

var guiEngines = map[string]guiEngine{}

func registerEngine(name string, fn guiEngine) { guiEngines[name] = fn }

func init() { registerEngine("browser", browserEngine) }

// browserEngine hands the URL to xdg-open and then blocks until the
// process is signalled — there's no window handle to wait on, so the
// user kills the process when done (Ctrl+C in a terminal, or the
// session's process manager).
func browserEngine(url string) error {
	openInBrowser(url)
	fmt.Println("opened in default browser — Ctrl+C to stop server")
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	<-sigs
	return nil
}

func runGui(args []string) {
	paths, rest := dataPaths(args)
	addr := "localhost:7878"
	requested := "auto"
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		switch {
		case a == "--addr" && i+1 < len(rest):
			addr = rest[i+1]
			i++
		case strings.HasPrefix(a, "--addr="):
			addr = strings.TrimPrefix(a, "--addr=")
		case a == "--engine" && i+1 < len(rest):
			requested = rest[i+1]
			i++
		case strings.HasPrefix(a, "--engine="):
			requested = strings.TrimPrefix(a, "--engine=")
		case a == "--list-engines":
			listEnginesAndExit()
		}
	}
	root := makeRoot(paths)

	url, errCh, err := startServer(root, addr)
	check(err)

	chosen := pickEngine(requested)
	fmt.Printf("flexkb gui: %s  (engine: %s)\n", url, chosen)
	fn := guiEngines[chosen]

	guiErr := make(chan error, 1)
	go func() { guiErr <- fn(url) }()

	select {
	case e := <-guiErr:
		if e != nil {
			fmt.Fprintln(os.Stderr, "gui engine error:", e)
			os.Exit(1)
		}
	case e := <-errCh:
		if e != nil {
			fmt.Fprintln(os.Stderr, "server error:", e)
			os.Exit(1)
		}
	}
}

// pickEngine resolves the user's --engine= choice. "auto" prefers webview
// > lorca > browser. A specific request falls back to auto with a warning
// if that engine wasn't compiled in.
func pickEngine(req string) string {
	if req != "" && req != "auto" {
		if _, ok := guiEngines[req]; ok {
			return req
		}
		fmt.Fprintf(os.Stderr, "warn: engine %q not built into this binary; falling back to auto\n", req)
	}
	for _, name := range []string{"webview", "lorca", "browser"} {
		if _, ok := guiEngines[name]; ok {
			return name
		}
	}
	return "browser"
}

func listEnginesAndExit() {
	names := make([]string, 0, len(guiEngines))
	for n := range guiEngines {
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Println("engines compiled into this binary:")
	for _, n := range names {
		fmt.Println("  ", n)
	}
	os.Exit(0)
}
