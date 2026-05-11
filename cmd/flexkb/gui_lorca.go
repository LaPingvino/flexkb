//go:build lorca

// Lorca engine — launches a system Chromium-based browser in --app=
// mode (chromeless window pinned to a single URL). Built when
// `go build -tags lorca`. CGO-free; needs google-chrome / chromium /
// microsoft-edge somewhere on $PATH at runtime.

package main

import (
	"github.com/zserge/lorca"
)

func init() { registerEngine("lorca", lorcaEngine) }

func lorcaEngine(url string) error {
	ui, err := lorca.New(url, "", 1100, 700)
	if err != nil {
		return err
	}
	defer ui.Close()
	<-ui.Done()
	return nil
}
