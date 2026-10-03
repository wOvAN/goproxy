// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package proxy

import (
	"path"
	"strconv"
	"strings"
	"time"
)

// Cache-Control values for the various kinds of served content.
// Module version files and sumdb tiles/lookups are content-addressed and
// immutable, so they get a long max age; list and @latest are volatile and
// get the list cache expiry; sumdb latest is volatile; supported is a
// probe endpoint and must never be cached.
func cacheControlFor(urlPath string) string {
	if after, ok := strings.CutPrefix(urlPath, "/sumdb/"); ok {
		// /sumdb/<db>/<what>
		rest := after
		_, after, ok := strings.Cut(rest, "/")
		if !ok {
			return ""
		}
		switch what := after; {
		case what == "supported":
			return "no-store"
		case what == "latest":
			return publicMaxAge(sumdbLatestAge)
		case strings.HasPrefix(what, "tile/"), strings.HasPrefix(what, "lookup/"):
			return publicMaxAge(immutableExpire)
		}
		return ""
	}
	_, after, ok := strings.Cut(urlPath, "/@")
	if !ok {
		return ""
	}
	what := after
	switch what {
	case "latest", "v/list":
		return publicMaxAge(ListExpire)
	}
	switch path.Ext(what) {
	case ".info", ".mod", ".zip":
		return publicMaxAge(immutableExpire)
	}
	return ""
}

// Max ages used by cacheControlFor. list/@latest expire with the router
// list cache; immutable (content-addressed) files are cached for a week.
var (
	immutableExpire = 7 * 24 * time.Hour
	sumdbLatestAge  = time.Minute
)

// publicMaxAge formats a Cache-Control value for the given duration.
func publicMaxAge(d time.Duration) string {
	return "public, max-age=" + strconv.Itoa(int(d.Seconds()))
}
