// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// runCacheGC periodically sweeps the module download cache, removing files
// whose last access time is older than gcKeep and the directories they
// emptied. It runs only when gcInterval > 0.
//
// The sweep is best-effort and safe to run against a live cache: it only
// removes files untouched for gcKeep (so an in-flight fetch, whose files
// were just written, is never a candidate; on Windows a file held open
// simply fails to delete and is retried next sweep), and os.Remove only
// deletes empty directories.
func runCacheGC() {
	log.Printf("cache gc: interval %s, keep %s\n", gcInterval, gcKeep)
	for range time.NewTicker(gcInterval).C {
		files, bytes, dirs := sweepCache(downloadRoot, gcKeep)
		if files > 0 || dirs > 0 {
			log.Printf("cache gc: removed %d file(s) (%d bytes), %d dir(s)\n", files, bytes, dirs)
		}
	}
}

// sweepCache removes cache files last accessed before now-keep, then prunes
// the directories that became empty. It returns the counts of removed files
// and directories and the bytes freed.
func sweepCache(root string, keep time.Duration) (files int, bytes int64, dirs int) {
	if root == "" {
		return 0, 0, 0
	}
	cutoff := time.Now().Add(-keep)
	var seenDirs []string
	//nolint:errcheck // best-effort sweep: per-entry errors are skipped
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != root {
				seenDirs = append(seenDirs, p)
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		at, ok := atime(info)
		if !ok {
			return nil
		}
		if !at.Before(cutoff) {
			return nil
		}
		bytes += info.Size()
		if err := os.Remove(p); err == nil {
			files++
		}
		return nil
	})
	// Prune deepest first so a chain of emptied dirs removes bottom-up.
	// Under one root, path length orders siblings by depth. os.Remove
	// fails (no-op) on non-empty dirs, so nothing live is touched.
	sort.Slice(seenDirs, func(i, j int) bool { return len(seenDirs[i]) > len(seenDirs[j]) })
	for _, d := range seenDirs {
		if err := os.Remove(d); err == nil {
			dirs++
		}
	}
	return files, bytes, dirs
}
