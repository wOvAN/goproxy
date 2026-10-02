// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build windows

package main

import (
	"io/fs"
	"syscall"
	"time"
)

// atime returns the last access time of a file. Windows reports
// last-access times only when the filesystem tracks them; files without a
// usable access time are reported as not ok and skipped by the sweep.
func atime(info fs.FileInfo) (time.Time, bool) {
	st, ok := info.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return time.Time{}, false
	}
	return time.Unix(0, st.LastAccessTime.Nanoseconds()), true
}
