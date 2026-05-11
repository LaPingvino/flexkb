//go:build webview

// Webview engine — native GTK/webkit2gtk window. Built when
// `go build -tags webview` is used (PKGBUILD's default). Requires
// CGO + webkit2gtk-4.1 at runtime.

package main

import (
	webview "github.com/webview/webview_go"
)

func init() { registerEngine("webview", webviewEngine) }

func webviewEngine(url string) error {
	w := webview.New(false)
	defer w.Destroy()
	w.SetTitle("flexkb — keyboard layout preview")
	w.SetSize(1100, 700, webview.HintNone)
	w.Navigate(url)
	w.Run()
	return nil
}
