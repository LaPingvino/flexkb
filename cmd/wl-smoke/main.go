// Command wl-smoke connects to the live Wayland compositor, lists
// the advertised globals, and exits. Useful during early daemon
// development to confirm the wire/dispatch layers actually talk
// to a real compositor; not shipped in any release build.
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/lapingvino/flexkb/internal/wlclient"
	"github.com/lapingvino/flexkb/internal/wlwire"
)

func main() {
	conn, err := wlwire.Dial()
	if err != nil {
		fmt.Fprintln(os.Stderr, "dial:", err)
		os.Exit(1)
	}
	defer conn.Close()
	d := wlclient.NewDispatcher(conn, nil)
	disp, err := wlclient.ConnectDisplay(d)
	if err != nil {
		fmt.Fprintln(os.Stderr, "display:", err)
		os.Exit(1)
	}
	go d.Run()
	done := make(chan struct{})
	_, err = disp.GetRegistry(
		func(name uint32, iface string, version uint32) {
			fmt.Printf("global %3d  %s  v%d\n", name, iface, version)
		},
		nil,
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, "get_registry:", err)
		os.Exit(1)
	}
	disp.Sync(0, func(uint32) { close(done) })
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		fmt.Fprintln(os.Stderr, "timeout waiting for sync")
	}
}
