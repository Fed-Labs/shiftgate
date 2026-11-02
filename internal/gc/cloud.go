package gc

import (
	"context"

	"shift.dev/shift/internal/objectstore"
)

// CloudSweep intentionally fails closed when the prefix may be written by more
// than one agent. The local offline lock cannot coordinate independent agents,
// so deleting shared cloud chunks here would be unsafe. A future deployment may
// pass multiAgent=false only when it has an external distributed lock.
func CloudSweep(ctx context.Context, store objectstore.Store, prefix string, multiAgent bool) error {
	if store == nil {
		return ErrSharedCloudPrefixUncoordinated
	}
	if multiAgent || prefix == "" {
		return ErrSharedCloudPrefixUncoordinated
	}
	return ErrSharedCloudPrefixUncoordinated
}
