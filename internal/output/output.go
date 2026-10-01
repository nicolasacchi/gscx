// Package output renders command results as JSON, with an optional gjson
// filter (--jq) and the fleet's agent-mode row cap.
package output

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

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

// Cap applies the --limit flag (or the agent-mode default cap) to rows. The
// returned *Truncation is nil when nothing was dropped.
func Cap[T any](rows []T, userLimit int) ([]T, *Truncation) {
	capped, truncated := clout.CapRows(rows, clout.ResolveLimit(userLimit))
	if !truncated {
		return rows, nil
	}
	return capped, &Truncation{Truncated: true, Shown: len(capped), Total: len(rows)}
}
