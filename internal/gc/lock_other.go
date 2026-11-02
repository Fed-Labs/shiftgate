//go:build !linux

package gc

import "errors"

func acquireStateLock(string) (*StateLock, error) {
	return nil, errors.New("offline GC locking is supported only on Linux")
}
