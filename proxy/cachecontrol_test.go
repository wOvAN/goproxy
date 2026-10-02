// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package proxy

import "testing"

func TestCacheControlFor(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/github.com/x/y/@v/list", "public, max-age=300"},
		{"/github.com/x/y/@latest", "public, max-age=300"},
		{"/github.com/x/y/@v/v1.0.0.info", "public, max-age=604800"},
		{"/github.com/x/y/@v/v1.0.0.mod", "public, max-age=604800"},
		{"/github.com/x/y/@v/v1.0.0.zip", "public, max-age=604800"},
		{"/sumdb/sum.golang.org/supported", "no-store"},
		{"/sumdb/sum.golang.org/latest", "public, max-age=60"},
		{"/sumdb/sum.golang.org/tile/0/000", "public, max-age=604800"},
		{"/sumdb/sum.golang.org/lookup/github.com/x/y", "public, max-age=604800"},
		{"/sumdb/sum.golang.org", ""},
		{"/", ""},
		{"/notamodule", ""},
	}
	for _, tc := range tests {
		if got := cacheControlFor(tc.path); got != tc.want {
			t.Errorf("cacheControlFor(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}
