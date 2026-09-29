// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package sumdb implements sumdb handler proxy.
package sumdb

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandler(t *testing.T) {
	if ret := t.Run("supported", testSupported); !ret {
		t.Error("supported test failed, stop test")
		t.FailNow()
	}
	t.Run("proxy", testProxy)
}

func testSupported(t *testing.T) {
	type TestCase struct {
		name          string
		db            string
		wantSupported bool
	}

	tests := []TestCase{
		{
			name:          "sum.golang.org",
			db:            "sum.golang.org",
			wantSupported: true,
		},
		{
			name:          "gosum.io",
			db:            "gosum.io",
			wantSupported: true,
		},
		{
			name:          "sum.golang.google.cn",
			db:            "sum.golang.google.cn",
			wantSupported: true,
		},
		{
			name:          "other",
			db:            "other",
			wantSupported: false,
		},
	}

	for _, testcase := range tests {
		t.Run(testcase.name, func(t *testing.T) {
			recoder := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("https://goproxy.io/sumdb/%s/supported", testcase.db), nil)
			Handler(recoder, req)

			resp := recoder.Result()
			if support := (resp.StatusCode == http.StatusOK); support != testcase.wantSupported {
				t.Errorf("db %s: want %v got %v", testcase.db, testcase.wantSupported, support)
			}
			_ = resp.Body.Close()
		})
	}
}

func testProxy(t *testing.T) {
	type TestCase struct {
		name       string
		db         string
		path       string
		expectSucc bool
	}
	tests := []TestCase{
		{
			name:       "lookup",
			db:         "sum.golang.google.cn",
			path:       "lookup/github.com/goproxyio/goproxy@v1.0.0", // this is a fake testcase
			expectSucc: true,
		},
	}

	for _, testcase := range tests {
		t.Run(testcase.name, func(t *testing.T) {

			recoder := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("https://goproxy.io/sumdb/%s/%s", testcase.db, testcase.path), nil)
			Handler(recoder, req)

			resp := recoder.Result()
			if succ := (resp.StatusCode == http.StatusOK); succ != testcase.expectSucc {
				t.Errorf("FETCH from db %s/%s got unexpect http status %d", testcase.db, testcase.path, resp.StatusCode)
				return
			}
		})
	}
}

func TestParsePath(t *testing.T) {
	type TestCase struct {
		name        string
		rawPath     string
		wantWhichDB string
		wantPath    string
		wantErr     bool
	}

	tests := []TestCase{
		{
			name:        "valid path with sum.golang.org",
			rawPath:     "/sumdb/sum.golang.org/supported",
			wantWhichDB: "sum.golang.org",
			wantPath:    "supported",
			wantErr:     false,
		},
		{
			name:        "valid path with lookup",
			rawPath:     "/sumdb/sum.golang.org/lookup/github.com/test@v1.0.0",
			wantWhichDB: "sum.golang.org",
			wantPath:    "lookup/github.com/test@v1.0.0",
			wantErr:     false,
		},
		{
			name:        "valid path with gosum.io",
			rawPath:     "/sumdb/gosum.io/supported",
			wantWhichDB: "gosum.io",
			wantPath:    "supported",
			wantErr:     false,
		},
		{
			name:    "invalid path - too few parts",
			rawPath: "/sumdb/sum.golang.org",
			wantErr: true,
		},
		{
			name:    "invalid path - empty",
			rawPath: "",
			wantErr: true,
		},
		{
			name:    "invalid path - only root",
			rawPath: "/",
			wantErr: true,
		},
		{
			name:    "invalid path - only sumdb",
			rawPath: "/sumdb",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			whichDB, p, err := parsePath(tc.rawPath)

			if tc.wantErr {
				if err == nil {
					t.Errorf("parsePath(%q) expected error, got nil", tc.rawPath)
				}
				if err != errSumPathInvalid {
					t.Errorf("parsePath(%q) expected errSumPathInvalid, got %v", tc.rawPath, err)
				}
				return
			}

			if err != nil {
				t.Errorf("parsePath(%q) unexpected error: %v", tc.rawPath, err)
				return
			}

			if whichDB != tc.wantWhichDB {
				t.Errorf("parsePath(%q) whichDB = %q, want %q", tc.rawPath, whichDB, tc.wantWhichDB)
			}

			if p != tc.wantPath {
				t.Errorf("parsePath(%q) path = %q, want %q", tc.rawPath, p, tc.wantPath)
			}
		})
	}
}

func TestSetSumdbProxy(t *testing.T) {
	originalSupportedSumDB := make(map[string][]string)
	for k, v := range supportedSumDB {
		originalSupportedSumDB[k] = append([]string(nil), v...)
	}
	defer func() {
		supportedSumDB = originalSupportedSumDB
	}()

	type TestCase struct {
		name       string
		proxyHost  string
		wantURLs   map[string]string
	}

	tests := []TestCase{
		{
			name:      "empty proxy host - should not modify",
			proxyHost: "",
			wantURLs: map[string]string{
				"sum.golang.org":       "https://sum.golang.org/",
				"sum.golang.google.cn": "https://sum.golang.org/",
				"gosum.io":             "https://gosum.io/",
			},
		},
		{
			name:      "set proxy host without trailing slash",
			proxyHost: "https://goproxy.cn",
			wantURLs: map[string]string{
				"sum.golang.org":       "https://goproxy.cn/sumdb/sum.golang.org/",
				"sum.golang.google.cn": "https://goproxy.cn/sumdb/sum.golang.google.cn/",
				"gosum.io":             "https://goproxy.cn/sumdb/gosum.io/",
			},
		},
		{
			name:      "set proxy host with trailing slash",
			proxyHost: "https://goproxy.cn/",
			wantURLs: map[string]string{
				"sum.golang.org":       "https://goproxy.cn/sumdb/sum.golang.org/",
				"sum.golang.google.cn": "https://goproxy.cn/sumdb/sum.golang.google.cn/",
				"gosum.io":             "https://goproxy.cn/sumdb/gosum.io/",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			supportedSumDB = make(map[string][]string)
			for k, v := range originalSupportedSumDB {
				supportedSumDB[k] = append([]string(nil), v...)
			}

			SetSumdbProxy(tc.proxyHost)

			if tc.proxyHost == "" {
				for dbName := range supportedSumDB {
					if len(supportedSumDB[dbName]) != len(originalSupportedSumDB[dbName]) {
						t.Errorf("SetSumdbProxy(\"\") modified %s unexpectedly", dbName)
					}
				}
				return
			}

			for dbName, expectedURL := range tc.wantURLs {
				urls, exists := supportedSumDB[dbName]
				if !exists {
					t.Errorf("SetSumdbProxy() removed db %s", dbName)
					continue
				}
				if len(urls) != 1 {
					t.Errorf("SetSumdbProxy() db %s has %d URLs, want 1", dbName, len(urls))
					continue
				}
				if urls[0] != expectedURL {
					t.Errorf("SetSumdbProxy() db %s = %q, want %q", dbName, urls[0], expectedURL)
				}
			}
		})
	}
}

func TestHandlerInvalidPath(t *testing.T) {
	type TestCase struct {
		name           string
		path           string
		expectedStatus int
	}

	tests := []TestCase{
		{
			name:           "path too short",
			path:           "/sumdb/sum.golang.org",
			expectedStatus: http.StatusGone,
		},
		{
			name:           "unsupported database",
			path:           "/sumdb/unsupported.db.com/supported",
			expectedStatus: http.StatusGone,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("https://goproxy.io%s", tc.path), nil)
			Handler(recorder, req)

			resp := recorder.Result()
			if resp.StatusCode != tc.expectedStatus {
				t.Errorf("Handler() status = %d, want %d", resp.StatusCode, tc.expectedStatus)
			}
			_ = resp.Body.Close()
		})
	}
}
