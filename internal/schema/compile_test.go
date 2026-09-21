package schema

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/fervovita/AuthZ-dev/internal/core"
)

func compile(t *testing.T, src string) (*core.Schema, *core.Dictionary) {
	t.Helper()

	d := core.NewDictionary()

	s, err := Compile("test.schema", src, d)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	return s, d
}

// compileErr compiles src expecting it to fail with one problem, and returns that problem.
func compileErr(t *testing.T, src string) *Error {
	t.Helper()

	_, err := Compile("test.schema", src, core.NewDictionary())
	if err == nil {
		t.Fatal("Compile succeeded; want an error")
	}

	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("err = %v; want an *Error", err)
	}

	if strings.Count(err.Error(), "test.schema:") != 1 {
		t.Fatalf("err = %v; want exactly one problem", err)
	}

	return e
}

func TestCompileExample(t *testing.T) {
	t.Parallel()

	s, d := compile(t, `
		definition user {}

		// Folders nest, and viewing one lets you view what is inside.
		definition folder {
			relation parent: folder
			relation viewer: user | user:* | team#member
			permission view = viewer + parent->view
		}

		definition team {
			relation member: user | team#member
		}

		definition document {
			relation parent: folder
			relation viewer: user | user:* | team#member
			relation banned: user
			permission view = (viewer + parent->view) - banned
		}
	`)

	typ, rel := d.LookupType, d.LookupRelation
	docRef := func(name string) core.RelationRef {
		return core.RelationRef{Type: typ("document"), Relation: rel(name)}
	}

	rw, ok := s.Rewrite(docRef("view"))
	if !ok {
		t.Fatal("document#view is not defined")
	}

	want := core.Exclusion(
		core.Union(core.ComputedUserset(rel("viewer")), core.TupleToUserset(rel("parent"), rel("view"))),
		core.ComputedUserset(rel("banned")),
	)
	if !reflect.DeepEqual(rw, want) {
		t.Errorf("document#view = %+v; want %+v", rw, want)
	}

	for _, c := range []struct {
		st   core.SubjectType
		want bool
	}{
		{core.DirectType(typ("user")), true},
		{core.WildcardType(typ("user")), true},
		{core.UsersetType(typ("team"), rel("member")), true},
		{core.DirectType(typ("team")), false},
	} {
		if got := s.Allows(docRef("viewer"), c.st); got != c.want {
			t.Errorf("document#viewer accepts %+v = %v; want %v", c.st, got, c.want)
		}
	}
}

// The tree is the one written: a chain of one operator is a single node, and what parentheses
// hold is not flattened into the chain around them.
func TestCompileKeepsTheTreeAsWritten(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		expr string
		want func(r func(string) core.RelationID) core.Rewrite
	}{
		{"a", func(r func(string) core.RelationID) core.Rewrite {
			return core.ComputedUserset(r("a"))
		}},
		{"((a))", func(r func(string) core.RelationID) core.Rewrite {
			return core.ComputedUserset(r("a"))
		}},
		{"p->a", func(r func(string) core.RelationID) core.Rewrite {
			return core.TupleToUserset(r("p"), r("a"))
		}},
		{"a + b + c", func(r func(string) core.RelationID) core.Rewrite {
			return core.Union(core.ComputedUserset(r("a")), core.ComputedUserset(r("b")), core.ComputedUserset(r("c")))
		}},
		{"(a + b) + c", func(r func(string) core.RelationID) core.Rewrite {
			return core.Union(core.Union(core.ComputedUserset(r("a")), core.ComputedUserset(r("b"))), core.ComputedUserset(r("c")))
		}},
		{"a & b & c", func(r func(string) core.RelationID) core.Rewrite {
			return core.Intersection(core.ComputedUserset(r("a")), core.ComputedUserset(r("b")), core.ComputedUserset(r("c")))
		}},
		{"a - b - c", func(r func(string) core.RelationID) core.Rewrite {
			return core.Exclusion(core.Exclusion(core.ComputedUserset(r("a")), core.ComputedUserset(r("b"))), core.ComputedUserset(r("c")))
		}},
		{"(a + b) - c", func(r func(string) core.RelationID) core.Rewrite {
			return core.Exclusion(core.Union(core.ComputedUserset(r("a")), core.ComputedUserset(r("b"))), core.ComputedUserset(r("c")))
		}},
		{"a + (b - c)", func(r func(string) core.RelationID) core.Rewrite {
			return core.Union(core.ComputedUserset(r("a")), core.Exclusion(core.ComputedUserset(r("b")), core.ComputedUserset(r("c"))))
		}},
		{"(a & p->b) + c", func(r func(string) core.RelationID) core.Rewrite {
			return core.Union(core.Intersection(core.ComputedUserset(r("a")), core.TupleToUserset(r("p"), r("b"))), core.ComputedUserset(r("c")))
		}},
	} {
		t.Run(c.expr, func(t *testing.T) {
			t.Parallel()

			s, d := compile(t, `
				definition user {}
				definition doc {
					relation a: user
					relation b: user
					relation c: user
					relation p: doc
					permission x = `+c.expr+`
				}`)

			rw, ok := s.Rewrite(core.RelationRef{Type: d.LookupType("doc"), Relation: d.LookupRelation("x")})
			if !ok {
				t.Fatal("doc#x is not defined")
			}

			if want := c.want(d.LookupRelation); !reflect.DeepEqual(rw, want) {
				t.Errorf("doc#x = %+v; want %+v", rw, want)
			}
		})
	}
}

// Different operators without parentheses read differently from one implementation to the next,
// so the error offers both readings instead of picking one.
func TestCompileRejectsMixedOperators(t *testing.T) {
	t.Parallel()

	const schema = `definition user {}
definition doc {
  relation a: user
  relation b: user
  relation c: user
  relation d: user
  permission x = %s
}`

	for _, c := range []struct {
		expr, want string
		col        int
	}{
		{"a + b - c", "cannot mix + and - without parentheses; write one of\n\t(a + b) - c\n\ta + (b - c)", 24},
		{"a - b + c", "cannot mix - and + without parentheses; write one of\n\t(a - b) + c\n\ta - (b + c)", 24},
		{"a & b + c", "cannot mix & and + without parentheses; write one of\n\t(a & b) + c\n\ta & (b + c)", 24},
		{"a + b + c & d", "cannot mix + and & without parentheses; write one of\n\t(a + b + c) & d\n\ta + b + (c & d)", 28},
		{"(a + b) - c & d", "cannot mix - and & without parentheses; write one of\n\t((a + b) - c) & d\n\t(a + b) - (c & d)", 30},
		{"a + b -", "cannot mix + and - without parentheses", 24},
	} {
		t.Run(c.expr, func(t *testing.T) {
			t.Parallel()

			e := compileErr(t, strings.Replace(schema, "%s", c.expr, 1))

			if !errors.Is(e, ErrSyntax) {
				t.Errorf("err = %v; want ErrSyntax", e)
			}

			if e.Msg != c.want {
				t.Errorf("Msg =\n%s\nwant\n%s", e.Msg, c.want)
			}

			if e.Line != 7 || e.Col != c.col {
				t.Errorf("at %d:%d; want 7:%d, the second operator", e.Line, e.Col, c.col)
			}
		})
	}
}

// A long expression is often wrapped over several lines, but a suggestion is read on one.
func TestCompileQuotesAWrappedExpressionOnOneLine(t *testing.T) {
	t.Parallel()

	e := compileErr(t, `definition user {}
definition doc {
  relation a: user
  relation b: user
  relation c: user
  permission x = a +
    b -
    c
}`)

	want := "cannot mix + and - without parentheses; write one of\n\t(a + b) - c\n\ta + (b - c)"
	if e.Msg != want {
		t.Errorf("Msg =\n%s\nwant\n%s", e.Msg, want)
	}
}

func TestCompileRejectsWhatV1LeavesOut(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		name, src, want string
	}{
		{"caveat", `caveat ip(allowed ipaddress) { true }`, "caveat is not supported"},
		{"use", `use expiration`, "use is not supported"},
		{"import", `import "other"`, "import is not supported"},
		{"partial", `partial shared {}`, "partial is not supported"},
		{"with", `definition user {} definition d { relation r: user with expiration }`, "caveats and expiration are not supported"},
		{"nil", `definition user {} definition d { relation r: user permission p = nil }`, "nil is not supported"},
		{"arrow functions", `definition d { relation r: d permission p = r.any(p) }`, "arrow functions such as .any() and .all() are not supported"},
		{"chained arrows", `definition d { relation r: d permission p = r->r->p }`, "chained arrows are not supported"},
		{"wildcard with a relation", `definition t { relation m: t relation r: t:*#m }`, "a wildcard cannot carry a relation"},
		{"prefixed type", `definition org/user {}`, "prefixed names such as myorg/document are not supported"},
		{"prefixed subject type", `definition user {} definition d { relation r: org/user }`, "prefixed names"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			e := compileErr(t, c.src)

			if !errors.Is(e, ErrSyntax) {
				t.Errorf("err = %v; want ErrSyntax", e)
			}

			if !strings.Contains(e.Msg, c.want) {
				t.Errorf("Msg = %q; want it to say %q", e.Msg, c.want)
			}
		})
	}
}

func TestCompileRejectsMalformedSource(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		name, src, want string
	}{
		{"no definition", `relation r: user`, `expected definition, found "relation"`},
		{"unclosed block", `definition user {`, `expected relation, permission or "}", found end of file`},
		{"unclosed comment", `definition user {} /* open`, "comment is not closed with */"},
		{"no subject type", `definition d { relation r: }`, `expected a subject type, found "}"`},
		{"no expression", `definition d { permission p = }`, `expected a relation name, found "}"`},
		{"unclosed parenthesis", `definition d { relation r: d permission p = (r }`, `expected ")", found "}"`},
		{"keyword as a name", `definition d { relation relation: d }`, `expected a relation name, found the keyword "relation"`},
		{"stray character", `definition d { relation r: d, d }`, `unexpected ','`},
		{"wildcard without star", `definition d { relation r: d:x }`, `expected "*", found "x"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			e := compileErr(t, c.src)

			if !errors.Is(e, ErrSyntax) {
				t.Errorf("err = %v; want ErrSyntax", e)
			}

			if !strings.Contains(e.Msg, c.want) {
				t.Errorf("Msg = %q; want it to say %q", e.Msg, c.want)
			}
		})
	}
}

// Names follow core's rules, and every name refers to something the file declares.
func TestCompileChecksNames(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		name, src, want string
		line, col       int
	}{
		{"uppercase type", "definition User {}", `type name "User" is not lowercase letters`, 1, 12},
		{"leading digit", "definition d {\n  relation 1r: d\n}", `name "1r" is not lowercase letters`, 2, 12},
		{"too long", "definition d {\n  relation " + strings.Repeat("r", 65) + ": d\n}", "at most 64 bytes", 2, 12},
		{"undefined type", "definition d {\n  relation r: usr\n}", "type usr has no definition", 2, 15},
		{"undeclared relation", "definition d {\n  relation r: d\n  permission p = rr\n}", "rr is not declared in any definition", 3, 18},
		{"undeclared userset relation", "definition d {\n  relation r: d#m\n}", "m is not declared in any definition", 2, 17},
		{"definition twice", "definition d {}\ndefinition d {}", "definition d is declared twice (first at line 1)", 2, 12},
		{"member twice", "definition d {\n  relation r: d\n  permission r = r\n}", "relation d#r defined twice", 3, 14},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			e := compileErr(t, c.src)

			if !errors.Is(e, core.ErrSchemaInvalid) {
				t.Errorf("err = %v; want core.ErrSchemaInvalid", e)
			}

			if !strings.Contains(e.Msg, c.want) {
				t.Errorf("Msg = %q; want it to say %q", e.Msg, c.want)
			}

			if e.Line != c.line || e.Col != c.col {
				t.Errorf("at %d:%d; want %d:%d", e.Line, e.Col, c.line, c.col)
			}
		})
	}
}

// Every problem found before core is asked comes back, in the order of the file.
// The names are checked before any reference is, so these two are found the other way around.
func TestCompileReportsEveryCheckError(t *testing.T) {
	t.Parallel()

	_, err := Compile("test.schema", `definition d {
  relation r: d
  permission p = typo
}
definition Bad {}`, core.NewDictionary())

	want := "test.schema:3:18: typo is not declared in any definition\n" +
		`test.schema:5:12: type name "Bad" is not ` + nameRule
	if err == nil || err.Error() != want {
		t.Fatalf("err =\n%v\nwant\n%s", err, want)
	}
}

// Build's reasons come back in names, at the definition each is about, in the order of the file.
// Build reports the loop after every other reason, so here the order of the file is not Build's.
func TestCompilePlacesBuildErrors(t *testing.T) {
	t.Parallel()

	_, err := Compile("test.schema", `definition user {}
definition folder {
  relation editor: user
}
definition document {
  relation viewer: user
  permission deny = viewer - deny
  permission view = viewer + editor
}`, core.NewDictionary())

	want := "test.schema:7:14: document#deny excludes document#deny, which reaches back to it\n" +
		"test.schema:8:14: document#view refers to undefined document#editor"
	if err == nil || err.Error() != want {
		t.Fatalf("err =\n%v\nwant\n%s", err, want)
	}

	var se *core.SchemaError
	if !errors.As(err, &se) {
		t.Errorf("err = %v; want Build's *core.SchemaError behind it", err)
	}

	if !errors.Is(err, core.ErrSchemaInvalid) {
		t.Errorf("err = %v; want core.ErrSchemaInvalid", err)
	}
}

// A name that fails a check is not interned, since an interned id is never given back.
// What core refuses is another matter: by then the names are in the dictionary.
func TestCompileChecksNamesBeforeInterning(t *testing.T) {
	t.Parallel()

	d := core.NewDictionary()

	if _, err := Compile("test.schema", "definition d {\n  relation r: d\n  permission p = typo\n}", d); err == nil {
		t.Fatal("Compile succeeded; want an error")
	}

	for _, name := range []string{"d", "r", "p", "typo"} {
		if d.LookupType(name) != 0 || d.LookupRelation(name) != 0 {
			t.Errorf("%q was interned by a schema that was refused", name)
		}
	}
}

// Comments decide nothing, in either form, and a comment over several lines still leaves the lines after it counted right.
func TestCompileSkipsComments(t *testing.T) {
	t.Parallel()

	s, d := compile(t, `/**
 * an entity that can be granted permissions
 */
definition user {}

// a line comment holding /* does not open one
definition document {
	/** who may read it */
	relation /* inline */ viewer: user /**/
	permission view = viewer // and a trailing one
}`)

	rw, ok := s.Rewrite(core.RelationRef{Type: d.LookupType("document"), Relation: d.LookupRelation("view")})
	if !ok || !reflect.DeepEqual(rw, core.ComputedUserset(d.LookupRelation("viewer"))) {
		t.Errorf("document#view = %+v, %v; want viewer", rw, ok)
	}

	e := compileErr(t, "/*\n\n*/ definition d {\n  relation r: usr\n}")
	if e.Line != 4 || e.Col != 15 {
		t.Errorf("at %d:%d; want 4:15, the lines inside the comment counted", e.Line, e.Col)
	}
}

func TestCompileAcceptsAnEmptyFile(t *testing.T) {
	t.Parallel()

	compile(t, "// nothing yet\n")
}
