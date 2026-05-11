// flexkb serve — HTTP server + embedded static frontend for a live
// composition preview. Run `flexkb serve`, open http://localhost:7878,
// pick a layout/variant, see the composed key grid color-coded by which
// dimension each level came from (transformation / positional overlay /
// letter overlay / substitution). MVP scope is read-only — edit/save and
// activate are deferred to a follow-up.
//
// `flexkb gui` (see gui.go) reuses the server here and wraps it in a
// dedicated window via a chosen rendering engine (webview / lorca /
// browser). The server itself is engine-agnostic.
package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/lapingvino/flexkb/internal/compose"
	"github.com/lapingvino/flexkb/internal/model"
)

//go:embed web
var webFS embed.FS

func runServe(args []string) {
	paths, rest := dataPaths(args)
	addr := "localhost:7878"
	openBrowser := false
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		switch {
		case a == "--addr" && i+1 < len(rest):
			addr = rest[i+1]
			i++
		case strings.HasPrefix(a, "--addr="):
			addr = strings.TrimPrefix(a, "--addr=")
		case a == "--open":
			openBrowser = true
		}
	}
	root := makeRoot(paths)

	url, errCh, err := startServer(root, addr)
	check(err)
	fmt.Printf("flexkb serve listening on %s\n", url)
	fmt.Printf("data search path: %v\n", paths)

	if openBrowser {
		go openInBrowser(url)
	}

	check(<-errCh)
}

// startServer binds an HTTP listener and starts serving in a goroutine,
// returning the resolved URL (useful when addr uses port :0 or omits
// the host) and a channel that receives the serve error if the server
// stops on its own. Used by both `serve` and `gui`.
func startServer(root model.DataRoot, addr string) (string, <-chan error, error) {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		return "", nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(sub)))
	mux.HandleFunc("/api/layouts", func(w http.ResponseWriter, r *http.Request) {
		handleLayouts(w, r, root)
	})
	mux.HandleFunc("/api/compose", func(w http.ResponseWriter, r *http.Request) {
		handleCompose(w, r, root)
	})
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", nil, err
	}
	url := "http://" + ln.Addr().String()
	errCh := make(chan error, 1)
	go func() { errCh <- http.Serve(ln, mux) }()
	return url, errCh, nil
}

// openInBrowser hands the URL to xdg-open after a brief pause to give
// the HTTP server's accept loop a moment to start. Used by `serve
// --open` and as the fallback engine for `gui`.
func openInBrowser(url string) {
	time.Sleep(150 * time.Millisecond)
	if err := exec.Command("xdg-open", url).Start(); err != nil {
		fmt.Fprintf(os.Stderr, "warn: xdg-open %s failed: %v\n", url, err)
	}
}

// apiVariant is the JSON shape we ship for the picker UI.
type apiVariant struct {
	Name           string   `json:"name"`
	Description    string   `json:"description"`
	Physical       string   `json:"physical"`
	Transformation string   `json:"transformation"`
	Additions      []string `json:"additions"`
	Substitutions  []string `json:"substitutions"`
	Passthrough    bool     `json:"passthrough"`
	Default        bool     `json:"default"`
}

type apiLayoutFile struct {
	File     string       `json:"file"`
	Variants []apiVariant `json:"variants"`
}

func handleLayouts(w http.ResponseWriter, _ *http.Request, root model.DataRoot) {
	names, err := root.ListLayoutFiles()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]apiLayoutFile, 0, len(names))
	for _, n := range names {
		lf, err := root.LayoutFile(n)
		if err != nil {
			continue
		}
		af := apiLayoutFile{File: lf.File}
		for _, v := range lf.Variants {
			af.Variants = append(af.Variants, apiVariant{
				Name:           v.Name,
				Description:    v.Description,
				Physical:       v.Physical,
				Transformation: v.Transformation,
				Additions:      v.Additions,
				Substitutions:  v.Substitutions,
				Passthrough:    v.Passthrough,
				Default:        v.Default,
			})
		}
		out = append(out, af)
	}
	writeJSON(w, out)
}

// apiLevel pairs a level value with the dimension that contributed it.
// Source is one of: "transformation", "position", "letter", "substitution",
// "" (no value). The frontend uses this to color the cell.
type apiLevel struct {
	Value  string `json:"value"`
	Source string `json:"source"`
}

type apiCompose struct {
	File           string                `json:"file"`
	Variant        string                `json:"variant"`
	Description    string                `json:"description"`
	Physical       string                `json:"physical"`
	Transformation string                `json:"transformation"`
	Additions      []string              `json:"additions"`
	Substitutions  []string              `json:"substitutions"`
	Keys           []string              `json:"keys"`
	Symbols        map[string][]apiLevel `json:"symbols"`
	Includes       []string              `json:"includes"`
	Warnings       []string              `json:"warnings"`
	Passthrough    bool                  `json:"passthrough"`
}

func handleCompose(w http.ResponseWriter, r *http.Request, root model.DataRoot) {
	file := r.URL.Query().Get("file")
	variant := r.URL.Query().Get("variant")
	if file == "" || variant == "" {
		http.Error(w, "file and variant query params required", http.StatusBadRequest)
		return
	}
	lf, err := root.LayoutFile(file)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	var spec model.LayoutSpec
	found := false
	for _, v := range lf.Variants {
		if v.Name == variant {
			spec = v
			found = true
			break
		}
	}
	if !found {
		http.Error(w, fmt.Sprintf("variant %q not found in %s", variant, file), http.StatusNotFound)
		return
	}

	out := apiCompose{
		File:           lf.File,
		Variant:        spec.Name,
		Description:    spec.Description,
		Physical:       spec.Physical,
		Transformation: spec.Transformation,
		Additions:      spec.Additions,
		Substitutions:  spec.Substitutions,
		Passthrough:    spec.Passthrough,
	}

	if spec.Passthrough {
		// Passthrough variants don't compose — just label them and bail.
		out.Warnings = []string{"variant is passthrough — verbatim upstream xkb"}
		writeJSON(w, out)
		return
	}

	tracked, warns, err := composeWithSources(root, spec)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out.Keys = tracked.Keys
	out.Symbols = tracked.Symbols
	out.Includes = tracked.Includes
	out.Warnings = warns
	writeJSON(w, out)
}

type trackedLayout struct {
	Keys     []string
	Symbols  map[string][]apiLevel
	Includes []string
}

// composeWithSources runs four staged composes (trans-only → +positional
// → +letter → +substitution) and diffs each stage against the previous
// to figure out which dimension contributed each level. This reuses the
// existing compose pipeline without modification — every diff is one
// pass, and we get clean per-cell provenance for the UI to color by.
func composeWithSources(root model.DataRoot, spec model.LayoutSpec) (trackedLayout, []string, error) {
	phys, err := root.Physical(spec.Physical)
	if err != nil {
		return trackedLayout{}, nil, err
	}
	trans, err := root.Transformation(spec.Transformation)
	if err != nil {
		return trackedLayout{}, nil, err
	}
	adds := make([]model.Addition, 0, len(spec.Additions))
	for _, n := range spec.Additions {
		a, err := root.Addition(n)
		if err != nil {
			return trackedLayout{}, nil, err
		}
		adds = append(adds, a)
	}
	subs := make([]model.Substitution, 0, len(spec.Substitutions))
	for _, n := range spec.Substitutions {
		s, err := root.Substitution(n)
		if err != nil {
			return trackedLayout{}, nil, err
		}
		subs = append(subs, s)
	}

	// Split each addition into a positional-only and a letter-only variant
	// so we can compose the two stages independently.
	addsPos := make([]model.Addition, 0, len(adds))
	for _, a := range adds {
		if len(a.Overlays) == 0 {
			continue
		}
		addsPos = append(addsPos, model.Addition{
			Name:     a.Name,
			Overlays: a.Overlays,
			Includes: a.Includes,
		})
	}

	r1 := compose.ComposeFromPartsWithSubs(spec, phys, trans, nil, nil)
	r2 := compose.ComposeFromPartsWithSubs(spec, phys, trans, addsPos, nil)
	r3 := compose.ComposeFromPartsWithSubs(spec, phys, trans, adds, nil)
	r4 := compose.ComposeFromPartsWithSubs(spec, phys, trans, adds, subs)

	out := trackedLayout{
		Keys:     r4.Layout.Keys,
		Symbols:  map[string][]apiLevel{},
		Includes: r4.Layout.Includes,
	}

	for _, k := range r4.Layout.Keys {
		final := levelsOf(r4.Layout.Symbols, k)
		afterLetter := levelsOf(r3.Layout.Symbols, k)
		afterPos := levelsOf(r2.Layout.Symbols, k)
		afterTrans := levelsOf(r1.Layout.Symbols, k)
		row := make([]apiLevel, len(final))
		for i, v := range final {
			row[i] = apiLevel{Value: v, Source: classifyLevel(i, v, afterLetter, afterPos, afterTrans)}
		}
		out.Symbols[k] = row
	}
	return out, r4.Warnings, nil
}

func levelsOf(symbols map[string]model.KeySymbols, k string) []string {
	s, ok := symbols[k]
	if !ok {
		return nil
	}
	return s.Levels
}

func levelAt(levels []string, i int) string {
	if i < 0 || i >= len(levels) {
		return ""
	}
	return levels[i]
}

// classifyLevel walks the four stages backwards to identify which
// dimension first put the final value at this level. If the value is
// already present after substitutions but the level before substitutions
// holds the same value (modulo no rewrite), credit upstream stages.
// Empty final values are dimensionless.
func classifyLevel(i int, final string, afterLetter, afterPos, afterTrans []string) string {
	if final == "" {
		return ""
	}
	letter := levelAt(afterLetter, i)
	pos := levelAt(afterPos, i)
	trans := levelAt(afterTrans, i)

	// Substitution: final differs from pre-substitution (afterLetter).
	if final != letter {
		return "substitution"
	}
	// Letter overlay: letter-stage differs from positional-only stage.
	if letter != pos {
		return "letter"
	}
	// Positional overlay: positional-stage differs from transformation-only.
	if pos != trans {
		return "position"
	}
	// Otherwise it's whatever the transformation already produced.
	if trans != "" {
		return "transformation"
	}
	return ""
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
