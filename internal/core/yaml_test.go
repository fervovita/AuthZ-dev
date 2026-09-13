package core

import (
	"bytes"
	"embed"
	"io/fs"
	"path"
	"strings"
	"testing"

	yaml "go.yaml.in/yaml/v3"
)

// The cases are compiled in, so running the suite touches no files and an empty
// testdata is a build failure rather than a silently empty run.
//
//go:embed testdata/*.yaml
var caseFS embed.FS

// caseFile is one scenario: a schema, the tuples stored under it, and the checks
// that schema is supposed to answer a certain way.
type caseFile struct {
	Name   string             `yaml:"name"`
	Schema map[string]typeDef `yaml:"schema"`
	Tuples []string           `yaml:"tuples"`
	Checks []checkCase        `yaml:"checks"`
}

// typeDef is one object type. Relations store tuples and list the subjects they accept.
// Permissions are computed and store none.
type typeDef struct {
	Relations   map[string][]string `yaml:"relations"`
	Permissions map[string]any      `yaml:"permissions"`
}

type checkCase struct {
	Check  string `yaml:"check"`
	Expect bool   `yaml:"expect"`
	Why    string `yaml:"why"`
}

// TestYAMLCases runs every scenario in testdata.
// Each one crosses features that already have unit tests of their own, because the crossings are where they disagree,
// and writes its tuples as strings, which puts the dictionary on the path as well.
func TestYAMLCases(t *testing.T) {
	t.Parallel()

	files, err := fs.Glob(caseFS, "testdata/*.yaml")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}

	if len(files) == 0 {
		t.Fatal("no cases in testdata")
	}

	for _, name := range files {
		t.Run(path.Base(name), func(t *testing.T) {
			t.Parallel()

			runCaseFile(t, name)
		})
	}
}

func runCaseFile(t *testing.T, name string) {
	t.Helper()

	raw, err := caseFS.ReadFile(name)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	var cf caseFile

	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)

	if err := dec.Decode(&cf); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if cf.Name == "" {
		t.Fatal("the case has no name")
	}

	if len(cf.Checks) == 0 {
		t.Fatalf("%s: no checks; the case would pass without asking anything", cf.Name)
	}

	d := NewDictionary()
	schema := buildSchema(t, d, cf.Schema)
	src := loadTuples(t, d, cf.Tuples)

	engine, err := NewEngine(schema, src)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	for _, c := range cf.Checks {
		obj, rel, subj := parseTuple(t, d, c.Check)

		res, err := engine.Check(t.Context(), CheckRequest{
			Object: obj, Relation: rel, Subject: subj, Proof: ProofJustify,
		})
		if err != nil {
			t.Errorf("%s: %s: %v", cf.Name, c.Check, err)

			continue
		}

		if res.Allowed != c.Expect {
			t.Errorf("%s\n  %s = %v; want %v (%s)\n%s",
				cf.Name, c.Check, res.Allowed, c.Expect, c.Why, formatProof(res.Proof))
		}

		if len(res.Proof.Nodes) == 0 {
			t.Errorf("%s: %s: the check recorded no proof", cf.Name, c.Check)

			continue
		}

		// Settling can raise the answer after the root node was first closed, so the two
		// have to be read back together.
		if root := res.Proof.Nodes[0].Outcome; root != outcomeOf(res.Allowed) {
			t.Errorf("%s\n  %s: Allowed = %v but the root node says %s\n%s",
				cf.Name, c.Check, res.Allowed, root, formatProof(res.Proof))
		}
	}
}

func outcomeOf(allowed bool) Outcome {
	if allowed {
		return OutcomeAllowed
	}

	return OutcomeDenied
}

func buildSchema(t *testing.T, d *Dictionary, defs map[string]typeDef) *Schema {
	t.Helper()

	b := NewSchemaBuilder()

	for typName, def := range defs {
		typ := internType(t, d, typName)

		for relName, accepted := range def.Relations {
			rel := internRelation(t, d, relName)

			b.Relation(RelationRef{Type: typ, Relation: rel}, toSubjectTypes(t, d, accepted)...)
		}

		for relName, body := range def.Permissions {
			rel := internRelation(t, d, relName)

			b.Permission(RelationRef{Type: typ, Relation: rel}, toRewrite(t, d, body))
		}
	}

	s, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	return s
}

func toSubjectTypes(t *testing.T, d *Dictionary, accepted []string) []SubjectType {
	t.Helper()

	out := make([]SubjectType, 0, len(accepted))
	for _, s := range accepted {
		out = append(out, toSubjectType(t, d, s))
	}

	return out
}

// toSubjectType reads one accepted subject: "user", "user:*", or "team#member".
func toSubjectType(t *testing.T, d *Dictionary, s string) SubjectType {
	t.Helper()

	if typName, relName, ok := strings.Cut(s, "#"); ok {
		return UsersetType(internType(t, d, typName), internRelation(t, d, relName))
	}

	if typName, ok := strings.CutSuffix(s, ":"+WildcardMarker); ok {
		return WildcardType(internType(t, d, typName))
	}

	return DirectType(internType(t, d, s))
}

// Validating before interning matters: ids are never reused, so an illegal name interned
// here would hold its id for the life of the dictionary.
func internType(t *testing.T, d *Dictionary, name string) TypeID {
	t.Helper()

	if !ValidType(name) {
		t.Fatalf("type %q is not a legal identifier", name)
	}

	typ, err := d.InternType(name)
	if err != nil {
		t.Fatalf("intern type %q: %v", name, err)
	}

	return typ
}

// toRewrite reads the case format: a bare relation name is a leaf, and any/all/exclude are the combinators.
// A permission cannot read stored tuples, so there is no "this" to write.
func toRewrite(t *testing.T, d *Dictionary, v any) Rewrite {
	t.Helper()

	switch node := v.(type) {
	case string:
		return ComputedUserset(internRelation(t, d, node))

	case map[string]any:
		if len(node) != 1 {
			t.Fatalf("a rewrite takes exactly one operator, got %d: %v", len(node), node)
		}

		for op, body := range node {
			switch op {
			case "any":
				return Union(toRewrites(t, d, body)...)
			case "all":
				return Intersection(toRewrites(t, d, body)...)
			case "exclude":
				parts, ok := body.(map[string]any)
				if !ok {
					t.Fatalf("exclude takes base and subtract, got %v", body)
				}

				return Exclusion(toRewrite(t, d, parts["base"]), toRewrite(t, d, parts["subtract"]))
			default:
				t.Fatalf("unknown operator %q", op)
			}
		}
	}

	t.Fatalf("cannot read rewrite %v (%T)", v, v)

	return Rewrite{}
}

func internRelation(t *testing.T, d *Dictionary, name string) RelationID {
	t.Helper()

	if !ValidRelation(name) {
		t.Fatalf("relation %q is not a legal identifier", name)
	}

	rel, err := d.InternRelation(name)
	if err != nil {
		t.Fatalf("intern relation %q: %v", name, err)
	}

	return rel
}

func toRewrites(t *testing.T, d *Dictionary, v any) []Rewrite {
	t.Helper()

	items, ok := v.([]any)
	if !ok {
		t.Fatalf("expected a list of operands, got %v", v)
	}

	out := make([]Rewrite, 0, len(items))
	for _, item := range items {
		out = append(out, toRewrite(t, d, item))
	}

	return out
}

func loadTuples(t *testing.T, d *Dictionary, lines []string) *stubSource {
	t.Helper()

	b := store()

	for _, line := range lines {
		obj, rel, subj := parseTuple(t, d, line)
		b.add(obj, rel, subj)
	}

	return b.source()
}

// parseTuple reads "type:id#relation@subject", the form FormatTuple writes.
// Identifiers exclude ':' '#' and '@', so the first occurrence of each is the separator.
func parseTuple(t *testing.T, d *Dictionary, s string) (ObjectRef, RelationID, SubjectRef) {
	t.Helper()

	at := strings.Index(s, "@")
	if at < 0 {
		t.Fatalf("%q has no subject", s)
	}

	left, right := s[:at], s[at+1:]

	hash := strings.Index(left, "#")
	if hash < 0 {
		t.Fatalf("%q has no relation", s)
	}

	objType, objID := splitRef(t, left[:hash])
	relName := left[hash+1:]

	subjType, subjID, subjRel := splitSubject(t, right)

	if !ValidObject(objType, objID) {
		t.Fatalf("%q: %s:%s is not a legal resource", s, objType, objID)
	}

	if !ValidRelation(relName) {
		t.Fatalf("%q: %s is not a legal relation", s, relName)
	}

	if !ValidSubject(subjType, subjID, subjRel) {
		t.Fatalf("%q: the subject is not legal", s)
	}

	obj, err := d.InternObjectRef(objType, objID)
	if err != nil {
		t.Fatalf("%q: %v", s, err)
	}

	rel, err := d.InternRelation(relName)
	if err != nil {
		t.Fatalf("%q: %v", s, err)
	}

	subj, err := d.InternSubjectRef(subjType, subjID, subjRel)
	if err != nil {
		t.Fatalf("%q: %v", s, err)
	}

	return obj, rel, subj
}

func splitRef(t *testing.T, s string) (typ, id string) {
	t.Helper()

	colon := strings.Index(s, ":")
	if colon < 0 {
		t.Fatalf("%q is not type:id", s)
	}

	return s[:colon], s[colon+1:]
}

func splitSubject(t *testing.T, s string) (typ, id, rel string) {
	t.Helper()

	if hash := strings.Index(s, "#"); hash >= 0 {
		typ, id = splitRef(t, s[:hash])

		return typ, id, s[hash+1:]
	}

	typ, id = splitRef(t, s)

	return typ, id, ""
}

// The case format writes tuples the way FormatTuple does, so a round trip has to
// come back unchanged or the cases are describing something else.
func TestYAMLTupleFormRoundTrips(t *testing.T) {
	t.Parallel()

	d := NewDictionary()

	for _, want := range []string{
		"document:d1#viewer@user:alice",
		"document:d1#viewer@team:eng#member",
		"document:d1#viewer@user:*",
	} {
		obj, rel, subj := parseTuple(t, d, want)

		if got := d.FormatTuple(Tuple{Object: obj, Relation: rel, Subject: subj}); got != want {
			t.Errorf("round trip = %q; want %q", got, want)
		}
	}
}
