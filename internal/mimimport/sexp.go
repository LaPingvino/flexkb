// Package mimimport converts m17n-db `.mim` input-method rule files
// into flexkb InputMethod YAML. m17n-mim is an S-expression format
// — Lisp-shaped — so the parser is in two parts: a tokenizer/AST
// builder in sexp.go, and a semantic walker that recognises the
// specific top-level forms flexkb cares about (`input-method`,
// `description`, `map`, `state`) in convert.go.
//
// Scope: the table-based subset that covers ~95% of m17n-db. Real
// mim has a Lisp action language used by complex IMs (Hangul,
// Vietnamese tone composition, anything that does pushback or
// commit-and-resume). Files that use action forms beyond literal
// strings/characters are logged and skipped at convert time; the
// parser itself accepts the full grammar so we don't lose
// information that future flexkb versions might use.
package mimimport

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Node is one element of the parsed m17n S-expression tree.
type Node struct {
	// Kind is "list", "symbol", "string", or "char". Use it to
	// discriminate before reading the typed field.
	Kind string
	// Children — only set when Kind=="list".
	Children []Node
	// Sym — only set when Kind=="symbol".
	Sym string
	// Str — only set when Kind=="string" or "char". For char, Str
	// is the single-rune string form of the character literal.
	Str string
	// Pos is the byte offset in the source where this node begins.
	// Used in error messages to point at the offending location.
	Pos int
}

// ParseSexp parses an m17n-mim S-expression file. Multiple top-
// level forms are returned as a list of root nodes; the caller (see
// convert.go) walks them to find the ones it cares about.
func ParseSexp(src string) ([]Node, error) {
	p := &parser{src: src}
	var out []Node
	for {
		p.skipWhitespaceAndComments()
		if p.pos >= len(p.src) {
			return out, nil
		}
		n, err := p.readNode()
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
}

type parser struct {
	src string
	pos int
}

func (p *parser) skipWhitespaceAndComments() {
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			p.pos++
		case c == ';':
			// Line comment: skip to next newline.
			for p.pos < len(p.src) && p.src[p.pos] != '\n' {
				p.pos++
			}
		default:
			return
		}
	}
}

func (p *parser) readNode() (Node, error) {
	if p.pos >= len(p.src) {
		return Node{}, fmt.Errorf("unexpected end of input")
	}
	start := p.pos
	c := p.src[p.pos]
	switch {
	case c == '(':
		return p.readList()
	case c == '"':
		s, err := p.readString()
		return Node{Kind: "string", Str: s, Pos: start}, err
	case c == '?':
		s, err := p.readCharLit()
		return Node{Kind: "char", Str: s, Pos: start}, err
	default:
		s := p.readSymbol()
		if s == "" {
			return Node{}, fmt.Errorf("unexpected character %q at offset %d", c, p.pos)
		}
		return Node{Kind: "symbol", Sym: s, Pos: start}, nil
	}
}

func (p *parser) readList() (Node, error) {
	start := p.pos
	p.pos++ // consume '('
	var children []Node
	for {
		p.skipWhitespaceAndComments()
		if p.pos >= len(p.src) {
			return Node{}, fmt.Errorf("unterminated list starting at offset %d", start)
		}
		if p.src[p.pos] == ')' {
			p.pos++
			return Node{Kind: "list", Children: children, Pos: start}, nil
		}
		n, err := p.readNode()
		if err != nil {
			return Node{}, err
		}
		children = append(children, n)
	}
}

func (p *parser) readString() (string, error) {
	start := p.pos
	p.pos++ // consume opening quote
	var b strings.Builder
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == '"' {
			p.pos++
			return b.String(), nil
		}
		if c == '\\' {
			p.pos++
			if p.pos >= len(p.src) {
				return "", fmt.Errorf("trailing backslash in string at offset %d", start)
			}
			esc := p.src[p.pos]
			switch esc {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case '\\':
				b.WriteByte('\\')
			case '"':
				b.WriteByte('"')
			default:
				// Unknown escape — copy verbatim. Real mim files
				// rarely hit this; permissive parser keeps moving.
				b.WriteByte(esc)
			}
			p.pos++
			continue
		}
		b.WriteByte(c)
		p.pos++
	}
	return "", fmt.Errorf("unterminated string starting at offset %d", start)
}

// readCharLit reads a character literal of the form `?X` or
// `?\uXXXX` / `?\xXX` etc. Returns the literal's single-rune
// string form so the convert layer can treat strings and chars
// uniformly when building output text.
func (p *parser) readCharLit() (string, error) {
	start := p.pos
	p.pos++ // consume '?'
	if p.pos >= len(p.src) {
		return "", fmt.Errorf("incomplete char literal at offset %d", start)
	}
	c := p.src[p.pos]
	if c == '\\' {
		p.pos++
		if p.pos >= len(p.src) {
			return "", fmt.Errorf("incomplete escape in char literal at offset %d", start)
		}
		esc := p.src[p.pos]
		p.pos++
		switch esc {
		case 'n':
			return "\n", nil
		case 't':
			return "\t", nil
		case 'r':
			return "\r", nil
		case '\\':
			return "\\", nil
		case '"':
			return "\"", nil
		case 'u':
			return p.readHexEscape(4)
		case 'U':
			return p.readHexEscape(8)
		case 'x':
			return p.readHexEscape(2)
		default:
			// Permissive — fall through with the literal byte.
			return string(esc), nil
		}
	}
	// Plain character — handle multibyte UTF-8 properly.
	r, n := utf8.DecodeRuneInString(p.src[p.pos:])
	if r == utf8.RuneError && n == 1 {
		return "", fmt.Errorf("invalid UTF-8 in char literal at offset %d", start)
	}
	p.pos += n
	return string(r), nil
}

func (p *parser) readHexEscape(width int) (string, error) {
	if p.pos+width > len(p.src) {
		return "", fmt.Errorf("incomplete hex escape at offset %d", p.pos)
	}
	var v rune
	for i := 0; i < width; i++ {
		c := p.src[p.pos+i]
		var d rune
		switch {
		case c >= '0' && c <= '9':
			d = rune(c) - '0'
		case c >= 'a' && c <= 'f':
			d = rune(c) - 'a' + 10
		case c >= 'A' && c <= 'F':
			d = rune(c) - 'A' + 10
		default:
			return "", fmt.Errorf("non-hex digit %q in hex escape at offset %d", c, p.pos+i)
		}
		v = v*16 + d
	}
	p.pos += width
	return string(v), nil
}

// readSymbol reads a bare identifier — letters, digits, hyphens,
// underscores, plus a handful of punctuation marks mim uses for
// modifier-prefixed key names ("C-x", "M-c", "S-Tab" etc).
func (p *parser) readSymbol() string {
	start := p.pos
	for p.pos < len(p.src) {
		r, n := utf8.DecodeRuneInString(p.src[p.pos:])
		if isSymbolRune(r) {
			p.pos += n
			continue
		}
		break
	}
	return p.src[start:p.pos]
}

func isSymbolRune(r rune) bool {
	switch r {
	case '(', ')', '"', ';', ' ', '\t', '\n', '\r':
		return false
	}
	if unicode.IsLetter(r) || unicode.IsDigit(r) {
		return true
	}
	// mim uses a wide range of punctuation in symbols (e.g. modifier
	// keysyms like "C-Tab"). Accept everything that's not whitespace
	// or a structural character.
	return true
}
