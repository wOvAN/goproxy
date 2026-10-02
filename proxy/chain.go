// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package proxy

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// An upstreamStep is one proxy URL of a GOPROXY-style -proxy chain.
type upstreamStep struct {
	u *url.URL
	// fallBackOnError corresponds to the "|" separator: the next step is
	// tried on any failure; after "," the next step is tried only when this
	// one answers not-exist (404/410).
	fallBackOnError bool
}

// parseProxyChain parses a GOPROXY-style proxy chain, e.g.
// "https://a.example,https://b.example|direct,off": comma-separated proxy
// URLs fall through to the next entry only on a not-exist answer, "|"
// entries fall through on any failure, and the chain may end with "direct"
// (fall back to the local go command) or "off" (stop, answer from the cache
// or with the last upstream failure).
//
// The semantics mirror the go command's GOPROXY list handling.
func parseProxyChain(s string) (steps []upstreamStep, tail string, err error) {
	for s != "" {
		var tok, sep string
		if i := strings.IndexAny(s, ",|"); i >= 0 {
			tok, sep, s = s[:i], s[i:i+1], s[i+1:]
			if s == "" {
				sep = "" // a trailing separator has no next step
			}
		} else {
			tok, s = s, ""
		}
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		if tok == "direct" || tok == "off" {
			if s != "" {
				return nil, "", fmt.Errorf("proxy chain: %q must be the last entry", tok)
			}
			return steps, tok, nil
		}
		u, err := url.Parse(tok)
		if err != nil || u.Host == "" {
			return nil, "", fmt.Errorf("proxy chain: invalid proxy URL %q", tok)
		}
		steps = append(steps, upstreamStep{u: u, fallBackOnError: sep == "|"})
	}
	if len(steps) == 0 {
		return nil, "", errors.New("proxy chain: contains no proxy URLs")
	}
	return steps, "", nil
}
