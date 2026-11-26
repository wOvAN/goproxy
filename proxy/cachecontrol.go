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

// Cache-Control values for the various kinds of served module content.
// Module version files are content-addressed and immutable, so they get a
// long max age; list and @latest are volatile and get the list cache expiry.
// sumdb responses are owned by the sumdb handler: its paths start with a db
// name that can span multiple segments, so they cannot be parsed here.
func cacheControlFor(urlPath string) string {
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

// immutableExpire is the max age of content-addressed (immutable) module
// files: a week.
var immutableExpire = 7 * 24 * time.Hour

// publicMaxAge formats a Cache-Control value for the given duration.
func publicMaxAge(d time.Duration) string {
	return "public, max-age=" + strconv.Itoa(int(d.Seconds()))
}
