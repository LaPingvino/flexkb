package coverage

import (
	"sort"

	"gopkg.in/yaml.v3"
)

// Locale describes the character requirements of one language. Chars
// is the case-folded set of "exemplar" letters the language uses in
// running text — small enough to be hand-curated, big enough to give
// a useful coverage signal. Loaded from data/locales.yaml.
type Locale struct {
	Code  string `yaml:"code"`
	Name  string `yaml:"name"`
	Chars string `yaml:"chars"`
}

// LocaleTable is the on-disk format for data/locales.yaml.
// LayoutHints maps an xkb layout-file basename ("us", "ch") to the
// list of language codes that file is relevant to ("ch" → de, fr, it).
// Falls back to layout-name = locale-code if a hint is missing.
type LocaleTable struct {
	Locales     []Locale            `yaml:"locales"`
	LayoutHints map[string][]string `yaml:"layout_hints"`
}

// LocalesFor returns the locale codes relevant to a layout file.
// Consults LayoutHints first; if none, falls back to "file basename
// is a locale code" matching against known locales.
func (t LocaleTable) LocalesFor(file string) []string {
	if hint, ok := t.LayoutHints[file]; ok {
		return hint
	}
	for _, l := range t.Locales {
		if l.Code == file {
			return []string{l.Code}
		}
	}
	return nil
}

// Report is the coverage result for one (layout, locale) pair.
type Report struct {
	Code     string   `json:"code"`
	Name     string   `json:"name"`
	Total    int      `json:"total"`
	Covered  int      `json:"covered"`
	Missing  []string `json:"missing"`
	Coverage float64  `json:"coverage"` // 0-1
}

// Analyze checks each character in `required` against the layout's
// produced-character set. Returns a Report. Characters are compared
// case-folded.
func Analyze(produced map[rune]bool, code, name, required string) Report {
	r := Report{Code: code, Name: name}
	seen := map[rune]bool{}
	for _, ch := range required {
		f := runeFold(ch)
		if seen[f] {
			continue
		}
		seen[f] = true
		r.Total++
		if produced[f] {
			r.Covered++
		} else {
			r.Missing = append(r.Missing, string(ch))
		}
	}
	if r.Total > 0 {
		r.Coverage = float64(r.Covered) / float64(r.Total)
	}
	sort.Strings(r.Missing)
	return r
}

// LoadTable parses the locales YAML.
func LoadTable(yamlBytes []byte) (LocaleTable, error) {
	var t LocaleTable
	if err := yaml.Unmarshal(yamlBytes, &t); err != nil {
		return t, err
	}
	return t, nil
}

// AnalyzeAll runs the analyzer for every locale in the table and
// returns the results sorted by code.
func AnalyzeAll(produced map[rune]bool, t LocaleTable) []Report {
	out := make([]Report, 0, len(t.Locales))
	for _, l := range t.Locales {
		out = append(out, Analyze(produced, l.Code, l.Name, l.Chars))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out
}
