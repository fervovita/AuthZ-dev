package core

import (
	"strings"
	"testing"
)

func TestValidType(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
		want  bool
	}{
		{"plain", "user", true},
		{"short", "db", true},
		{"underscore", "user_v2", true},
		{"digits", "user2", true},
		{"max length", strings.Repeat("a", maxTypeLen), true},
		{"empty", "", false},
		{"over length", strings.Repeat("a", maxTypeLen+1), false},
		{"uppercase", "User", false},
		{"leading digit", "1user", false},
		{"hyphen", "user-v2", false},
		{"bad at second", "u-ser", false},
		{"bad at last", "user-", false},
		{"delimiter", "user:v2", false},
		{"non-ascii", "유저", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			if got := ValidType(c.input); got != c.want {
				t.Errorf("ValidType(%q) = %v; want %v", c.input, got, c.want)
			}
		})
	}
}

func TestValidRelation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
		want  bool
	}{
		{"plain", "viewer", true},
		{"underscore", "can_view", true},
		{"max length", strings.Repeat("a", maxRelationLen), true},
		{"empty", "", false},
		{"over length", strings.Repeat("a", maxRelationLen+1), false},
		{"uppercase", "Viewer", false},
		{"ellipsis", "...", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			if got := ValidRelation(c.input); got != c.want {
				t.Errorf("ValidRelation(%q) = %v; want %v", c.input, got, c.want)
			}
		})
	}
}

func TestObjectIDSyntax(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
		want  bool
	}{
		{"plain", "d1", true},
		{"uuid", "550e8400-e29b-41d4-a716-446655440000", true},
		{"mixed case", "DocumentA", true},
		{"dotted", "com.example.resource", true},
		{"slash", "tenant/a", true},
		{"base64url", "YWxpY2VAZXhhbXBsZS5jb20=", true},
		{"max length", strings.Repeat("a", maxObjectIDLen), true},
		{"empty", "", false},
		{"over length", strings.Repeat("a", maxObjectIDLen+1), false},
		{"email", "alice@example.com", false},
		{"colon", "a:b", false},
		{"bad at last", "ab:", false},
		{"hash", "a#b", false},
		{"pipe", "a|b", false},
		{"wildcard marker", "*", false},
		{"star inside", "a*b", false},
		{"space", "a b", false},
		{"newline", "a\nb", false},
		{"non-ascii", "문서1", false},
		{"invalid utf-8", "a\xffb", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			if got := validObjectID(c.input); got != c.want {
				t.Errorf("validObjectID(%q) = %v; want %v", c.input, got, c.want)
			}
		})
	}
}

// The two positions do not share rules: only a subject may be the wildcard, and only a subject's relation may be empty.
func TestValidObjectAndValidSubject(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		typ, id, rel string
		object       bool
		subject      bool
	}{
		{"direct subject", "user", "alice", "", true, true},
		{"userset", "team", "eng", "member", true, true},
		{"wildcard", "user", WildcardMarker, "", false, true},
		{"wildcard with relation", "user", WildcardMarker, "member", false, false},
		{"bad type", "User", "alice", "", false, false},
		{"bad id", "user", "a b", "", false, false},
		{"bad relation", "user", "alice", "Bad", true, false},
		{"empty type", "", "alice", "", false, false},
		{"empty id", "user", "", "", false, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			if got := ValidObject(c.typ, c.id); got != c.object {
				t.Errorf("ValidObject(%q, %q) = %v; want %v", c.typ, c.id, got, c.object)
			}

			if got := ValidSubject(c.typ, c.id, c.rel); got != c.subject {
				t.Errorf("ValidSubject(%q, %q, %q) = %v; want %v", c.typ, c.id, c.rel, got, c.subject)
			}
		})
	}
}

// A syntax check that accepted what the reference rejects would send the loader chasing a phantom.
func TestValidSubjectAgreesWithSubjectRefValid(t *testing.T) {
	t.Parallel()

	d := NewDictionary()

	for _, c := range []struct{ id, rel string }{
		{"alice", ""}, {"eng", "member"}, {WildcardMarker, ""}, {WildcardMarker, "member"},
	} {
		subj, err := d.InternSubjectRef("user", c.id, c.rel)
		if err != nil {
			t.Fatalf("InternSubjectRef: %v", err)
		}

		if got, want := ValidSubject("user", c.id, c.rel), subj.Valid(); got != want {
			t.Errorf("user:%s#%s: ValidSubject = %v but SubjectRef.Valid = %v", c.id, c.rel, got, want)
		}
	}
}
