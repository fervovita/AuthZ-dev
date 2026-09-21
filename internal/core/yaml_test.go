package core_test

import (
	"bytes"
	"embed"
	"io/fs"
	"path"
	"testing"

	yaml "go.yaml.in/yaml/v3"

	"github.com/fervovita/AuthZ-dev/internal/core"
	"github.com/fervovita/AuthZ-dev/internal/schema"
)

// The cases are compiled in, so running the suite touches no files and an empty
// testdata is a build failure rather than a silently empty run.
//
//go:embed testdata/*.yaml
var caseFS embed.FS

// caseFile is one scenario: a schema in the language, the tuples stored under it, and the checks
// that schema is supposed to answer a certain way.
type caseFile struct {
	Name   string      `yaml:"name"`
	Schema string      `yaml:"schema"`
	Tuples []string    `yaml:"tuples"`
	Checks []checkCase `yaml:"checks"`
}

type checkCase struct {
	Check  string `yaml:"check"`
	Expect bool   `yaml:"expect"`
	Why    string `yaml:"why"`
}

// TestYAMLCases runs every scenario in testdata.
// Each one crosses features that already have unit tests of their own, because the crossings are where they disagree,
// and writes its schema and tuples as text, which puts the schema language and the dictionary on the path as well.
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

	d := core.NewDictionary()

	// The label says "schema" because the lines it counts are the block's, not the file's.
	s, err := schema.Compile(path.Base(name)+" schema", cf.Schema, d)
	if err != nil {
		t.Fatalf("schema: %v", err)
	}

	engine, err := core.NewEngine(s, core.LoadTuples(t, d, cf.Tuples))
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	for _, c := range cf.Checks {
		obj, rel, subj := core.ParseTuple(t, d, c.Check)

		res, err := engine.Check(t.Context(), core.CheckRequest{
			Object: obj, Relation: rel, Subject: subj, Proof: core.ProofJustify,
		})
		if err != nil {
			t.Errorf("%s: %s: %v", cf.Name, c.Check, err)

			continue
		}

		if res.Allowed != c.Expect {
			t.Errorf("%s\n  %s = %v; want %v (%s)\n%s",
				cf.Name, c.Check, res.Allowed, c.Expect, c.Why, core.FormatProof(res.Proof))
		}

		if len(res.Proof.Nodes) == 0 {
			t.Errorf("%s: %s: the check recorded no proof", cf.Name, c.Check)

			continue
		}

		// Settling can raise the answer after the root node was first closed, so the two
		// have to be read back together.
		if root := res.Proof.Nodes[0].Outcome; root != outcomeOf(res.Allowed) {
			t.Errorf("%s\n  %s: Allowed = %v but the root node says %s\n%s",
				cf.Name, c.Check, res.Allowed, root, core.FormatProof(res.Proof))
		}
	}
}

func outcomeOf(allowed bool) core.Outcome {
	if allowed {
		return core.OutcomeAllowed
	}

	return core.OutcomeDenied
}
