package schema

import (
	"fmt"
	"strings"

	"github.com/fervovita/AuthZ-dev/internal/core"
)

// definition is one "definition name { ... }" block.
type definition struct {
	name    token
	members []member
}

// member is a relation, which lists the subject types it accepts, or a permission, which has an expression.
type member struct {
	name  token
	types []subjectType // a relation's
	expr  expr          // a permission's; nil for a relation
}

// subjectType is "type", "type:*" or "type#relation".
type subjectType struct {
	typ      token
	relation token // tokEOF when absent
	wildcard bool
}

// expr is a permission's expression: *ref, *arrow or *combination.
type expr interface {
	// names calls visit on every relation name the expression refers to.
	names(visit func(token))
	rewrite(relations map[string]core.RelationID) core.Rewrite
}

// ref names another relation on the same object.
type ref struct{ name token }

func (r *ref) names(visit func(token)) { visit(r.name) }

func (r *ref) rewrite(relations map[string]core.RelationID) core.Rewrite {
	return core.ComputedUserset(relations[r.name.text])
}

// arrow is tupleset->relation.
type arrow struct{ tupleset, relation token }

func (a *arrow) names(visit func(token)) {
	visit(a.tupleset)
	visit(a.relation)
}

func (a *arrow) rewrite(relations map[string]core.RelationID) core.Rewrite {
	return core.TupleToUserset(relations[a.tupleset.text], relations[a.relation.text])
}

// combination joins operands with one operator. Parentheses are not flattened into the chain around
// them, so the tree is the one written: "+" and "&" hold any number of operands, and "-" exactly two.
type combination struct {
	op       string
	operands []expr
}

func (c *combination) names(visit func(token)) {
	for _, o := range c.operands {
		o.names(visit)
	}
}

func (c *combination) rewrite(relations map[string]core.RelationID) core.Rewrite {
	children := make([]core.Rewrite, len(c.operands))
	for i, o := range c.operands {
		children[i] = o.rewrite(relations)
	}

	switch c.op {
	case "+":
		return core.Union(children...)
	case "&":
		return core.Intersection(children...)
	default:
		return core.Exclusion(children[0], children[1])
	}
}

// reserved are the words a name may not take. name refuses nil as well, with a message of its own.
var reserved = map[string]bool{"definition": true, "relation": true, "permission": true}

// unsupported are the top-level words v1 leaves out. Naming them says so, rather than that a definition was expected.
var unsupported = map[string]bool{"caveat": true, "use": true, "import": true, "partial": true}

// parser reads a whole file, stopping at the first syntax error.
type parser struct {
	filename string
	src      string
	lex      *lexer
	tok      token
}

func parse(filename, src string) ([]definition, error) {
	p := &parser{filename: filename, src: src, lex: newLexer(filename, src)}
	if err := p.advance(); err != nil {
		return nil, err
	}

	var defs []definition

	for p.tok.kind != tokEOF {
		d, err := p.definition()
		if err != nil {
			return nil, err
		}

		defs = append(defs, d)
	}

	return defs, nil
}

func (p *parser) advance() error {
	t, err := p.lex.next()
	if err != nil {
		return err
	}

	p.tok = t

	return nil
}

func (p *parser) is(kind tokenKind, text string) bool {
	return p.tok.kind == kind && p.tok.text == text
}

func (p *parser) expect(punct string) error {
	if !p.is(tokPunct, punct) {
		return p.errorf("expected %q, found %s", punct, p.tok)
	}

	return p.advance()
}

func (p *parser) errorf(format string, args ...any) error {
	return syntaxError(p.filename, p.tok, fmt.Sprintf(format, args...))
}

// name takes a name that is not a keyword. what says what the name is for.
func (p *parser) name(what string) (token, error) {
	t := p.tok

	switch {
	case t.kind != tokName:
		return t, p.errorf("expected %s, found %s", what, t)
	case t.text == "nil":
		return t, p.errorf("nil is not supported")
	case reserved[t.text]:
		return t, p.errorf("expected %s, found the keyword %q", what, t.text)
	}

	return t, p.advance()
}

func (p *parser) definition() (definition, error) {
	if !p.is(tokName, "definition") {
		if p.tok.kind == tokName && unsupported[p.tok.text] {
			return definition{}, p.errorf("%s is not supported", p.tok.text)
		}

		return definition{}, p.errorf("expected definition, found %s", p.tok)
	}

	if err := p.advance(); err != nil {
		return definition{}, err
	}

	name, err := p.name("a type name")
	if err != nil {
		return definition{}, err
	}

	if err := p.expect("{"); err != nil {
		return definition{}, err
	}

	d := definition{name: name}

	for !p.is(tokPunct, "}") {
		var m member

		switch {
		case p.is(tokName, "relation"):
			m, err = p.relation()
		case p.is(tokName, "permission"):
			m, err = p.permission()
		default:
			return definition{}, p.errorf("expected relation, permission or \"}\", found %s", p.tok)
		}

		if err != nil {
			return definition{}, err
		}

		d.members = append(d.members, m)
	}

	return d, p.advance()
}

func (p *parser) relation() (member, error) {
	if err := p.advance(); err != nil {
		return member{}, err
	}

	name, err := p.name("a relation name")
	if err != nil {
		return member{}, err
	}

	if err := p.expect(":"); err != nil {
		return member{}, err
	}

	m := member{name: name}

	for {
		st, err := p.subjectType()
		if err != nil {
			return member{}, err
		}

		m.types = append(m.types, st)

		if !p.is(tokPunct, "|") {
			break
		}

		if err := p.advance(); err != nil {
			return member{}, err
		}
	}

	if p.is(tokName, "with") {
		return member{}, p.errorf("caveats and expiration are not supported")
	}

	return m, nil
}

func (p *parser) subjectType() (subjectType, error) {
	typ, err := p.name("a subject type")
	if err != nil {
		return subjectType{}, err
	}

	st := subjectType{typ: typ}

	switch {
	case p.is(tokPunct, ":"):
		if err := p.advance(); err != nil {
			return subjectType{}, err
		}

		if err := p.expect("*"); err != nil {
			return subjectType{}, err
		}

		st.wildcard = true

		if p.is(tokPunct, "#") {
			return subjectType{}, p.errorf("a wildcard cannot carry a relation")
		}
	case p.is(tokPunct, "#"):
		if err := p.advance(); err != nil {
			return subjectType{}, err
		}

		if st.relation, err = p.name("a relation name"); err != nil {
			return subjectType{}, err
		}
	}

	return st, nil
}

func (p *parser) permission() (member, error) {
	if err := p.advance(); err != nil {
		return member{}, err
	}

	name, err := p.name("a permission name")
	if err != nil {
		return member{}, err
	}

	if err := p.expect("="); err != nil {
		return member{}, err
	}

	e, err := p.expr()
	if err != nil {
		return member{}, err
	}

	return member{name: name, expr: e}, nil
}

func (p *parser) isOperator() bool {
	return p.is(tokPunct, "+") || p.is(tokPunct, "&") || p.is(tokPunct, "-")
}

// span is where an operand sits in the source, parentheses included.
type span struct{ start, end int }

// expr reads operands joined by one operator. A second operator ends it with an error, since
// mixed operators without parentheses read differently from one implementation to the next.
func (p *parser) expr() (expr, error) {
	first, at, err := p.operand()
	if err != nil {
		return nil, err
	}

	if !p.isOperator() {
		return first, nil
	}

	op := p.tok.text
	operands, spans := []expr{first}, []span{at}

	for p.isOperator() {
		if p.tok.text != op {
			return nil, p.mixed(op, spans)
		}

		if err := p.advance(); err != nil {
			return nil, err
		}

		next, at, err := p.operand()
		if err != nil {
			return nil, err
		}

		operands, spans = append(operands, next), append(spans, at)
	}

	if op != "-" {
		return &combination{op: op, operands: operands}, nil
	}

	// A chain of exclusions reads from the left, as subtraction does.
	acc := operands[0]
	for _, o := range operands[1:] {
		acc = &combination{op: "-", operands: []expr{acc, o}}
	}

	return acc, nil
}

func (p *parser) operand() (expr, span, error) {
	start := p.tok.off

	if p.is(tokPunct, "(") {
		if err := p.advance(); err != nil {
			return nil, span{}, err
		}

		e, err := p.expr()
		if err != nil {
			return nil, span{}, err
		}

		end := p.tok.end()
		if err := p.expect(")"); err != nil {
			return nil, span{}, err
		}

		return e, span{start, end}, nil
	}

	name, err := p.name("a relation name")
	if err != nil {
		return nil, span{}, err
	}

	if !p.is(tokPunct, "->") {
		return &ref{name: name}, span{start, name.end()}, nil
	}

	if err := p.advance(); err != nil {
		return nil, span{}, err
	}

	rel, err := p.name("a relation name")
	if err != nil {
		return nil, span{}, err
	}

	if p.is(tokPunct, "->") {
		return nil, span{}, p.errorf("chained arrows are not supported")
	}

	return &arrow{tupleset: name, relation: rel}, span{start, rel.end()}, nil
}

// mixed reports op2 following a chain of op, quoting both ways the source could be meant.
func (p *parser) mixed(op string, chain []span) error {
	op2 := p.tok
	plain := syntaxError(p.filename, op2, fmt.Sprintf("cannot mix %s and %s without parentheses", op, op2.text))

	if err := p.advance(); err != nil {
		return plain
	}

	_, right, err := p.operand()
	if err != nil {
		return plain
	}

	// An expression may be wrapped over several lines; the suggestions read better on one.
	text := func(from, to span) string {
		return strings.Join(strings.Fields(p.src[from.start:to.end]), " ")
	}

	first, last := chain[0], chain[len(chain)-1]

	return syntaxError(p.filename, op2, fmt.Sprintf(
		"cannot mix %s and %s without parentheses; write one of\n\t(%s) %s %s\n\t%s %s (%s %s %s)",
		op, op2.text,
		text(first, last), op2.text, text(right, right),
		text(first, chain[len(chain)-2]), op, text(last, last), op2.text, text(right, right)))
}
