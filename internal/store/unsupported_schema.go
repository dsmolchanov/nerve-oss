package store

import "fmt"

// UnsupportedSchemaError reports an operation that requires a Core migration
// the live schema has not reached. Artifact B spans Core [28,29]; autonomous
// provider-fence operations exist only at Core 29 and above.
type UnsupportedSchemaError struct {
	Operation    string
	RequiresCore int64
}

func (e *UnsupportedSchemaError) Error() string {
	return fmt.Sprintf("%s requires core schema %d", e.Operation, e.RequiresCore)
}
