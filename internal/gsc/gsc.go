// Package gsc builds the Search Console API client and maps its errors onto
// the fleet's structured error type.
package gsc

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nicolasacchi/clicore/cierrors"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	searchconsole "google.golang.org/api/searchconsole/v1"
)

// Scope is read-only: gscx never writes to Search Console.
const Scope = searchconsole.WebmastersReadonlyScope

// NewService returns a Search Console client. credentialsFile is a
// service-account key; empty uses Application Default Credentials.
func NewService(ctx context.Context, credentialsFile, userAgent string) (*searchconsole.Service, error) {
	opts := []option.ClientOption{option.WithScopes(Scope)}
	if credentialsFile != "" {
		opts = append(opts, option.WithAuthCredentialsFile(option.ServiceAccount, credentialsFile))
	}
	if userAgent != "" {
		opts = append(opts, option.WithUserAgent(userAgent))
	}
	svc, err := searchconsole.NewService(ctx, opts...)
	if err != nil {
		return nil, &cierrors.APIError{
			Kind:   "auth_failed",
			Detail: fmt.Sprintf("search console client: %v", err),
			Hint:   "set GOOGLE_APPLICATION_CREDENTIALS or credentials_file to a service-account key with access to the property",
		}
	}
	return svc, nil
}

// Wrap converts a Google API error into a *cierrors.APIError with a hint.
// Other errors are returned unchanged.
func Wrap(err error) error {
	if err == nil {
		return nil
	}
	var gerr *googleapi.Error
	if !errors.As(err, &gerr) {
		return err
	}
	e := &cierrors.APIError{
		Status: gerr.Code,
		Kind:   cierrors.KindForStatus(gerr.Code),
		Detail: strings.TrimSpace(gerr.Message),
	}
	if e.Detail == "" {
		e.Detail = strings.TrimSpace(gerr.Body)
	}
	switch gerr.Code {
	case 401:
		e.Hint = "credentials rejected: check the service-account key"
	case 403:
		e.Hint = "the service account needs at least Restricted access on this Search Console property (Settings → Users and permissions); check --site spelling (sc-domain:… vs https://…/)"
	case 429:
		e.Hint = "quota exceeded: URL Inspection allows 2,000/day and 600/min per property; retry later or lower inspect.daily_budget / per_minute"
	}
	return e
}
