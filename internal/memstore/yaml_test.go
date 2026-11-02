package memstore

import (
	"bytes"
	"io/fs"
	"os"
	"testing"

	yaml "go.yaml.in/yaml/v3"
)

// TestYAMLCases answers the evaluator's scenarios from a replica instead of core's test map:
// the same schema, tuples and expectations, reached through names as a sidecar's caller sends them.
func TestYAMLCases(t *testing.T) {
	t.Parallel()

	cases := os.DirFS("../core/testdata")

	files, err := fs.Glob(cases, "*.yaml")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}

	if len(files) == 0 {
		t.Fatal("no cases in ../core/testdata")
	}

	for _, name := range files {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			raw, err := fs.ReadFile(cases, name)
			if err != nil {
				t.Fatalf("read: %v", err)
			}

			// The fields core's runner reads; KnownFields fails this test when the format grows.
			var cf struct {
				Name   string   `yaml:"name"`
				Schema string   `yaml:"schema"`
				Tuples []string `yaml:"tuples"`
				Checks []struct {
					Check  string `yaml:"check"`
					Expect bool   `yaml:"expect"`
					Why    string `yaml:"why"`
				} `yaml:"checks"`
			}

			dec := yaml.NewDecoder(bytes.NewReader(raw))
			dec.KnownFields(true)

			if err := dec.Decode(&cf); err != nil {
				t.Fatalf("decode: %v", err)
			}

			var s Store

			reset(t, &s, cf.Schema, cf.Tuples...)

			for _, c := range cf.Checks {
				got, err := s.Check(t.Context(), checkOf(t, c.Check))
				if err != nil {
					t.Errorf("%s: %s: %v", cf.Name, c.Check, err)

					continue
				}

				if got != c.Expect {
					t.Errorf("%s\n  %s = %v; want %v (%s)", cf.Name, c.Check, got, c.Expect, c.Why)
				}
			}
		})
	}
}
