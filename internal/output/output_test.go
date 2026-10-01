package output

import (
	"bytes"
	"testing"
)

func TestEmitPlain(t *testing.T) {
	var buf bytes.Buffer
	if err := Emit(&buf, map[string]any{"a": 1}, ""); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "{\n  \"a\": 1\n}\n" {
		t.Fatalf("got %q", got)
	}
}

func TestEmitJQ(t *testing.T) {
	v := map[string]any{"rows": []map[string]any{{"page": "/a"}, {"page": "/b"}}, "name": "x"}
	cases := map[string]string{
		"name":         "x\n",
		"rows.#.page":  "[\n  \"/a\",\n  \"/b\"\n]\n",
		"missing.path": "null\n",
	}
	for q, want := range cases {
		var buf bytes.Buffer
		if err := Emit(&buf, v, q); err != nil {
			t.Fatal(err)
		}
		if buf.String() != want {
			t.Errorf("jq %q: got %q want %q", q, buf.String(), want)
		}
	}
}

func TestCap(t *testing.T) {
	t.Setenv("CLAUDECODE", "")
	rows := make([]int, 150)
	got, tr := Cap(rows, 0)
	if len(got) != 150 || tr != nil {
		t.Fatalf("human unbounded: len=%d tr=%v", len(got), tr)
	}
	got, tr = Cap(rows, 10)
	if len(got) != 10 || tr == nil || tr.Total != 150 || tr.Shown != 10 {
		t.Fatalf("explicit cap: len=%d tr=%+v", len(got), tr)
	}
	t.Setenv("CLAUDECODE", "1")
	got, tr = Cap(rows, 0)
	if len(got) != 100 || tr == nil {
		t.Fatalf("agent cap: len=%d tr=%+v", len(got), tr)
	}
}
