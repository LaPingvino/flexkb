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
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/lapingvino/flexkb/internal/compose"
	"github.com/lapingvino/flexkb/internal/model"
	"github.com/lapingvino/flexkb/internal/xkbwriter"
)

//go:embed web
var webFS embed.FS

func runServe(args []string) {
	paths, rest := dataPaths(args)
	addr := "localhost:7878"
	openBrowser := false
	overrideUsed := false
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
	// If the user passed --data explicitly we honour it as fixed.
	// Otherwise rediscover on each request so saves to user XDG paths
	// become visible without restart.
	for _, a := range args {
		if a == "--data" || strings.HasPrefix(a, "--data=") {
			overrideUsed = true
		}
	}
	rootFn := makeRootFn(paths, overrideUsed)

	url, errCh, err := startServer(rootFn, addr)
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
//
// rootFn is called on every request to compute the current DataRoot —
// when the user is on auto-discovery (no --data) this means a freshly-
// saved file under ~/.config/flexkb/data/ appears immediately in
// /api/layouts without restarting the server.
func startServer(rootFn func() model.DataRoot, addr string) (string, <-chan error, error) {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		return "", nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(sub)))
	mux.HandleFunc("/api/paths", func(w http.ResponseWriter, r *http.Request) {
		handlePaths(w, r, rootFn())
	})
	mux.HandleFunc("/api/modules", func(w http.ResponseWriter, r *http.Request) {
		handleModules(w, r, rootFn())
	})
	mux.HandleFunc("/api/layouts", func(w http.ResponseWriter, r *http.Request) {
		handleLayouts(w, r, rootFn())
	})
	mux.HandleFunc("/api/compose", func(w http.ResponseWriter, r *http.Request) {
		handleCompose(w, r, rootFn())
	})
	mux.HandleFunc("/api/compose-spec", func(w http.ResponseWriter, r *http.Request) {
		handleComposeSpec(w, r, rootFn())
	})
	mux.HandleFunc("/api/save", func(w http.ResponseWriter, r *http.Request) {
		handleSave(w, r, rootFn())
	})
	mux.HandleFunc("/api/activate", func(w http.ResponseWriter, r *http.Request) {
		handleActivate(w, r, rootFn())
	})
	mux.HandleFunc("/api/xkb", func(w http.ResponseWriter, r *http.Request) {
		handleXKB(w, r, rootFn())
	})
	mux.HandleFunc("/api/module", func(w http.ResponseWriter, r *http.Request) {
		handleModule(w, r, rootFn())
	})
	mux.HandleFunc("/api/variant", func(w http.ResponseWriter, r *http.Request) {
		handleVariant(w, r, rootFn())
	})
	mux.HandleFunc("/api/active", func(w http.ResponseWriter, r *http.Request) {
		handleActive(w, r)
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
	File       string       `json:"file"`
	Variants   []apiVariant `json:"variants"`
	SourcePath string       `json:"sourcePath"`
	SourceKind string       `json:"sourceKind"`
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
		src, _ := root.Find("layouts", n)
		af := apiLayoutFile{File: lf.File, SourcePath: src, SourceKind: classifyPath(src)}
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

// classifyPath labels a data path as user / system / dev based on its
// location. Same heuristic the `flexkb paths` CLI uses, kept in sync
// so the GUI shows familiar terms.
func classifyPath(p string) string {
	if p == "" {
		return ""
	}
	if home, _ := os.UserHomeDir(); home != "" && strings.HasPrefix(p, home) {
		return "user"
	}
	if strings.Contains(p, "/usr/") {
		return "system"
	}
	return "dev"
}

type apiPath struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
}

func handlePaths(w http.ResponseWriter, _ *http.Request, root model.DataRoot) {
	paths := root.ResolvedPaths()
	out := make([]apiPath, 0, len(paths))
	for _, p := range paths {
		out = append(out, apiPath{Path: p, Kind: classifyPath(p)})
	}
	writeJSON(w, out)
}

type apiModule struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Script      string   `json:"script,omitempty"`  // transformations
	Scripts     []string `json:"scripts,omitempty"` // additions
	SourcePath  string   `json:"sourcePath"`
	SourceKind  string   `json:"sourceKind"`
}

type apiModules struct {
	Physicals       []apiModule `json:"physicals"`
	Transformations []apiModule `json:"transformations"`
	Additions       []apiModule `json:"additions"`
	Substitutions   []apiModule `json:"substitutions"`
}

func handleModules(w http.ResponseWriter, _ *http.Request, root model.DataRoot) {
	out := apiModules{}

	if names, err := root.ListNames("physical"); err == nil {
		for n, src := range names {
			p, err := root.Physical(n)
			if err != nil {
				continue
			}
			out.Physicals = append(out.Physicals, apiModule{
				Name: n, Description: p.Description,
				SourcePath: src, SourceKind: classifyPath(src),
			})
		}
	}
	if names, err := root.ListNames("transformations"); err == nil {
		for n, src := range names {
			t, err := root.Transformation(n)
			if err != nil {
				continue
			}
			out.Transformations = append(out.Transformations, apiModule{
				Name: n, Description: t.Description, Script: t.Script,
				SourcePath: src, SourceKind: classifyPath(src),
			})
		}
	}
	if names, err := root.ListNames("additions"); err == nil {
		for n, src := range names {
			a, err := root.Addition(n)
			if err != nil {
				continue
			}
			out.Additions = append(out.Additions, apiModule{
				Name: n, Description: a.Description, Scripts: a.Scripts,
				SourcePath: src, SourceKind: classifyPath(src),
			})
		}
	}
	if names, err := root.ListNames("substitutions"); err == nil {
		for n, src := range names {
			s, err := root.Substitution(n)
			if err != nil {
				continue
			}
			out.Substitutions = append(out.Substitutions, apiModule{
				Name: n, Description: s.Description,
				SourcePath: src, SourceKind: classifyPath(src),
			})
		}
	}
	sort.Slice(out.Physicals, func(i, j int) bool { return out.Physicals[i].Name < out.Physicals[j].Name })
	sort.Slice(out.Transformations, func(i, j int) bool { return out.Transformations[i].Name < out.Transformations[j].Name })
	sort.Slice(out.Additions, func(i, j int) bool { return out.Additions[i].Name < out.Additions[j].Name })
	sort.Slice(out.Substitutions, func(i, j int) bool { return out.Substitutions[i].Name < out.Substitutions[j].Name })
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

// handleComposeSpec previews a user-built LayoutSpec without writing
// anything. The frontend's Compose tab uses this for live preview as
// the user tweaks pickers. POST body is a JSON LayoutSpec; response is
// the same shape as /api/compose.
func handleComposeSpec(w http.ResponseWriter, r *http.Request, root model.DataRoot) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var spec model.LayoutSpec
	if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
		http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if spec.Physical == "" || spec.Transformation == "" {
		http.Error(w, "physical and transformation required", http.StatusBadRequest)
		return
	}
	if spec.Name == "" {
		spec.Name = "preview"
	}
	tracked, warns, err := composeWithSources(root, spec)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	out := apiCompose{
		File:           "",
		Variant:        spec.Name,
		Description:    spec.Description,
		Physical:       spec.Physical,
		Transformation: spec.Transformation,
		Additions:      spec.Additions,
		Substitutions:  spec.Substitutions,
		Keys:           tracked.Keys,
		Symbols:        tracked.Symbols,
		Includes:       tracked.Includes,
		Warnings:       warns,
	}
	writeJSON(w, out)
}

// apiSaveRequest is what the GUI's Compose tab POSTs to /api/save when
// the user clicks "Save". A file name plus one or more variants — the
// server appends to or replaces a file in the user's XDG data dir.
type apiSaveRequest struct {
	File     string             `json:"file"`
	Variants []model.LayoutSpec `json:"variants"`
	// Merge controls whether to merge into an existing file (replace
	// matching variant names, keep the rest) or write fresh, dropping
	// anything else in that file. Default true — safer.
	Merge bool `json:"merge"`
}

type apiSaveResponse struct {
	Path string `json:"path"`
}

// handleSave writes a LayoutFile to ~/.config/flexkb/data/layouts/.
// Existing-file behaviour: if Merge=true (the default), variants in
// the request replace same-named variants in the existing file and
// new ones append; everything else stays. If Merge=false, the file is
// overwritten verbatim. The system data tree is never touched.
func handleSave(w http.ResponseWriter, r *http.Request, _ model.DataRoot) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req apiSaveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.File == "" {
		http.Error(w, "file name required", http.StatusBadRequest)
		return
	}
	if !validFileName(req.File) {
		http.Error(w, "invalid file name (a-z, 0-9, _-. only, no slashes)", http.StatusBadRequest)
		return
	}
	if len(req.Variants) == 0 {
		http.Error(w, "at least one variant required", http.StatusBadRequest)
		return
	}

	userDir, err := userLayoutsDir()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := os.MkdirAll(userDir, 0o755); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	path := filepath.Join(userDir, req.File+".yaml")

	var lf model.LayoutFile
	lf.File = req.File
	if req.Merge {
		if existing, err := os.ReadFile(path); err == nil {
			if perr := yaml.Unmarshal(existing, &lf); perr != nil {
				http.Error(w, "existing file at "+path+" failed to parse: "+perr.Error(), http.StatusConflict)
				return
			}
			if lf.File == "" {
				lf.File = req.File
			}
		}
	}

	// Merge: replace any same-named variant, append new ones.
	for _, v := range req.Variants {
		replaced := false
		for i, ex := range lf.Variants {
			if ex.Name == v.Name {
				lf.Variants[i] = v
				replaced = true
				break
			}
		}
		if !replaced {
			lf.Variants = append(lf.Variants, v)
		}
	}

	buf, err := yaml.Marshal(lf)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Tempfile + rename for atomicity.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o644); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, apiSaveResponse{Path: path})
}

// apiActivateRequest selects what `flexkb activate` should apply.
type apiActivateRequest struct {
	File    string `json:"file"`
	Variant string `json:"variant"`
}

type apiActivateResponse struct {
	OK     bool   `json:"ok"`
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
}

// handleActivate shells out to `flexkb activate <file> <variant>` so
// the existing CLI activation flow (xkb tree regen → setxkbmap →
// xkbcomp) is reused verbatim. The handler captures stdout/stderr so
// the GUI can surface any errors (typical case on Wayland: setxkbmap
// can't talk to the session — we report it without crashing).
func handleActivate(w http.ResponseWriter, r *http.Request, _ model.DataRoot) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req apiActivateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.File == "" || req.Variant == "" {
		http.Error(w, "file and variant required", http.StatusBadRequest)
		return
	}
	if !validFileName(req.File) || !validFileName(req.Variant) {
		http.Error(w, "invalid file or variant name", http.StatusBadRequest)
		return
	}
	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0]
	}
	cmd := exec.Command(exe, "activate", req.File, req.Variant)
	var stdoutBuf, stderrBuf strings.Builder
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf
	runErr := cmd.Run()
	writeJSON(w, apiActivateResponse{
		OK:     runErr == nil,
		Stdout: stdoutBuf.String(),
		Stderr: stderrBuf.String(),
	})
}

// handleXKB returns the raw composed xkb_symbols block for a variant.
// The Browse and Compose tabs use this to show the actual output that
// `flexkb compose` would emit — the canonical artifact, not just the
// pretty-printed grid.
func handleXKB(w http.ResponseWriter, r *http.Request, root model.DataRoot) {
	file := r.URL.Query().Get("file")
	variant := r.URL.Query().Get("variant")
	if file == "" || variant == "" {
		// POST form: compose a user-supplied spec directly.
		if r.Method == http.MethodPost {
			var spec model.LayoutSpec
			if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
				http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
				return
			}
			if spec.Physical == "" || spec.Transformation == "" {
				http.Error(w, "physical and transformation required", http.StatusBadRequest)
				return
			}
			if spec.Name == "" {
				spec.Name = "preview"
			}
			res, err := compose.Compose(root, spec)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			emitXKB(w, res.Layout, res.Warnings)
			return
		}
		http.Error(w, "file and variant query params required (or POST a spec)", http.StatusBadRequest)
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
		http.Error(w, "variant not found", http.StatusNotFound)
		return
	}
	res, err := compose.Compose(root, spec)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	emitXKB(w, res.Layout, res.Warnings)
}

func emitXKB(w http.ResponseWriter, layout model.ComposedLayout, warnings []string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	for _, warn := range warnings {
		fmt.Fprintf(w, "// warn: %s\n", warn)
	}
	if err := xkbwriter.WriteVariant(w, xkbwriter.Options{}, layout); err != nil {
		fmt.Fprintf(w, "\n// error writing xkb: %v\n", err)
	}
}

// apiModuleDetail is the inspect-a-module response. Carries the raw
// YAML source for power users plus a `summary` slice the frontend
// renders as a quick what-it-does view without re-parsing YAML.
type apiModuleDetail struct {
	Kind        string             `json:"kind"`
	Name        string             `json:"name"`
	Description string             `json:"description,omitempty"`
	SourcePath  string             `json:"sourcePath"`
	SourceKind  string             `json:"sourceKind"`
	Raw         string             `json:"raw"`
	Summary     []apiSummaryEntry  `json:"summary,omitempty"`
	Levels      [][]string         `json:"levels,omitempty"` // for transformations: ordered level samples
}

// apiSummaryEntry is one row in the module inspect panel — a key/value
// pair where Key is something stable (xkb code, letter, source token)
// and Value is the contributed levels or target glyph.
type apiSummaryEntry struct {
	Key   string   `json:"key"`
	Value []string `json:"value"`
	Note  string   `json:"note,omitempty"`
}

func handleModule(w http.ResponseWriter, r *http.Request, root model.DataRoot) {
	kind := r.URL.Query().Get("kind")
	name := r.URL.Query().Get("name")
	if kind == "" || name == "" {
		http.Error(w, "kind and name required", http.StatusBadRequest)
		return
	}
	subdir := map[string]string{
		"physical":       "physical",
		"transformation": "transformations",
		"addition":       "additions",
		"substitution":   "substitutions",
	}[kind]
	if subdir == "" {
		http.Error(w, "unknown kind", http.StatusBadRequest)
		return
	}
	src, err := root.Find(subdir, strings.TrimPrefix(name, "~"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	raw, err := os.ReadFile(src)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := apiModuleDetail{
		Kind: kind, Name: name,
		SourcePath: src, SourceKind: classifyPath(src),
		Raw: string(raw),
	}
	switch kind {
	case "physical":
		p, err := root.Physical(name)
		if err == nil {
			out.Description = p.Description
			out.Summary = append(out.Summary, apiSummaryEntry{Key: "keys", Value: p.Keys})
		}
	case "transformation":
		t, err := root.Transformation(name)
		if err == nil {
			out.Description = t.Description
			for _, k := range sortedKeys(t.Keys) {
				out.Summary = append(out.Summary, apiSummaryEntry{Key: k, Value: t.Keys[k].Levels})
			}
		}
	case "addition":
		a, err := root.Addition(name)
		if err == nil {
			out.Description = a.Description
			for _, k := range sortedKeys(a.Overlays) {
				out.Summary = append(out.Summary, apiSummaryEntry{Key: "pos " + k, Value: a.Overlays[k].Levels})
			}
			for _, l := range sortedLetterKeys(a.LetterOverlays) {
				lo := a.LetterOverlays[l]
				note := ""
				if lo.Priority != "" {
					note = "priority=" + lo.Priority
				}
				if len(lo.Fallback) > 0 {
					if note != "" {
						note += " · "
					}
					note += "fallback: " + strings.Join(lo.Fallback, ",")
				}
				out.Summary = append(out.Summary, apiSummaryEntry{Key: "letter " + l, Value: lo.Levels, Note: note})
			}
		}
	case "substitution":
		// Resolve the substitution, applying the inverse if asked.
		looked := name
		if strings.HasPrefix(name, "~") {
			looked = name
		}
		s, err := root.Substitution(looked)
		if err == nil {
			out.Description = s.Description
			ks := make([]string, 0, len(s.Map))
			for k := range s.Map {
				ks = append(ks, k)
			}
			sort.Strings(ks)
			for _, k := range ks {
				out.Summary = append(out.Summary, apiSummaryEntry{Key: k, Value: []string{s.Map[k]}})
			}
		}
	}
	writeJSON(w, out)
}

func sortedKeys(m map[string]model.KeySymbols) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
func sortedLetterKeys(m map[string]model.LetterOverlay) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// handleVariant — DELETE removes a single variant from a USER-layer
// layout file. System and dev paths are read-only by design (would
// silently fail on next `flexkb generate`); refuse instead of doing
// something the next CLI run won't honour.
func handleVariant(w http.ResponseWriter, r *http.Request, root model.DataRoot) {
	if r.Method != http.MethodDelete {
		http.Error(w, "DELETE required", http.StatusMethodNotAllowed)
		return
	}
	file := r.URL.Query().Get("file")
	variant := r.URL.Query().Get("variant")
	if file == "" || variant == "" {
		http.Error(w, "file and variant query params required", http.StatusBadRequest)
		return
	}
	if !validFileName(file) || !validFileName(variant) {
		http.Error(w, "invalid file or variant name", http.StatusBadRequest)
		return
	}
	src, err := root.Find("layouts", file)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if classifyPath(src) != "user" {
		http.Error(w, "refusing to modify non-user file at "+src, http.StatusForbidden)
		return
	}
	userDir, err := userLayoutsDir()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	expected := filepath.Join(userDir, file+".yaml")
	if src != expected {
		http.Error(w, "user-layer file is at "+src+" not the expected "+expected, http.StatusForbidden)
		return
	}
	buf, err := os.ReadFile(src)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var lf model.LayoutFile
	if err := yaml.Unmarshal(buf, &lf); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	filtered := lf.Variants[:0]
	removed := false
	for _, v := range lf.Variants {
		if v.Name == variant {
			removed = true
			continue
		}
		filtered = append(filtered, v)
	}
	if !removed {
		http.Error(w, "variant not present in user file", http.StatusNotFound)
		return
	}
	lf.Variants = filtered
	if len(lf.Variants) == 0 {
		// Empty file — remove it so the layout disappears from listings
		// rather than leaving a stub.
		if err := os.Remove(src); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"deleted": true, "fileRemoved": true, "path": src})
		return
	}
	out, err := yaml.Marshal(lf)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	tmp := src + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := os.Rename(tmp, src); err != nil {
		os.Remove(tmp)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"deleted": true, "fileRemoved": false, "path": src})
}

// apiActive reports the currently-applied keyboard layout. We try
// setxkbmap -query first (covers X11 + Xwayland); failing that we
// fall back to /etc/X11/xorg.conf.d/00-keyboard.conf (persistent
// system config) so the GUI still has something useful to show on
// pure Wayland sessions where setxkbmap isn't meaningful.
type apiActive struct {
	Layout  string `json:"layout"`
	Variant string `json:"variant"`
	Model   string `json:"model"`
	Source  string `json:"source"`
	OK      bool   `json:"ok"`
}

func handleActive(w http.ResponseWriter, _ *http.Request) {
	a := apiActive{}
	if out, err := exec.Command("setxkbmap", "-query").Output(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) != 2 {
				continue
			}
			k := strings.TrimSpace(parts[0])
			v := strings.TrimSpace(parts[1])
			switch k {
			case "layout":
				a.Layout = v
			case "variant":
				a.Variant = v
			case "model":
				a.Model = v
			}
		}
		if a.Layout != "" {
			a.Source = "setxkbmap"
			a.OK = true
			writeJSON(w, a)
			return
		}
	}
	// Fallback: persistent system config.
	if buf, err := os.ReadFile("/etc/X11/xorg.conf.d/00-keyboard.conf"); err == nil {
		for _, line := range strings.Split(string(buf), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "Option \"XkbLayout\"") {
				if i := strings.Index(line[20:], "\""); i >= 0 {
					a.Layout = strings.Trim(line[19+i+1:], " \t\"")
				}
				// Simpler: take the second quoted field.
				fields := splitQuoted(line)
				if len(fields) >= 2 {
					a.Layout = fields[1]
				}
			} else if strings.HasPrefix(line, "Option \"XkbVariant\"") {
				fields := splitQuoted(line)
				if len(fields) >= 2 {
					a.Variant = fields[1]
				}
			} else if strings.HasPrefix(line, "Option \"XkbModel\"") {
				fields := splitQuoted(line)
				if len(fields) >= 2 {
					a.Model = fields[1]
				}
			}
		}
		if a.Layout != "" {
			a.Source = "xorg.conf.d"
			a.OK = true
		}
	}
	writeJSON(w, a)
}

func splitQuoted(s string) []string {
	var out []string
	in := false
	cur := strings.Builder{}
	for _, r := range s {
		if r == '"' {
			if in {
				out = append(out, cur.String())
				cur.Reset()
			}
			in = !in
			continue
		}
		if in {
			cur.WriteRune(r)
		}
	}
	return out
}

// userLayoutsDir returns the highest-priority writable layouts/ dir.
// Mirrors model.DiscoverPaths' first-path choice (XDG_CONFIG_HOME/flexkb/
// data or ~/.config/flexkb/data) so a save lands where /api/layouts can
// read it back.
func userLayoutsDir() (string, error) {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "flexkb", "data", "layouts"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "flexkb", "data", "layouts"), nil
}

// validFileName guards the Save endpoint against path traversal and
// nonsense values. xkb file names are short, lowercase ascii — match
// that conservatively.
func validFileName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.'
		if !ok {
			return false
		}
	}
	return s != "." && s != ".."
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
