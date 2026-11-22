package afcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/observation"
)

// hostObservationsClient is the slice of the daemon client that
// `host observations` uses.
type hostObservationsClient interface {
	HostObservations() (*afclient.HostObservations, error)
	ScanHostObservations(ctx context.Context, kinds []string) (*afclient.HostObservations, error)
}

// defaultHostObservationsClient reuses the daemon factory's client.
func defaultHostObservationsClient(cfg afclient.DaemonConfig) hostObservationsClient {
	client, _ := defaultDaemonFactory(cfg).(hostObservationsClient)
	return client
}

func newHostObservationsCmd(newClient func(afclient.DaemonConfig) hostObservationsClient) *cobra.Command {
	var (
		kinds          []string
		rescan, asJSON bool
		port           int
		host           string
	)
	cmd := &cobra.Command{
		Use:   "observations",
		Short: "List this host's observed items as a checklist",
		Long: "List what the daemon observes on this host (for example subscription logins) with each\n" +
			"item's readiness, usage windows and local enabled flag. Items absent from the previous\n" +
			"completed scan are marked NEW. --rescan runs a full scan first (at most one a minute).",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg := afclient.DefaultDaemonConfig()
			if host != "" {
				cfg.Host = host
			}
			if port != 0 {
				cfg.Port = port
			}
			client := newClient(cfg)
			if client == nil {
				return errors.New("host observations: no daemon client")
			}
			var (
				obs *afclient.HostObservations
				err error
			)
			if rescan {
				obs, err = client.ScanHostObservations(cmd.Context(), kinds)
			} else {
				obs, err = client.HostObservations()
			}
			if err != nil {
				return fmt.Errorf("host observations: %w", err)
			}
			filterObservations(obs, kinds)
			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(obs)
			}
			writeObservationChecklist(cmd.OutOrStdout(), obs)
			return nil
		},
	}
	cmd.Flags().StringSliceVar(&kinds, "kind", nil, "Only this kind (repeatable), for example subscription-login")
	cmd.Flags().BoolVar(&rescan, "rescan", false, "Run a full scan now before listing")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Print the report and marks as JSON")
	cmd.Flags().IntVar(&port, "port", 0, "Daemon HTTP port (default from daemon.yaml: 7734)")
	cmd.Flags().StringVar(&host, "host", "", "Daemon HTTP host (default: 127.0.0.1)")
	return cmd
}

// filterObservations keeps only items and marks of kinds, when given, and
// narrows the report's coverage to match: its kinds become the requested
// kinds the report actually covers, and it stays full only for a non-empty
// coverage. A filter that covers none of the reported kinds therefore never
// reads as an authoritative "no items".
func filterObservations(obs *afclient.HostObservations, kinds []string) {
	if len(kinds) == 0 {
		return
	}
	keep := func(kind string) bool { return slices.Contains(kinds, kind) }
	obs.Report.Kinds = slices.DeleteFunc(obs.Report.Kinds, func(k string) bool { return !keep(k) })
	if len(obs.Report.Kinds) == 0 {
		obs.Report.Full = false
	}
	obs.Report.Items = slices.DeleteFunc(obs.Report.Items, func(it afclient.HostObservationItem) bool { return !keep(it.Kind) })
	obs.Marks = slices.DeleteFunc(obs.Marks, func(m afclient.HostObservationMark) bool { return !keep(m.Kind) })
}

// writeObservationChecklist prints one checklist line per item.
func writeObservationChecklist(w io.Writer, obs *afclient.HostObservations) {
	if len(obs.Report.Items) == 0 {
		_, _ = fmt.Fprintln(w, "no observed items")
		return
	}
	marks := map[string]afclient.HostObservationMark{}
	for _, m := range obs.Marks {
		marks[m.ItemKey+"/"+m.Slot] = m
	}
	for _, it := range obs.Report.Items {
		mark, ok := marks[it.ItemKey+"/"+it.Slot]
		box := "[x]"
		if ok && !mark.Enabled {
			box = "[ ]"
		}
		line := []string{box, it.Kind}
		if it.Kind == observation.KindSubscriptionLogin {
			var attrs observation.SubscriptionLogin
			if err := json.Unmarshal(it.Attributes, &attrs); err == nil {
				plan := attrs.Plan
				if plan == "" {
					plan = "-"
				}
				line = append(line, attrs.Harness+"/"+attrs.Provider, "plan="+plan)
				line = append(line, "slot="+it.Slot, it.Readiness.State, windowsSummary(attrs))
			}
		} else {
			line = append(line, "slot="+it.Slot, it.Readiness.State)
		}
		if mark.New {
			line = append(line, "NEW")
		}
		_, _ = fmt.Fprintln(w, strings.Join(line, "  "))
	}
}

// windowsSummary renders each window's usage, for example "primary 5%".
func windowsSummary(attrs observation.SubscriptionLogin) string {
	var parts []string
	for _, a := range attrs.Allowances {
		for _, win := range a.Windows {
			if a.Unit == observation.UnitTokens && win.Limit != nil {
				parts = append(parts, fmt.Sprintf("%s %g/%g", win.ID, win.Used, *win.Limit))
				continue
			}
			parts = append(parts, fmt.Sprintf("%s %g%%", win.ID, win.Used))
		}
	}
	if len(parts) == 0 {
		return "no windows"
	}
	return strings.Join(parts, " · ")
}
