package persistence

import "errors"

// ErrNotFound reports that a durable record does not exist. It is the
// sentinel callers compare against to tell "no such record" from "storage
// failed".
var ErrNotFound = errors.New("record not found")
