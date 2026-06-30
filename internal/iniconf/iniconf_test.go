package iniconf

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseSimple(t *testing.T) {
	in := `# header comment
; another comment style

fritz-url   = https://fritz.box
fritz-user  = smartmeter
fritz-password = "p@ss w/ spaces"
listen      = :8080
poll        = 2s
phase-mode  = split   # trailing comment
url-with-fragment = https://example/path#anchor
empty-line-above =

[ignored-section]
`
	m, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"fritz-url":         "https://fritz.box",
		"fritz-user":        "smartmeter",
		"fritz-password":    "p@ss w/ spaces",
		"listen":            ":8080",
		"poll":              "2s",
		"phase-mode":        "split",
		"url-with-fragment": "https://example/path#anchor",
		"empty-line-above":  "",
	}
	for k, want := range cases {
		got, ok := m[k]
		if !ok {
			t.Errorf("missing key %q", k)
			continue
		}
		if got != want {
			t.Errorf("%s: got %q, want %q", k, got, want)
		}
	}
}

func TestParseRejectsMissingEquals(t *testing.T) {
	_, err := Parse(strings.NewReader("foo bar baz"))
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestParseRejectsEmptyKey(t *testing.T) {
	_, err := Parse(strings.NewReader("= value"))
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestParseEmptyReturnsErrEmpty(t *testing.T) {
	_, err := Parse(strings.NewReader("# only comments\n# nothing else\n"))
	if !errors.Is(err, ErrEmpty) {
		t.Fatalf("expected ErrEmpty, got %v", err)
	}
}

func TestParseFileMissing(t *testing.T) {
	_, err := ParseFile(filepath.Join(t.TempDir(), "does-not-exist.conf"))
	if !os.IsNotExist(err) {
		t.Fatalf("expected IsNotExist, got %v", err)
	}
}

func TestParseFileEmptyOK(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.conf")
	if err := os.WriteFile(p, []byte("# comments only\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := ParseFile(p)
	if err != nil {
		t.Fatalf("expected nil error on comments-only file, got %v", err)
	}
	if len(m) != 0 {
		t.Fatalf("expected empty map, got %v", m)
	}
}

func TestUnquote(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{`hello`, `hello`},
		{`"hello"`, `hello`},
		{`'hello'`, `hello`},
		{`"with spaces"`, `with spaces`},
		{`"mixed'`, `"mixed'`},
		{``, ``},
	} {
		if got := unquote(tc.in); got != tc.want {
			t.Errorf("unquote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
