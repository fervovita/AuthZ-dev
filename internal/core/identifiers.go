package core

// WildcardMarker is a subject id meaning every subject of the type: the "*" in user:*.
const WildcardMarker = "*"

const (
	maxTypeLen     = 64
	maxRelationLen = 64
	maxObjectIDLen = 1024
)

// ValidObject reports whether typ and id may name a resource, as in document:d1.
func ValidObject(typ, id string) bool {
	return ValidType(typ) && validObjectID(id)
}

// ValidSubject reports whether typ, id and relation may name a subject.
// Two forms are legal only here: an empty relation, meaning the subject names an object
// directly, and an id of WildcardMarker, which no userset may carry.
func ValidSubject(typ, id, relation string) bool {
	if !ValidType(typ) {
		return false
	}

	if id == WildcardMarker {
		return relation == ""
	}

	return validObjectID(id) && (relation == "" || ValidRelation(relation))
}

// ValidType reports whether s may name an object type.
func ValidType(s string) bool {
	return validDSLName(s, maxTypeLen)
}

// ValidRelation reports whether s may name a relation or permission.
func ValidRelation(s string) bool {
	return validDSLName(s, maxRelationLen)
}

// validObjectID is unexported because the rules differ by position: callers use ValidObject or ValidSubject.
func validObjectID(s string) bool {
	if s == "" || len(s) > maxObjectIDLen {
		return false
	}

	for i := range len(s) {
		if !objectIDByte(s[i]) {
			return false
		}
	}

	return true
}

// validDSLName matches ^[a-z][a-z0-9_]*$ within maxLen bytes.
func validDSLName(s string, maxLen int) bool {
	if s == "" || len(s) > maxLen {
		return false
	}

	if s[0] < 'a' || s[0] > 'z' {
		return false
	}

	for i := 1; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}

	return true
}

// objectIDByte matches [a-zA-Z0-9_.\-/=+].
func objectIDByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z',
		c >= 'A' && c <= 'Z',
		c >= '0' && c <= '9':
		return true
	}

	switch c {
	case '_', '.', '-', '/', '=', '+':
		return true
	}

	return false
}
