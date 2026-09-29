package main

import (
	"errors"
	"fmt"
	"io/fs"
	"testing"
)

func TestMapNotFound(t *testing.T) {
	notFound := fmt.Errorf("go: list -m -json -versions X@latest:\ngo: X@latest: no matching versions for query \"latest\"\n")
	if !errors.Is(mapNotFound(notFound), fs.ErrNotExist) {
		t.Error("go command not-found diagnostic must map to fs.ErrNotExist")
	}
	other := fmt.Errorf("go: mod download -json X@v1.0.0:\nexit status 1\n")
	if errors.Is(mapNotFound(other), fs.ErrNotExist) {
		t.Error("transient go command failure must not map to fs.ErrNotExist")
	}
}
