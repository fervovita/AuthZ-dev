package core

import (
	"strconv"
	"strings"
	"testing"
)

// formatProof renders a proof as an indented tree, for test failure output.
func formatProof(p Proof) string {
	var b strings.Builder

	if len(p.Nodes) == 0 {
		return "(empty)"
	}

	var walk func(idx, depth int)

	walk = func(idx, depth int) {
		n := p.Nodes[idx]

		b.WriteString(strings.Repeat("  ", depth))
		b.WriteString("[" + strconv.Itoa(idx) + "] " + n.Op.String() + " " + n.Outcome.String())

		if n.Memoized {
			b.WriteString(" (memoized)")
		}

		b.WriteString("\n")

		for _, c := range n.Children {
			walk(c, depth+1)
		}
	}

	walk(0, 0)

	if p.Truncated {
		b.WriteString("(truncated)\n")
	}

	return b.String()
}

// reachableNodes returns what a proof reaches by walking from the root, which is every
// recorded node unless a parent lost the link to a child it opened.
func reachableNodes(p Proof) map[int]bool {
	seen := map[int]bool{}

	if len(p.Nodes) == 0 {
		return seen
	}

	var walk func(idx int)

	walk = func(idx int) {
		if seen[idx] {
			return
		}

		seen[idx] = true

		for _, c := range p.Nodes[idx].Children {
			walk(c)
		}
	}

	walk(0)

	return seen
}

func TestRecorderRecordsTreeShape(t *testing.T) {
	t.Parallel()

	r := recorder{mode: ProofJustify}

	root := r.open(doc(oD1), rView, OpUnion)
	child := r.open(doc(oD1), rViewer, OpThis)

	r.close(child, false)
	r.addChild(root, child)
	r.close(root, true)

	p := r.proof()

	if len(p.Nodes) != 2 {
		t.Fatalf("got %d nodes; want 2", len(p.Nodes))
	}

	if p.Nodes[root].Outcome != OutcomeAllowed || p.Nodes[child].Outcome != OutcomeDenied {
		t.Errorf("outcomes = %s, %s", p.Nodes[root].Outcome, p.Nodes[child].Outcome)
	}

	if len(p.Nodes[root].Children) != 1 || p.Nodes[root].Children[0] != child {
		t.Errorf("root children = %v; want [%d]", p.Nodes[root].Children, child)
	}
}

// Children always point backwards, so a proof can be walked from the root without a second pass.
func TestRecorderChildrenPointBackwards(t *testing.T) {
	t.Parallel()

	r := recorder{mode: ProofJustify}

	root := r.open(doc(oD1), rView, OpUnion)

	for range 3 {
		child := r.open(doc(oD1), rViewer, OpThis)
		r.close(child, false)
		r.addChild(root, child)
	}

	r.close(root, false)

	for i, n := range r.proof().Nodes {
		for _, c := range n.Children {
			if c <= i {
				t.Errorf("node %d has child %d; children come after their parent", i, c)
			}
		}
	}
}

func TestRecorderStopsAtMaxProofNodes(t *testing.T) {
	t.Parallel()

	r := recorder{mode: ProofJustify}

	for range MaxProofNodes {
		if idx := r.open(doc(oD1), rViewer, OpThis); idx == noNode {
			t.Fatal("ran out of room before the limit")
		}
	}

	if r.truncated {
		t.Error("truncated before exceeding the limit")
	}

	if idx := r.open(doc(oD1), rViewer, OpThis); idx != noNode {
		t.Errorf("open returned %d past the limit; want noNode", idx)
	}

	p := r.proof()

	if !p.Truncated {
		t.Error("Truncated = false after passing the limit")
	}

	if len(p.Nodes) != MaxProofNodes {
		t.Errorf("got %d nodes; want %d", len(p.Nodes), MaxProofNodes)
	}
}

// With collection off every operation is a no-op, so the evaluator can call them unconditionally.
func TestRecorderDisabledIsInert(t *testing.T) {
	t.Parallel()

	r := recorder{mode: ProofNone}

	idx := r.open(doc(oD1), rViewer, OpThis)
	if idx != noNode {
		t.Fatalf("open = %d; want noNode", idx)
	}

	r.close(idx, true)
	r.addChild(idx, idx)

	if p := r.proof(); len(p.Nodes) != 0 || p.Truncated {
		t.Errorf("recorded %d nodes with collection off", len(p.Nodes))
	}
}

func TestOutcomeString(t *testing.T) {
	t.Parallel()

	cases := map[Outcome]string{
		OutcomeIncomplete: "incomplete",
		OutcomeAllowed:    "allowed",
		OutcomeDenied:     "denied",
		OutcomeCycle:      "cycle",
		Outcome(9):        "outcome(9)",
	}

	for o, want := range cases {
		if got := o.String(); got != want {
			t.Errorf("Outcome(%d).String() = %q; want %q", o, got, want)
		}
	}
}

func TestRecorderLeafIsInertWhenDisabled(t *testing.T) {
	t.Parallel()

	r := recorder{mode: ProofNone}

	if idx := r.leaf(doc(oD1), rViewer, OpThis, OutcomeCycle, true); idx != noNode {
		t.Errorf("leaf = %d; want noNode with collection off", idx)
	}

	if len(r.proof().Nodes) != 0 {
		t.Error("leaf recorded a node with collection off")
	}
}
