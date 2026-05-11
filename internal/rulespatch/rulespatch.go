// Package rulespatch updates the xkb rules registry files (evdev.xml,
// base.xml, evdev.lst, base.lst) so that variants flexkb generates appear
// in GUI keyboard pickers (GNOME, KDE, gnome-control-center, the COSMIC
// keyboard panel, etc.). Without this, a user can `flexkb activate
// us dvorak-mine` from the command line but their distro's "Settings →
// Keyboard" dialog won't list "dvorak-mine" because the picker reads
// rules/evdev.xml, not the symbols/ tree.
//
// Approach: parse the upstream XML / lst, merge in flexkb's layout files
// and variants, write back. For variants that already exist in the
// upstream registry we don't duplicate; for fresh combinations we add a
// new <variant> entry (and a new <layout> if needed).
package rulespatch

import (
	"bufio"
	"encoding/xml"
	"fmt"
	"os"
	"strings"
)

// LayoutInfo is what rulespatch needs to know about a flexkb layout file
// to inject it. Callers (typically the CLI) build this slice from their
// layout/variant scan and hand it off.
type LayoutInfo struct {
	// File is the symbols/<File> name and the xkb registry layout name.
	File string
	// Description is what shows in the GUI as the layout label.
	Description string
	// Variants is the per-variant entries to merge in.
	Variants []VariantInfo
}

// VariantInfo identifies one variant for registry purposes.
type VariantInfo struct {
	Name        string
	Description string
}

// --------------------------------------------------------------------------
// XML form
// --------------------------------------------------------------------------

type xmlRegistry struct {
	XMLName    xml.Name      `xml:"xkbConfigRegistry"`
	Version    string        `xml:"version,attr"`
	ModelList  *xmlModelList `xml:"modelList,omitempty"`
	LayoutList xmlLayoutList `xml:"layoutList"`
	// We don't model option lists / rules — they pass through unchanged
	// by parsing the rest as raw inner XML.
	OptionList xmlPassthrough `xml:"optionList,omitempty"`
}

type xmlPassthrough struct {
	Inner string `xml:",innerxml"`
}

type xmlModelList struct {
	Inner string `xml:",innerxml"`
}

type xmlLayoutList struct {
	Layouts []xmlLayout `xml:"layout"`
}

type xmlLayout struct {
	ConfigItem  xmlConfigItem  `xml:"configItem"`
	VariantList xmlVariantList `xml:"variantList"`
}

type xmlVariantList struct {
	Variants []xmlVariant `xml:"variant"`
}

type xmlVariant struct {
	ConfigItem xmlConfigItem `xml:"configItem"`
}

type xmlConfigItem struct {
	Name             string         `xml:"name"`
	ShortDescription string         `xml:"shortDescription,omitempty"`
	Description      string         `xml:"description"`
	CountryList      *xmlPassthrough `xml:"countryList,omitempty"`
	LanguageList     *xmlPassthrough `xml:"languageList,omitempty"`
}

// PatchXML reads an xkb rules XML file, merges in `layouts`, and writes
// the result to `dst`. The merge is conservative: existing entries are
// untouched; new variants are appended; new layouts are added at the end
// of the layoutList.
func PatchXML(srcPath, dstPath string, layouts []LayoutInfo) error {
	src, err := os.ReadFile(srcPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", srcPath, err)
	}
	var reg xmlRegistry
	if err := xml.Unmarshal(src, &reg); err != nil {
		return fmt.Errorf("parse %s: %w", srcPath, err)
	}

	// Build a quick index for "do we already have this layout?".
	byName := map[string]int{}
	for i, l := range reg.LayoutList.Layouts {
		byName[l.ConfigItem.Name] = i
	}

	for _, in := range layouts {
		idx, exists := byName[in.File]
		if !exists {
			// New layout — append.
			reg.LayoutList.Layouts = append(reg.LayoutList.Layouts, xmlLayout{
				ConfigItem: xmlConfigItem{
					Name:        in.File,
					Description: in.Description,
				},
				VariantList: xmlVariantList{Variants: variantEntries(in)},
			})
			continue
		}

		// Existing layout — append only new variants.
		existing := map[string]bool{}
		for _, v := range reg.LayoutList.Layouts[idx].VariantList.Variants {
			existing[v.ConfigItem.Name] = true
		}
		for _, v := range in.Variants {
			if existing[v.Name] {
				continue
			}
			reg.LayoutList.Layouts[idx].VariantList.Variants = append(
				reg.LayoutList.Layouts[idx].VariantList.Variants,
				xmlVariant{ConfigItem: xmlConfigItem{Name: v.Name, Description: v.Description}},
			)
		}
	}

	out, err := xml.MarshalIndent(reg, "", "  ")
	if err != nil {
		return err
	}
	// encoding/xml strips the DOCTYPE; re-add a minimal XML preamble.
	preamble := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE xkbConfigRegistry SYSTEM "xkb.dtd">
`
	return os.WriteFile(dstPath, []byte(preamble+string(out)+"\n"), 0o644)
}

func variantEntries(in LayoutInfo) []xmlVariant {
	out := make([]xmlVariant, 0, len(in.Variants))
	for _, v := range in.Variants {
		out = append(out, xmlVariant{
			ConfigItem: xmlConfigItem{Name: v.Name, Description: v.Description},
		})
	}
	return out
}

// --------------------------------------------------------------------------
// .lst form (legacy text format some tools still read)
// --------------------------------------------------------------------------

// PatchLst reads an xkb rules .lst file, ensures every flexkb layout and
// variant has an entry under the right section, and writes the result.
// The file has line-oriented sections like `! layout` and `! variant`;
// we parse those then write back preserving everything we didn't touch.
func PatchLst(srcPath, dstPath string, layouts []LayoutInfo) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()

	sections := map[string][]string{}
	order := []string{}
	current := ""

	sc := bufio.NewScanner(src)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "! ") {
			current = strings.TrimSpace(strings.TrimPrefix(line, "! "))
			if _, seen := sections[current]; !seen {
				order = append(order, current)
				sections[current] = []string{}
			}
			continue
		}
		sections[current] = append(sections[current], line)
	}
	if err := sc.Err(); err != nil {
		return err
	}

	// Ensure layout entries.
	layoutLines := sections["layout"]
	have := lstNames(layoutLines)
	for _, in := range layouts {
		if have[in.File] {
			continue
		}
		layoutLines = append(layoutLines, fmt.Sprintf("  %-15s %s", in.File, in.Description))
	}
	sections["layout"] = layoutLines

	// Ensure variant entries — format `  <name>  <file>: <description>`.
	variantLines := sections["variant"]
	haveVar := map[string]bool{}
	for _, l := range variantLines {
		fields := strings.Fields(l)
		if len(fields) >= 2 {
			haveVar[fields[1]+"/"+fields[0]] = true
		}
	}
	for _, in := range layouts {
		for _, v := range in.Variants {
			key := in.File + ":/" + v.Name // separator different from above on purpose
			_ = key
			lstKey := in.File + "/" + v.Name
			if haveVar[lstKey] {
				continue
			}
			variantLines = append(variantLines,
				fmt.Sprintf("  %-15s %s: %s", v.Name, in.File, v.Description))
		}
	}
	sections["variant"] = variantLines

	dst, err := os.Create(dstPath)
	if err != nil {
		return err
	}
	defer dst.Close()
	w := bufio.NewWriter(dst)
	for _, name := range order {
		fmt.Fprintf(w, "! %s\n", name)
		for _, line := range sections[name] {
			fmt.Fprintln(w, line)
		}
	}
	return w.Flush()
}

// lstNames extracts the first whitespace-delimited token from each non-
// blank line — that's the entry name in xkb .lst format.
func lstNames(lines []string) map[string]bool {
	out := map[string]bool{}
	for _, l := range lines {
		fields := strings.Fields(l)
		if len(fields) == 0 {
			continue
		}
		out[fields[0]] = true
	}
	return out
}

// EnsureRules walks the four canonical rules files in `outRoot/rules/`
// (evdev.xml, evdev.lst, base.xml, base.lst) and patches each with the
// flexkb layouts. Missing files are silently skipped — some lightweight
// xkb trees ship only one rules format.
func EnsureRules(outRoot string, layouts []LayoutInfo) error {
	files := []struct {
		name    string
		patcher func(string, string, []LayoutInfo) error
	}{
		{"evdev.xml", PatchXML},
		{"base.xml", PatchXML},
		{"evdev.lst", PatchLst},
		{"base.lst", PatchLst},
	}
	for _, f := range files {
		path := outRoot + "/rules/" + f.name
		if _, err := os.Stat(path); err != nil {
			continue
		}
		if err := f.patcher(path, path, layouts); err != nil {
			return fmt.Errorf("patching %s: %w", f.name, err)
		}
	}
	return nil
}

