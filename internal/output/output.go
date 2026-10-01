// Package output renders command results as JSON, with an optional gjson
// filter (--jq) and the fleet's agent-mode row cap.
package output

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync/atomic"

	clout "github.com/nicolasacchi/clicore/output"
	"github.com/tidwall/gjson"
)

// Emit writes v as indented JSON to w. If jq is non-empty it is applied as a
// gjson path (e.g. "rows.#.page", "by_type.product") and only the match is
// printed: strings unquoted, objects/arrays as indented JSON, missing → null.
func Emit(w io.Writer, v any, jq string) error {
	// No HTML escaping: URLs with query strings must print '&', '<', '>'
	// verbatim instead of as unicode escapes.
	var raw bytes.Buffer
	enc := json.NewEncoder(&raw)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("encode output: %w", err)
	}
	b := bytes.TrimRight(raw.Bytes(), "\n")
	var err error
	if jq != "" {
		if capped.Load() {
			// The truncated marker is usually filtered away by --jq, so say it
			// here: a filter over a capped list silently returns partial data.
			fmt.Fprintln(os.Stderr, "note: a row list was capped before --jq was applied; results may be partial. Pass --limit 0 for all rows.")
		}
		r := gjson.GetBytes(b, jq)
		switch {
		case !r.Exists():
			_, err = fmt.Fprintln(w, "null")
			return err
		case r.Type == gjson.String:
			_, err = fmt.Fprintln(w, r.String())
			return err
		default:
			b = []byte(r.Raw)
		}
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, b, "", "  "); err != nil {
		return fmt.Errorf("indent output: %w", err)
	}
	buf.WriteByte('\n')
	_, err = w.Write(buf.Bytes())
	return err
}

// Truncation describes a capped row list. It is embedded in outputs as
// "truncated" so agents can tell a partial list from a complete one.
type Truncation struct {
	Truncated bool `json:"truncated"`
	Shown     int  `json:"shown"`
	Total     int  `json:"total"`
}

// Unlimited is the --limit value meaning "no cap, even under CLAUDECODE".
// The root command maps an explicit --limit 0 to it.
const Unlimited = -1

// capped records whether any Cap call in this process dropped rows, so Emit
// can warn when a --jq filter ran over a partial list.
var capped atomic.Bool

// Cap applies the --limit flag (or the agent-mode default cap) to rows. The
// returned *Truncation is nil when nothing was dropped. userLimit < 0
// (Unlimited) disables the cap entirely.
func Cap[T any](rows []T, userLimit int) ([]T, *Truncation) {
	if userLimit < 0 {
		return rows, nil
	}
	out, truncated := clout.CapRows(rows, clout.ResolveLimit(userLimit))
	if !truncated {
		return rows, nil
	}
	capped.Store(true)
	return out, &Truncation{Truncated: true, Shown: len(out), Total: len(rows)}
}

// MarshalNoEscape marshals v without HTML-escaping '&', '<', '>'. API structs
// with their own MarshalJSON escape internally (encoding/json does), so the
// result is decoded and re-encoded; numbers are kept verbatim via UseNumber.
func MarshalNoEscape(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var generic any
	if err := dec.Decode(&generic); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(generic); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
