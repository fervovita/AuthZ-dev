package schema

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

type tokenKind uint8

const (
	tokEOF tokenKind = iota
	tokName
	tokPunct
)

// token is one lexeme.
type token struct {
	kind      tokenKind
	text      string
	line, col int
	off       int // byte offset, which errors use to quote the source back
}

// end is the offset just past the token.
func (t token) end() int {
	return t.off + len(t.text)
}

// String describes the token for an error message.
func (t token) String() string {
	if t.kind == tokEOF {
		return "end of file"
	}

	return fmt.Sprintf("%q", t.text)
}

// lexer splits source into tokens. Any run of letters, digits and underscores is a name here:
// which names are legal is core's rule, checked once the whole file is read.
type lexer struct {
	filename  string
	src       string
	off       int
	line, col int
}

func newLexer(filename, src string) *lexer {
	return &lexer{filename: filename, src: src, line: 1, col: 1}
}

func (l *lexer) next() (token, error) {
	for l.off < len(l.src) {
		switch rest := l.src[l.off:]; {
		case rest[0] == ' ' || rest[0] == '\t' || rest[0] == '\r' || rest[0] == '\n':
			l.advance(1)
		case strings.HasPrefix(rest, "//"):
			for l.off < len(l.src) && l.src[l.off] != '\n' {
				l.advance(1)
			}
		case strings.HasPrefix(rest, "/*"):
			end := strings.Index(rest[2:], "*/")
			if end < 0 {
				return token{}, syntaxError(l.filename, token{line: l.line, col: l.col}, "comment is not closed with */")
			}

			l.advance(2 + end + 2)
		default:
			return l.lex(rest)
		}
	}

	return token{kind: tokEOF, line: l.line, col: l.col, off: l.off}, nil
}

func (l *lexer) lex(rest string) (token, error) {
	t := token{line: l.line, col: l.col, off: l.off}

	switch {
	case nameByte(rest[0]):
		n := 1
		for n < len(rest) && nameByte(rest[n]) {
			n++
		}

		t.kind, t.text = tokName, rest[:n]
	case strings.HasPrefix(rest, "->"):
		t.kind, t.text = tokPunct, "->"
	case strings.IndexByte("{}():|#*+&-=", rest[0]) >= 0:
		t.kind, t.text = tokPunct, rest[:1]
	case rest[0] == '.':
		return t, syntaxError(l.filename, t, "arrow functions such as .any() and .all() are not supported; use tupleset->relation")
	case rest[0] == '/':
		// next has taken any comment, so a lone slash is a name carrying a prefix.
		return t, syntaxError(l.filename, t, "prefixed names such as myorg/document are not supported; drop the prefix")
	default:
		r, _ := utf8.DecodeRuneInString(rest)

		return t, syntaxError(l.filename, t, fmt.Sprintf("unexpected %q", r))
	}

	l.advance(len(t.text))

	return t, nil
}

func (l *lexer) advance(n int) {
	for range n {
		if l.src[l.off] == '\n' {
			l.line++
			l.col = 1
		} else {
			l.col++
		}

		l.off++
	}
}

func nameByte(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || (c == '_')
}
