package tempdoctor

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	providercodex "github.com/RenseiAI/donmai/provider/harness/codex"
	"github.com/RenseiAI/donmai/tempown"
)

// WriteText renders the report for a terminal.
func (r *Report) WriteText(w io.Writer) error {
	var b strings.Builder
	fmt.Fprintf(&b, "Temp folder:  %s\n", r.Root)
	scanNote := fmt.Sprintf("%d top-level entries, scanned in %s", r.TopLevel, time.Duration(r.ElapsedMS)*time.Millisecond)
	if r.Partial {
		scanNote += ", PARTIAL (" + r.PartialReason + ")"
	}
	fmt.Fprintf(&b, "Scan:         %s\n", scanNote)
	fmt.Fprintf(&b, "Contents:     %s entries, %s\n\n", countText(r.Entries, r.Estimated, false), bytesText(r.Bytes, r.Estimated, false))

	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "CLASS\tPRODUCER\tPATTERN\tITEMS\tENTRIES\tSIZE\tDETAIL")
	for _, g := range r.Groups {
		entries, size := "not measured", ""
		if g.Measured || g.Entries > 0 {
			entries = countText(g.Entries, g.Estimated, g.LowerBound)
			size = bytesText(g.Bytes, g.Estimated, g.LowerBound)
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\t%s\n", g.Class, g.Label, g.Pattern, g.Items, entries, size, groupDetail(g))
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	if len(r.FlatDirs) > 0 {
		b.WriteString("\nWarnings:\n")
		for _, f := range r.FlatDirs {
			fmt.Fprintf(&b, "  flat directory with %s direct entries: %s\n", countText(f.Entries, f.Estimated, false), f.Path)
		}
		b.WriteString("  Every tool start that lists such a directory, and every cleanup, pays for each entry.\n")
	}

	b.WriteString("\n")
	if r.BootCleanup.Applies {
		fmt.Fprintf(&b, "Boot cleanup: macOS empties this folder at boot, before the user session starts.\n")
		fmt.Fprintf(&b, "              Estimated cost: %s entries, %s at %s entries/s (rough).\n",
			countText(r.BootCleanup.Entries, r.Estimated, false), durationText(r.BootCleanup.Seconds), countText(int64(r.BootCleanup.EntriesPerSecond), false, false))
	} else {
		fmt.Fprintf(&b, "Boot cleanup: this OS does not empty this folder at boot; cleaning %s entries would take %s at %s entries/s.\n",
			countText(r.BootCleanup.Entries, r.Estimated, false), durationText(r.BootCleanup.Seconds), countText(int64(r.BootCleanup.EntriesPerSecond), false, false))
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// Reclaimable counts what Fix would remove without IncludeToolCaches:
// recorded dead daemon artefacts.
func (r *Report) Reclaimable() int {
	n := 0
	for _, g := range r.Groups {
		n += g.Verdicts[tempown.VerdictReclaimable]
	}
	return n
}

func groupDetail(g Group) string {
	switch {
	case len(g.Verdicts) > 0:
		return verdictText(g.Verdicts)
	case len(g.Shapes) > 0:
		return shapeText(g.Shapes)
	case g.Class == ClassToolCache:
		return "shared; removed only with --include-tool-caches"
	case g.Class == ClassDaemonCache:
		return "shared across sessions; kept"
	case g.Class == ClassCodex:
		return "reclaimed by the daemon's codex sweep"
	}
	return ""
}

func verdictText(v map[tempown.Verdict]int) string {
	order := []tempown.Verdict{tempown.VerdictReclaimable, tempown.VerdictLive, tempown.VerdictYoung, tempown.VerdictLegacy, tempown.VerdictUnowned, tempown.VerdictAmbiguous}
	parts := make([]string, 0, len(order))
	for _, k := range order {
		if v[k] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", v[k], k))
		}
	}
	return strings.Join(parts, ", ")
}

func shapeText(s map[providercodex.HomeShape]int) string {
	keys := make([]string, 0, len(s))
	for k := range s {
		keys = append(keys, string(k))
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%d %s", s[providercodex.HomeShape(k)], k))
	}
	return strings.Join(parts, ", ")
}

// WriteText renders a fix result for a terminal.
func (res FixResult) WriteText(w io.Writer) error {
	var b strings.Builder
	j := res.Janitor
	fmt.Fprintf(&b, "Daemon artefacts: removed %d (kept %d live, %d young, %d legacy untracked, %d unowned, %d ambiguous; %d errors)\n",
		j.Reclaimed, j.SkippedLive, j.SkippedYoung, j.SkippedLegacy, j.SkippedUnowned, j.SkippedAmbiguous, j.Errors)
	c := res.Codex
	fmt.Fprintf(&b, "Codex homes:      removed %d never used (left %d to the daemon's sweep, %d young; %d errors)\n",
		c.NeverUsed, c.SkippedOutOfScope, c.SkippedYoung, c.Errors)
	if res.ToolCaches.Requested {
		t := res.ToolCaches
		switch {
		case t.Refused != "":
			fmt.Fprintf(&b, "Tool caches:      not removed: %s\n", t.Refused)
		default:
			fmt.Fprintf(&b, "Tool caches:      removed %d", len(t.Removed))
			if len(t.Failed) > 0 {
				fmt.Fprintf(&b, ", %d failed", len(t.Failed))
			}
			b.WriteString("\n")
			for path, why := range t.Failed {
				fmt.Fprintf(&b, "  failed: %s: %s\n", path, why)
			}
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// countText renders an entry count: "2.77M", "~2.77M" when estimated,
// ">=2.77M" when only a lower bound is known.
func countText(n int64, estimated, lowerBound bool) string {
	s := shortCount(n)
	switch {
	case lowerBound:
		return ">=" + s
	case estimated:
		return "~" + s
	}
	return s
}

func shortCount(n int64) string {
	switch {
	case n >= 1_000_000:
		return trimFloat(float64(n)/1_000_000) + "M"
	case n >= 10_000:
		return trimFloat(float64(n)/1_000) + "k"
	}
	return strconv.FormatInt(n, 10)
}

func bytesText(n int64, estimated, lowerBound bool) string {
	units := []string{"B", "KB", "MB", "GB", "TB"}
	v := float64(n)
	i := 0
	for v >= 1000 && i < len(units)-1 {
		v /= 1000
		i++
	}
	s := trimFloat(v) + " " + units[i]
	if i == 0 {
		s = strconv.FormatInt(n, 10) + " B"
	}
	switch {
	case lowerBound:
		return ">=" + s
	case estimated:
		return "~" + s
	}
	return s
}

func trimFloat(v float64) string {
	if v >= 100 {
		return strconv.FormatFloat(math.Round(v), 'f', 0, 64)
	}
	if v >= 10 {
		return strconv.FormatFloat(v, 'f', 1, 64)
	}
	return strconv.FormatFloat(v, 'f', 2, 64)
}

func durationText(seconds float64) string {
	d := time.Duration(seconds * float64(time.Second))
	if d < time.Second {
		return "under a second"
	}
	return "about " + d.Round(time.Second).String()
}
