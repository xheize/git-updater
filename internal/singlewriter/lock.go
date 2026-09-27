// Package singlewriter holds OS locks for the lifetime of a server. Locks are
// released on crash, unlike existence-based lock files with stale PID recovery.
package singlewriter

import (
	"fmt"
	"os"
	"path/filepath"
)

func Acquire(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := lock(f); err != nil {
		f.Close()
		return nil, fmt.Errorf("another controller owns %s: %w", path, err)
	}
	return func() { unlock(f); f.Close() }, nil
}
