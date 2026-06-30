// Package iniconf loads simple "key = value" configuration files that
// mirror the style used by github.com/flunaras/fritzhome-cache.
//
// Recognised syntax:
//
//	# comment line
//	; comment line (also accepted)
//	key = value             # trailing comment after value
//	key=value
//	key = "value with spaces"
//	key = 'value with spaces'
//	key = value with spaces  # works without quotes too
//
// The file is intentionally flat (no sections). Empty lines are skipped.
// Keys are case-sensitive and match the long flag name without the
// leading "--".
package iniconf

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// Parse reads "key = value" pairs from r and returns them as a map.
// Returns ErrEmpty when the input contains no usable pairs.
func Parse(r io.Reader) (map[string]string, error) {
	out := make(map[string]string)
	scan := bufio.NewScanner(r)
	scan.Buffer(make([]byte, 0, 64*1024), 1<<20)
	line := 0
	for scan.Scan() {
		line++
		raw := scan.Text()
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		// Whole-line comments.
		if trimmed[0] == '#' || trimmed[0] == ';' {
			continue
		}
		// Sections like "[Section]" are silently ignored so we stay
		// forward-compatible if anyone adds them.
		if trimmed[0] == '[' && strings.HasSuffix(trimmed, "]") {
			continue
		}

		eq := strings.IndexByte(trimmed, '=')
		if eq < 0 {
			return nil, fmt.Errorf("config: line %d: missing '=' in %q", line, raw)
		}
		key := strings.TrimSpace(trimmed[:eq])
		val := strings.TrimSpace(trimmed[eq+1:])
		if key == "" {
			return nil, fmt.Errorf("config: line %d: empty key", line)
		}

		// Strip a trailing "# comment" — but only when '#' is preceded
		// by whitespace, so things like URLs containing '#' survive.
		if i := indexUnquotedComment(val); i >= 0 {
			val = strings.TrimSpace(val[:i])
		}
		// Strip matching surrounding quotes.
		val = unquote(val)

		out[key] = val
	}
	if err := scan.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, ErrEmpty
	}
	return out, nil
}

// ParseFile is a convenience wrapper around Parse that opens path. It
// returns os.IsNotExist-detectable errors when path does not exist so
// callers can treat "no file present" as a normal startup mode.
func ParseFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	m, err := Parse(f)
	if errors.Is(err, ErrEmpty) {
		// Treat an empty file the same as a missing one: return no
		// values, no error. Callers should fall back to defaults.
		return map[string]string{}, nil
	}
	return m, err
}

// ErrEmpty is returned when the file parsed successfully but contained
// nothing other than comments and blank lines.
var ErrEmpty = errors.New("config: empty")

// indexUnquotedComment returns the index of the first '#' (or ';') that
// is preceded by whitespace and is outside a quoted string. Returns -1
// if no such delimiter exists.
func indexUnquotedComment(s string) int {
	var inSingle, inDouble bool
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '#', ';':
			if inSingle || inDouble {
				continue
			}
			// Only treat as a comment when preceded by whitespace, so
			// that values like "foo#bar" are preserved.
			if i == 0 || s[i-1] == ' ' || s[i-1] == '\t' {
				return i
			}
		}
	}
	return -1
}

func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}
