package singlewriter

import (
	"path/filepath"
	"testing"
)

func TestExclusiveOwnershipAndRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "writer.lock")
	release, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	if other, err := Acquire(path); err == nil {
		other()
		t.Fatal("second writer admitted")
	}
	release()
	release, err = Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	release()
}
