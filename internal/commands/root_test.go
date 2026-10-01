package commands

import (
	"bytes"
	"testing"

	"github.com/nicolasacchi/gscx/internal/output"
)

func TestExplicitLimitZeroMeansUnlimited(t *testing.T) {
	old := flagLimit
	t.Cleanup(func() {
		flagLimit = old
		// cobra keeps Changed across Execute calls on the shared rootCmd.
		rootCmd.PersistentFlags().Lookup("limit").Changed = false
		rootCmd.SetArgs(nil)
	})
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetArgs([]string{"config", "path", "--limit", "0", "--config", t.TempDir() + "/c.toml"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if flagLimit != output.Unlimited {
		t.Fatalf("flagLimit = %d, want %d", flagLimit, output.Unlimited)
	}
}
