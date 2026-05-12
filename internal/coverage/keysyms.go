// Package coverage analyses how much of a target language's character
// set a composed layout can produce. Used by /api/coverage to surface
// "this layout covers 87% of Polish, missing ł ą ę" in the GUI — the
// data signal future autofill heuristics will lean on for which
// fillers / additions to recommend.
package coverage

import (
	"strconv"
	"strings"
	"unicode"
)

// Keysyms maps XKB symbol token names to the Unicode rune they emit.
// Subset of /usr/include/X11/keysymdef.h focused on European-language
// diacritic letters — the ones actual layouts use to type the
// characters required by en/de/fr/es/pt/it/pl/cs/sk/hu/ro/nl/da/no/
// sv/fi/is/tr/hr/cy/ga.
//
// Each entry is curated by hand; we only include keysyms that appear
// (in either case) somewhere in the language tables. Anything not in
// this map is decoded via the Uxxxx escape or returned as-is (single-
// char tokens like "a", "B", "5" decode trivially).
var Keysyms = map[string]rune{
	// Latin-1 supplements (most common Western European).
	"adiaeresis": 'ä', "Adiaeresis": 'Ä',
	"odiaeresis": 'ö', "Odiaeresis": 'Ö',
	"udiaeresis": 'ü', "Udiaeresis": 'Ü',
	"ediaeresis": 'ë', "Ediaeresis": 'Ë',
	"idiaeresis": 'ï', "Idiaeresis": 'Ï',
	"ydiaeresis": 'ÿ', "Ydiaeresis": 'Ÿ',
	"aacute": 'á', "Aacute": 'Á',
	"eacute": 'é', "Eacute": 'É',
	"iacute": 'í', "Iacute": 'Í',
	"oacute": 'ó', "Oacute": 'Ó',
	"uacute": 'ú', "Uacute": 'Ú',
	"yacute": 'ý', "Yacute": 'Ý',
	"agrave": 'à', "Agrave": 'À',
	"egrave": 'è', "Egrave": 'È',
	"igrave": 'ì', "Igrave": 'Ì',
	"ograve": 'ò', "Ograve": 'Ò',
	"ugrave": 'ù', "Ugrave": 'Ù',
	"acircumflex": 'â', "Acircumflex": 'Â',
	"ecircumflex": 'ê', "Ecircumflex": 'Ê',
	"icircumflex": 'î', "Icircumflex": 'Î',
	"ocircumflex": 'ô', "Ocircumflex": 'Ô',
	"ucircumflex": 'û', "Ucircumflex": 'Û',
	"atilde": 'ã', "Atilde": 'Ã',
	"ntilde": 'ñ', "Ntilde": 'Ñ',
	"otilde": 'õ', "Otilde": 'Õ',
	"aring": 'å', "Aring": 'Å',
	"ae": 'æ', "AE": 'Æ',
	"oe": 'œ', "OE": 'Œ',
	"oslash": 'ø', "Oslash": 'Ø',
	"ccedilla": 'ç', "Ccedilla": 'Ç',
	"ssharp": 'ß', "U1E9E": 'ẞ',
	"thorn": 'þ', "THORN": 'Þ',
	"eth": 'ð', "ETH": 'Ð',
	"exclamdown": '¡', "questiondown": '¿',
	// Latin Extended-A (Polish, Czech, Slovak, Hungarian, Romanian,
	// Croatian, Turkish, Baltic).
	"aogonek": 'ą', "Aogonek": 'Ą',
	"eogonek": 'ę', "Eogonek": 'Ę',
	"iogonek": 'į', "Iogonek": 'Į',
	"uogonek": 'ų', "Uogonek": 'Ų',
	"cacute": 'ć', "Cacute": 'Ć',
	"nacute": 'ń', "Nacute": 'Ń',
	"sacute": 'ś', "Sacute": 'Ś',
	"zacute": 'ź', "Zacute": 'Ź',
	"lstroke": 'ł', "Lstroke": 'Ł',
	"dstroke": 'đ', "Dstroke": 'Đ',
	"hstroke": 'ħ', "Hstroke": 'Ħ',
	"tslash": 'ŧ', "Tslash": 'Ŧ',
	"ccaron": 'č', "Ccaron": 'Č',
	"dcaron": 'ď', "Dcaron": 'Ď',
	"ecaron": 'ě', "Ecaron": 'Ě',
	"lcaron": 'ľ', "Lcaron": 'Ľ',
	"ncaron": 'ň', "Ncaron": 'Ň',
	"rcaron": 'ř', "Rcaron": 'Ř',
	"scaron": 'š', "Scaron": 'Š',
	"tcaron": 'ť', "Tcaron": 'Ť',
	"zcaron": 'ž', "Zcaron": 'Ž',
	"abreve": 'ă', "Abreve": 'Ă',
	"gbreve": 'ğ', "Gbreve": 'Ğ',
	"uring": 'ů', "Uring": 'Ů',
	"udoubleacute": 'ű', "Udoubleacute": 'Ű',
	"odoubleacute": 'ő', "Odoubleacute": 'Ő',
	"amacron": 'ā', "Amacron": 'Ā',
	"emacron": 'ē', "Emacron": 'Ē',
	"imacron": 'ī', "Imacron": 'Ī',
	"omacron": 'ō', "Omacron": 'Ō',
	"umacron": 'ū', "Umacron": 'Ū',
	"zabovedot": 'ż', "Zabovedot": 'Ż',
	"abovedot": '˙', "Iabovedot": 'İ',
	"idotless": 'ı',
	"scedilla": 'ş', "Scedilla": 'Ş',
	"tcedilla": 'ţ', "Tcedilla": 'Ţ',
	// Latin Extended-A commas (Romanian post-2003).
	"U0219": 'ș', "U0218": 'Ș',
	"U021B": 'ț', "U021A": 'Ț',
}

// Decode converts a single xkb symbol token to a Unicode rune. Returns
// the rune and ok=true if the token represents a typeable character;
// returns 0/false for non-character tokens (dead keys, special keys,
// "NoSymbol", etc.). Empty tokens return 0/false silently.
func Decode(token string) (rune, bool) {
	if token == "" || token == "NoSymbol" || token == "VoidSymbol" {
		return 0, false
	}
	if r, ok := Keysyms[token]; ok {
		return r, true
	}
	// Single-rune ASCII / general-string tokens: "a", "A", "5", "comma"
	// would already be a keysym name so single-char-only here.
	if len(token) == 1 {
		r := []rune(token)[0]
		if unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsPunct(r) || unicode.IsSymbol(r) {
			return r, true
		}
	}
	// "Uxxxx" or "Uxxxxxx" — explicit unicode codepoint.
	if (len(token) == 5 || len(token) == 7) && token[0] == 'U' {
		if v, err := strconv.ParseUint(token[1:], 16, 32); err == nil {
			r := rune(v)
			if unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsPunct(r) || unicode.IsSymbol(r) {
				return r, true
			}
		}
	}
	// Tokens we don't recognise — likely dead keys, named non-letter
	// keysyms (BackSpace, Tab), or composed sequences. Skip silently.
	return 0, false
}

// runeFold returns the case-folded form for coverage comparison. Used
// so a layout that produces 'A' counts as covering 'a' in the
// requirements list and vice versa.
func runeFold(r rune) rune {
	return unicode.ToLower(r)
}

// keysymByRune is the lazy reverse of Keysyms — rune → keysym name.
// Lazy-built because Keysyms is the source of truth; if anyone updates
// it, we don't want a parallel table going stale. Both lowercase and
// uppercase entries are populated where the Keysyms map carries them
// (so 'ç' → "ccedilla" and 'Ç' → "Ccedilla").
var keysymByRune map[rune]string

func buildKeysymByRune() {
	keysymByRune = make(map[rune]string, len(Keysyms))
	for name, r := range Keysyms {
		// First write wins. Keysyms has no duplicate-rune entries
		// today, so this is deterministic.
		if _, ok := keysymByRune[r]; !ok {
			keysymByRune[r] = name
		}
	}
}

// EncodeRune returns the xkb keysym name that produces this rune, plus
// ok=true on success. ASCII letters/digits/common punct return as the
// single-char token form ("a", "5", "."), matching the Decode path.
// Other runes consult the curated Keysyms reverse map; characters
// outside it fall back to the "Uxxxx" hex escape form (uppercase hex,
// 4 or 6 digits) which xkbcommon also accepts.
func EncodeRune(r rune) (string, bool) {
	if r == 0 {
		return "", false
	}
	if r < 0x80 {
		// ASCII letters/digits map to their single-char keysym
		// token (xkb accepts these directly).
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return string(r), true
		}
	}
	if keysymByRune == nil {
		buildKeysymByRune()
	}
	if name, ok := keysymByRune[r]; ok {
		return name, true
	}
	// Fallback: explicit Unicode escape. xkbcommon recognises Uxxxx /
	// Uxxxxxx; uppercase hex is the convention. Keep 4-digit form for
	// BMP, 6-digit for supplementary planes.
	if r <= 0xFFFF {
		return "U" + strings.ToUpper(strings.Repeat("0", 4-len(strconv.FormatInt(int64(r), 16))) + strconv.FormatInt(int64(r), 16)), true
	}
	return "U" + strings.ToUpper(strings.Repeat("0", 6-len(strconv.FormatInt(int64(r), 16))) + strconv.FormatInt(int64(r), 16)), true
}

// CollectChars walks every level of every key in the composed symbols
// map and returns the set of Unicode characters the layout can produce
// (case-folded). The result is the input to coverage analysis.
func CollectChars(symbols map[string][]string) map[rune]bool {
	out := map[rune]bool{}
	for _, levels := range symbols {
		for _, lv := range levels {
			if r, ok := Decode(lv); ok {
				out[runeFold(r)] = true
			}
		}
	}
	return out
}

// CollectCharsFromStrings is a convenience for callers that already
// have a flat string slice (e.g. compose Result.Layout.Symbols[k].Levels
// pre-flattened). Strings are split on whitespace if dense.
func CollectCharsFromStrings(tokens []string) map[rune]bool {
	out := map[rune]bool{}
	for _, t := range tokens {
		for _, part := range strings.Fields(t) {
			if r, ok := Decode(part); ok {
				out[runeFold(r)] = true
			}
		}
	}
	return out
}
