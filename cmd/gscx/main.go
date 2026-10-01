package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/nicolasacchi/clicore/cierrors"
	"github.com/nicolasacchi/gscx/internal/commands"
)

var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	commands.SetVersion(version)
	if err := commands.Execute(ctx); err != nil {
		os.Exit(report(err))
	}
}

// report prints err to stderr and returns the fleet-canonical exit code.
// Structured API errors are printed as a JSON envelope so agents can branch on
// kind/status without parsing prose.
func report(err error) int {
	var apiErr *cierrors.APIError
	if errors.As(err, &apiErr) {
		b, mErr := json.Marshal(map[string]any{"error": apiErr})
		if mErr == nil {
			fmt.Fprintln(os.Stderr, string(b))
		} else {
			fmt.Fprintln(os.Stderr, apiErr.Error())
		}
		return apiErr.ExitCode()
	}
	fmt.Fprintln(os.Stderr, "error:", err)
	return 1
}
