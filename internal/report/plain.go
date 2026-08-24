package report

import (
	"fmt"
	"io"
	"text/tabwriter"
)

// WritePlain renders a static, greppable report. Used when stdout is not a
// terminal, and when --plain is passed.
//
// Deliberately no lipgloss anywhere in this file: this path exists to be piped,
// and a styled byte is an invisible one. text/tabwriter aligns the columns
// instead, because it pads with real spaces.
func WritePlain(w io.Writer, summaries []Summary) {
	if len(summaries) == 0 {
		fmt.Fprintln(w, "No activity recorded yet.")
		fmt.Fprintln(w, "Commit in a repo under your base_dir and it will show up here.")
		return
	}

	t := Totalize(summaries)
	fmt.Fprintf(w, "git-sync activity: %d repos, %d events, %d problems\n\n",
		t.Repos, t.Events, t.Problems)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "REPO\tEVENTS\tPUSHES\tRECEIVES\tPROBLEMS\tLAST ACTIVITY")
	for _, s := range summaries {
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%s\n",
			s.Repo, s.Total, s.Pushes, s.Receives, s.Problems,
			s.LastActivity.Format("2006-01-02 15:04"))
	}
	tw.Flush()

	// Then the detail: one block per repo, newest event first. One event per
	// line, so `git-sync report | grep error` returns whole, useful lines.
	for _, s := range summaries {
		fmt.Fprintf(w, "\n%s\n", s.Repo)
		for _, e := range s.Events {
			fmt.Fprintf(w, "  %s  %-8s %-5s  %s\n",
				e.Time.Format("2006-01-02 15:04"), e.Op, e.Status, e.Msg)
		}
	}
}
