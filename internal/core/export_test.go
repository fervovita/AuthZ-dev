package core

// The scenario runner in yaml_test.go is package core_test, because it compiles each case's schema
// with internal/schema, which imports core. These hand it the helpers the package's own tests use.
var (
	LoadTuples  = loadTuples
	ParseTuple  = parseTuple
	FormatProof = formatProof
)
