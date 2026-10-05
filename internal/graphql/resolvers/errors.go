package resolvers

import "errors"

// errInvalid is returned by resolvers when an input variable is malformed.
// The dispatcher renders this into the GraphQL `errors` array.
func errInvalid(msg string) error { return errors.New("invalid: " + msg) }
