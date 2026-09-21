package schema

import (
	"cmp"
	"errors"
	"fmt"
	"slices"

	"github.com/fervovita/AuthZ-dev/internal/core"
)

// ErrSyntax reports source the language cannot read.
var ErrSyntax = errors.New("schema: syntax error")

// Error is one problem in a schema, placed where it starts. It matches ErrSyntax for what cannot be read,
// and core.ErrSchemaInvalid for what reads but is not a valid schema; Unwrap has the rest.
type Error struct {
	Filename  string
	Line, Col int
	Msg       string

	err error
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s:%d:%d: %s", e.Filename, e.Line, e.Col, e.Msg)
}

// Unwrap gives ErrSyntax, core.ErrSchemaInvalid, the *core.SchemaError Build reported, or,
// where a name could not be interned, what the dictionary refused it with.
func (e *Error) Unwrap() error {
	return e.err
}

func syntaxError(filename string, at token, msg string) *Error {
	return &Error{Filename: filename, Line: at.line, Col: at.col, Msg: msg, err: ErrSyntax}
}

func invalid(filename string, at token, format string, args ...any) *Error {
	return &Error{Filename: filename, Line: at.line, Col: at.col, Msg: fmt.Sprintf(format, args...), err: core.ErrSchemaInvalid}
}

// Compile reads src, a schema in the language, and builds it with its names interned in d.
// filename only labels the errors: nothing is read from disk.
// A name the checks catch never reaches d; one core refuses is interned all the same.
func Compile(filename, src string, d *core.Dictionary) (*core.Schema, error) {
	defs, err := parse(filename, src)
	if err != nil {
		return nil, err
	}

	if errs := check(filename, defs); len(errs) > 0 {
		return nil, join(errs)
	}

	return build(filename, defs, d)
}

// nameRule is ValidType's and ValidRelation's rule, said for someone writing a schema.
const nameRule = "lowercase letters, digits and _, starting with a letter, at most 64 bytes"

// check applies what core cannot see: core has no notion of a definition, and it holds ids, not names.
// Everything here runs before any name is interned, because an interned id is never given back.
func check(filename string, defs []definition) []*Error {
	var errs []*Error

	types := make(map[string]token)
	declared := make(map[string]bool)

	for _, d := range defs {
		if !core.ValidType(d.name.text) {
			errs = append(errs, invalid(filename, d.name, "type name %q is not %s", d.name.text, nameRule))
		}

		if first, ok := types[d.name.text]; ok {
			errs = append(errs, invalid(filename, d.name, "definition %s is declared twice (first at line %d)",
				d.name.text, first.line))
		} else {
			types[d.name.text] = d.name
		}

		for _, m := range d.members {
			if !core.ValidRelation(m.name.text) {
				errs = append(errs, invalid(filename, m.name, "name %q is not %s", m.name.text, nameRule))
			}

			declared[m.name.text] = true
		}
	}

	// A relation that some definition declares, but not the one referring to it, is core's to report.
	// One that no definition declares is a typo, and would be interned for good before core saw it.
	refer := func(name token) {
		if !declared[name.text] {
			errs = append(errs, invalid(filename, name, "%s is not declared in any definition", name.text))
		}
	}

	for _, d := range defs {
		for _, m := range d.members {
			for _, st := range m.types {
				if _, ok := types[st.typ.text]; !ok {
					errs = append(errs, invalid(filename, st.typ, "type %s has no definition", st.typ.text))
				}

				if st.relation.kind == tokName {
					refer(st.relation)
				}
			}

			if m.expr != nil {
				m.expr.names(refer)
			}
		}
	}

	return errs
}

// build interns the names, hands the definitions to core, and keeps where each one was written
// so that place can put Build's reasons back on the page.
func build(filename string, defs []definition, d *core.Dictionary) (*core.Schema, error) {
	types := make(map[string]core.TypeID)
	relations := make(map[string]core.RelationID)

	for _, def := range defs {
		id, err := d.InternType(def.name.text)
		if err != nil {
			return nil, &Error{Filename: filename, Line: def.name.line, Col: def.name.col, Msg: err.Error(), err: err}
		}

		types[def.name.text] = id

		for _, m := range def.members {
			id, err := d.InternRelation(m.name.text)
			if err != nil {
				return nil, &Error{Filename: filename, Line: m.name.line, Col: m.name.col, Msg: err.Error(), err: err}
			}

			relations[m.name.text] = id
		}
	}

	b := core.NewSchemaBuilder()
	at := make(map[core.RelationRef]token)

	for _, def := range defs {
		for _, m := range def.members {
			r := core.RelationRef{Type: types[def.name.text], Relation: relations[m.name.text]}
			at[r] = m.name // the later of two declarations, which is where "defined twice" belongs

			if m.expr != nil {
				b.Permission(r, m.expr.rewrite(relations))

				continue
			}

			accepted := make([]core.SubjectType, 0, len(m.types))

			for _, st := range m.types {
				typ := types[st.typ.text]

				switch {
				case st.wildcard:
					accepted = append(accepted, core.WildcardType(typ))
				case st.relation.kind == tokName:
					accepted = append(accepted, core.UsersetType(typ, relations[st.relation.text]))
				default:
					accepted = append(accepted, core.DirectType(typ))
				}
			}

			b.Relation(r, accepted...)
		}
	}

	s, err := b.Build()
	if err != nil {
		return nil, place(filename, err, at, d)
	}

	return s, nil
}

// place puts each of Build's reasons at the definition it is about, and says it in names.
func place(filename string, err error, at map[core.RelationRef]token, d *core.Dictionary) error {
	all := []error{err}

	var joined interface{ Unwrap() []error }
	if errors.As(err, &joined) {
		all = joined.Unwrap()
	}

	out := make([]*Error, 0, len(all))

	for _, e := range all {
		var se *core.SchemaError
		if !errors.As(e, &se) {
			// Build reports only SchemaErrors. Anything else is still an error, unplaced rather than lost.
			out = append(out, &Error{Filename: filename, Msg: e.Error(), err: e})

			continue
		}

		t := at[se.Ref]
		out = append(out, &Error{Filename: filename, Line: t.line, Col: t.col, Msg: se.Explain(d), err: se})
	}

	return join(out)
}

// join orders errors as they appear in the file.
func join(errs []*Error) error {
	slices.SortStableFunc(errs, func(a, b *Error) int {
		return cmp.Or(cmp.Compare(a.Line, b.Line), cmp.Compare(a.Col, b.Col))
	})

	all := make([]error, len(errs))
	for i, e := range errs {
		all[i] = e
	}

	return errors.Join(all...)
}
