package core

import "strconv"

// MaxProofNodes bounds a proof so a wide fan-out check cannot grow one without limit.
const MaxProofNodes = 256

// noNode marks a node that was not recorded, because collection is off or the proof is full.
const noNode = -1

// ProofMode selects how much of a check is recorded.
type ProofMode uint8

// ProofMode values.
const (
	ProofNone    ProofMode = iota // record nothing
	ProofJustify                  // record what justifies the outcome
)

// Outcome is what an evaluated node concluded.
type Outcome uint8

// Outcome values.
const (
	OutcomeIncomplete Outcome = iota // evaluation stopped before this node concluded
	OutcomeAllowed
	OutcomeDenied
	OutcomeCycle // denied because the node was already being evaluated
)

func (o Outcome) String() string {
	switch o {
	case OutcomeIncomplete:
		return "incomplete"
	case OutcomeAllowed:
		return "allowed"
	case OutcomeDenied:
		return "denied"
	case OutcomeCycle:
		return "cycle"
	default:
		return "outcome(" + strconv.Itoa(int(o)) + ")"
	}
}

// ProofNode is one evaluated rewrite. Children index into "Proof.Nodes".
type ProofNode struct {
	Object   ObjectRef
	Relation RelationID
	Op       Op
	Outcome  Outcome
	Memoized bool // answered from an earlier node of the same check
	Children []int
}

// Proof is why a check decided as it did. Nodes[0] is the root.
type Proof struct {
	Subject   SubjectRef
	Nodes     []ProofNode
	Truncated bool
}

// recorder builds a Proof during one check.
// Nodes are appended as they are opened, so a child's index always sits above its parent's.
type recorder struct {
	mode      ProofMode
	subject   SubjectRef
	nodes     []ProofNode
	truncated bool
	paused    int
}

func (r *recorder) enabled() bool {
	return r.mode != ProofNone && r.paused == 0
}

// pause stops recording for the rounds that settle a recursive component:
// they re-walk ground the traversal already covered, and have no parent node to hang from.
func (r *recorder) pause() { r.paused++ }

func (r *recorder) resume() { r.paused-- }

// open appends a node with no outcome yet and returns its index.
func (r *recorder) open(obj ObjectRef, rel RelationID, op Op) int {
	if !r.enabled() {
		return noNode
	}

	if len(r.nodes) >= MaxProofNodes {
		r.truncated = true

		return noNode
	}

	r.nodes = append(r.nodes, ProofNode{Object: obj, Relation: rel, Op: op})

	return len(r.nodes) - 1
}

// leaf records a node that has no evaluation underneath it.
func (r *recorder) leaf(obj ObjectRef, rel RelationID, op Op, outcome Outcome, memoized bool) int {
	idx := r.open(obj, rel, op)
	if idx == noNode {
		return noNode
	}

	r.nodes[idx].Outcome = outcome
	r.nodes[idx].Memoized = memoized

	return idx
}

func (r *recorder) close(idx int, allowed bool) {
	if idx == noNode {
		return
	}

	if allowed {
		r.nodes[idx].Outcome = OutcomeAllowed

		return
	}

	r.nodes[idx].Outcome = OutcomeDenied
}

func (r *recorder) addChild(parent, child int) {
	if parent == noNode || child == noNode {
		return
	}

	r.nodes[parent].Children = append(r.nodes[parent].Children, child)
}

func (r *recorder) proof() Proof {
	return Proof{Subject: r.subject, Nodes: r.nodes, Truncated: r.truncated}
}
