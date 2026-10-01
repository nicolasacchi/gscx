package commands

import (
	"sort"

	"github.com/nicolasacchi/gscx/internal/gsc"
	"github.com/spf13/cobra"
	searchconsole "google.golang.org/api/searchconsole/v1"
)

var sitesCmd = &cobra.Command{
	Use:   "sites",
	Short: "List the Search Console properties the credentials can see",
}

var sitesListCmd = &cobra.Command{
	Use:   "list",
	Short: "List accessible properties with the caller's permission level",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		svc, err := newGSC(ctx)
		if err != nil {
			return err
		}
		resp, err := svc.Sites.List().Context(ctx).Do()
		if err != nil {
			return gsc.Wrap(err)
		}
		return emit(cmd, siteViews(resp.SiteEntry))
	},
}

var sitesGetCmd = &cobra.Command{
	Use:   "get",
	Short: "Show the permission level for the --site property",
	Long: `Shows the caller's permission level on the --site property. A 403/404 here
means the service account is not a user of that property, or --site is spelled
differently from the property (sc-domain:example.com vs https://www.example.com/).`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		svc, err := newGSC(ctx)
		if err != nil {
			return err
		}
		resp, err := svc.Sites.Get(siteURL()).Context(ctx).Do()
		if err != nil {
			return gsc.Wrap(err)
		}
		return emit(cmd, siteView(resp))
	},
}

// siteEntryView is the output shape of a property.
type siteEntryView struct {
	SiteURL         string `json:"siteUrl"`
	PermissionLevel string `json:"permissionLevel"`
}

func siteView(s *searchconsole.WmxSite) siteEntryView {
	return siteEntryView{SiteURL: s.SiteUrl, PermissionLevel: s.PermissionLevel}
}

// siteViews converts and sorts by property URL; never nil so an empty account
// prints [] rather than null.
func siteViews(in []*searchconsole.WmxSite) []siteEntryView {
	out := make([]siteEntryView, 0, len(in))
	for _, s := range in {
		if s != nil {
			out = append(out, siteView(s))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SiteURL < out[j].SiteURL })
	return out
}

func init() {
	sitesCmd.AddCommand(sitesListCmd, sitesGetCmd)
	rootCmd.AddCommand(sitesCmd)
}
