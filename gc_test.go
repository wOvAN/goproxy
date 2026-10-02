// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSweepCache(t *testing.T) {
	probe := writeTestFile(t, t.TempDir(), "probe")
	info, err := os.Lstat(probe)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := atime(info); !ok {
		t.Skip("platform does not report access times")
	}

	root := t.TempDir()
	old := writeTestFile(t, root, filepath.Join("github.com", "x", "y", "@v", "v1.0.0.zip"))
	kept := writeTestFile(t, root, filepath.Join("github.com", "x", "y", "@v", "v2.0.0.zip"))
	sumdbOld := writeTestFile(t, root, filepath.Join("sumdb", "sum.golang.org", "lookup", "github.com/x/y"))

	oldTime := time.Now().Add(-48 * time.Hour)
	for _, f := range []string{old, sumdbOld} {
		if err := os.Chtimes(f, oldTime, oldTime); err != nil {
			t.Skipf("cannot set atime: %v", err)
		}
	}

	files, bytes, dirs := sweepCache(root, 24*time.Hour)
	if files != 2 {
		t.Errorf("removed files = %d, want 2", files)
	}
	if bytes == 0 {
		t.Error("bytes freed = 0, want > 0")
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("stale file must be removed")
	}
	if _, err := os.Stat(sumdbOld); !os.IsNotExist(err) {
		t.Error("stale sumdb entry must be removed")
	}
	if _, err := os.Stat(kept); err != nil {
		t.Errorf("fresh file removed: %v", err)
	}
	// Emptied dirs are pruned up to the module dir; kept's parents survive.
	if _, err := os.Stat(filepath.Join(root, "sumdb")); !os.IsNotExist(err) {
		t.Error("emptied sumdb tree must be pruned")
	}
	if _, err := os.Stat(filepath.Join(root, "github.com", "x", "y", "@v")); err != nil {
		t.Error("directory with live files must survive pruning")
	}
	if dirs == 0 {
		t.Error("emptied dirs pruned = 0, want > 0")
	}
	// Root itself is never removed.
	if _, err := os.Stat(root); err != nil {
		t.Error("root must never be removed")
	}
}

func TestSweepCacheEmpty(t *testing.T) {
	if files, _, dirs := sweepCache("", time.Hour); files != 0 || dirs != 0 {
		t.Error("empty root must be a no-op")
	}
}

func writeTestFile(t *testing.T, root, rel string) string {
	t.Helper()
	file := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(file), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("data"), 0o666); err != nil {
		t.Fatal(err)
	}
	return file
}
