package turnagent

import "fmt"

// errMissing returns a validation error for a missing required Config field.
func errMissing(field string) error {
	return fmt.Errorf("config field %q is required", field)
}
