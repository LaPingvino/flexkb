package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// runDoctor prints a read-only audit of system state relevant to
// flexkb: which packages own /usr/share/X11/xkb, whether
// ibus-daemon is running, whether flexkb-imed is reachable on
// its control socket, and recovery commands if anything looks
// off. Doesn't modify anything.
//
// Used as a pre-install / post-install sanity check. Print
// output is line-oriented so it pastes cleanly into bug reports.
func runDoctor(args []string) {
	_ = args
	w := bufio.NewWriter(os.Stdout)
	defer w.Flush()

	fmt.Fprintln(w, "flexkb doctor — read-only system audit")
	fmt.Fprintln(w, strings.Repeat("=", 50))
	fmt.Fprintln(w)

	// 1. Who owns /usr/share/X11/xkb?
	fmt.Fprintln(w, "## xkb tree ownership")
	if out, err := exec.Command("pacman", "-Qo", "/usr/share/X11/xkb/symbols/us").CombinedOutput(); err == nil {
		fmt.Fprintf(w, "  %s", out)
	} else {
		fmt.Fprintln(w, "  (pacman -Qo failed; not on Arch?)")
	}
	fmt.Fprintln(w)

	// 2. Is ibus-daemon running?
	fmt.Fprintln(w, "## ibus-daemon")
	if pid := pidOf("ibus-daemon"); pid != "" {
		fmt.Fprintf(w, "  running (pid %s)\n", pid)
		fmt.Fprintln(w, "  → flexkb-imed --ibus=replace would take org.freedesktop.IBus from this process.")
	} else {
		fmt.Fprintln(w, "  not running")
		fmt.Fprintln(w, "  → flexkb-imed --ibus=alongside or --ibus=replace will succeed without conflict.")
	}
	fmt.Fprintln(w)

	// 3. Is flexkb-imed running? Probe the control socket.
	fmt.Fprintln(w, "## flexkb-imed daemon")
	socketPath := flexkbImedSocketPath()
	if socketPath == "" {
		fmt.Fprintln(w, "  XDG_RUNTIME_DIR unset — daemon won't have a control socket path")
	} else if _, err := os.Stat(socketPath); err != nil {
		fmt.Fprintf(w, "  not running (no socket at %s)\n", socketPath)
	} else {
		fmt.Fprintf(w, "  socket present: %s\n", socketPath)
		probeDaemonSocket(w, socketPath)
	}
	fmt.Fprintln(w)

	// 4. Compositor + session type.
	fmt.Fprintln(w, "## desktop session")
	desk := firstNonEmpty(os.Getenv("XDG_CURRENT_DESKTOP"), os.Getenv("XDG_SESSION_DESKTOP"), "(unknown)")
	sess := firstNonEmpty(os.Getenv("XDG_SESSION_TYPE"), "(unknown)")
	wld := os.Getenv("WAYLAND_DISPLAY")
	dpy := os.Getenv("DISPLAY")
	fmt.Fprintf(w, "  XDG_CURRENT_DESKTOP : %s\n", desk)
	fmt.Fprintf(w, "  XDG_SESSION_TYPE    : %s\n", sess)
	fmt.Fprintf(w, "  WAYLAND_DISPLAY     : %s\n", wld)
	fmt.Fprintf(w, "  DISPLAY             : %s\n", dpy)
	switch {
	case strings.Contains(strings.ToLower(desk), "gnome"):
		fmt.Fprintln(w, "  → Mutter doesn't support input-method-v2. Use flexkb-imed --ibus=replace.")
	case strings.Contains(strings.ToLower(desk), "kde"):
		fmt.Fprintln(w, "  → KWin supports input-method-v2. flexkb-imed --wayland=true is the primary path.")
	default:
		fmt.Fprintln(w, "  → If your compositor is wlroots-based, --wayland=true should work.")
	}
	fmt.Fprintln(w)

	// 5. Data tree.
	fmt.Fprintln(w, "## flexkb data tree")
	candidates := []string{}
	if home, _ := os.UserHomeDir(); home != "" {
		candidates = append(candidates, filepath.Join(home, ".config/flexkb/data"))
	}
	candidates = append(candidates, "/usr/share/flexkb/data")
	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			fmt.Fprintf(w, "  %s ✓\n", p)
		} else {
			fmt.Fprintf(w, "  %s — not present\n", p)
		}
	}
	fmt.Fprintln(w)

	// 6. Recovery cheat-sheet.
	fmt.Fprintln(w, "## recovery commands")
	fmt.Fprintln(w, "  # If flexkb-xkb's xkb tree breaks the keyboard:")
	fmt.Fprintln(w, "  sudo pacman -S --overwrite '*' xkeyboard-config")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "  # If flexkb-imed is misbehaving:")
	fmt.Fprintln(w, "  systemctl --user stop flexkb-imed")
	fmt.Fprintln(w, "  systemctl --user disable flexkb-imed")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "  # If flexkb-imed displaced ibus-daemon and you want it back:")
	fmt.Fprintln(w, "  killall flexkb-imed 2>/dev/null")
	fmt.Fprintln(w, "  ibus-daemon -drx")
	fmt.Fprintln(w)
}

// pidOf returns the PID of a running process by name, or "" if
// not found. Avoids running pgrep so the tool works even when
// procps isn't installed.
func pidOf(name string) string {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid := e.Name()
		if pid == "" || pid[0] < '0' || pid[0] > '9' {
			continue
		}
		comm, err := os.ReadFile(filepath.Join("/proc", pid, "comm"))
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(comm)) == name {
			return pid
		}
	}
	return ""
}

func flexkbImedSocketPath() string {
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		return filepath.Join(xdg, "flexkb-imed.sock")
	}
	return ""
}

// probeDaemonSocket dials the daemon's control socket and asks
// it for status. Doctor uses this to render "what's running"
// without depending on the GUI being open.
//
// A "connection refused" on a present socket file means the
// previous daemon exited without cleanup (e.g. via os.Exit on a
// startup error). We surface this distinctly so the user knows
// the file is harmless and rebuilds will overwrite it cleanly.
func probeDaemonSocket(w *bufio.Writer, path string) {
	conn, err := net.DialTimeout("unix", path, 500*time.Millisecond)
	if err != nil {
		if strings.Contains(err.Error(), "connection refused") {
			fmt.Fprintln(w, "  STALE socket file (last daemon didn't clean up on exit).")
			fmt.Fprintln(w, "  → Safe to ignore — next `flexkb-imed` start will remove it.")
			fmt.Fprintf(w, "  → Manual cleanup: rm %s\n", path)
			return
		}
		fmt.Fprintf(w, "  socket present but not dialable: %v\n", err)
		return
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(time.Second))
	if _, err := conn.Write([]byte(`{"op":"status"}` + "\n")); err != nil {
		fmt.Fprintf(w, "  write: %v\n", err)
		return
	}
	scanner := bufio.NewScanner(conn)
	if scanner.Scan() {
		fmt.Fprintf(w, "  %s\n", scanner.Text())
	} else {
		fmt.Fprintf(w, "  no response (scan err: %v)\n", scanner.Err())
	}
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
