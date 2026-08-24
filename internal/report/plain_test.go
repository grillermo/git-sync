package report_test

import (
	"strings"
	"testing"
	"time"

	"github.com/grillermo/git-sync/internal/activity"
	"github.com/grillermo/git-sync/internal/report"
)

// Two fixed instants a few minutes apart, t1 before t2. Fixed rather than
// relative to time.Now() so the rendered timestamps never shift under the
// assertions below.
var (
	t1 = time.Date(2026, 8, 22, 12, 5, 0, 0, time.UTC)
	t2 = time.Date(2026, 8, 22, 12, 9, 0, 0, time.UTC)
)

func TestPlainShowsSummaryThenDetail(t *testing.T) {
	events := []activity.Event{
		{Time: t1, Repo: "work/api", Op: activity.OpPush, Status: activity.StatusOK, Msg: "pushed main to github"},
		{Time: t2, Repo: "work/api", Op: activity.OpReceive, Status: activity.StatusWarn, Msg: "diverged"},
	}
	var buf strings.Builder
	report.WritePlain(&buf, report.Summarize(events))
	out := buf.String()

	if !strings.Contains(out, "work/api") {
		t.Errorf("output should name the repo:\n%s", out)
	}
	// Summary before detail.
	if strings.Index(out, "1 repo") > strings.Index(out, "diverged") {
		t.Errorf("summary should come before per-repo detail:\n%s", out)
	}
	if !strings.Contains(out, "diverged") {
		t.Errorf("output should include the detail messages:\n%s", out)
	}
}

func TestPlainIsGreppableOnePerLine(t *testing.T) {
	// The whole point of the non-tty path: pipe it into grep.
	events := []activity.Event{
		{Time: t1, Repo: "a", Op: activity.OpPush, Status: activity.StatusOK, Msg: "pushed"},
		{Time: t2, Repo: "a", Op: activity.OpPush, Status: activity.StatusError, Msg: "push failed"},
	}
	var buf strings.Builder
	report.WritePlain(&buf, report.Summarize(events))
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, "push failed") && strings.Contains(line, "pushed to") {
			t.Errorf("two events collapsed onto one line: %q", line)
		}
	}
}

func TestPlainOnEmptyHistoryExplainsWhy(t *testing.T) {
	var buf strings.Builder
	report.WritePlain(&buf, nil)
	out := buf.String()
	if !strings.Contains(out, "No activity") {
		t.Errorf("empty output should say so plainly, got:\n%s", out)
	}
}

func TestPlainMarksProblems(t *testing.T) {
	events := []activity.Event{
		{Time: t1, Repo: "a", Op: activity.OpReceive, Status: activity.StatusError, Msg: "fetch failed"},
	}
	var buf strings.Builder
	report.WritePlain(&buf, report.Summarize(events))
	if !strings.Contains(buf.String(), "error") {
		t.Errorf("a problem should be labelled in the output:\n%s", buf.String())
	}
}

// This is the property the whole two-renderer split exists to protect. A
// single lipgloss style leaking into this path would corrupt every downstream
// grep, awk and diff - and would do it invisibly, since a terminal renders the
// escapes away. Assert on the bytes, not on how it looks.
func TestPlainEmitsNoANSIEscapes(t *testing.T) {
	events := []activity.Event{
		{Time: t1, Repo: "work/api", Op: activity.OpPush, Status: activity.StatusOK, Msg: "pushed main"},
		{Time: t2, Repo: "work/api", Op: activity.OpReceive, Status: activity.StatusWarn, Msg: "diverged"},
		{Time: t2, Repo: "personal/notes", Op: activity.OpPush, Status: activity.StatusError, Msg: "push failed"},
	}
	var buf strings.Builder
	report.WritePlain(&buf, report.Summarize(events))
	if got := buf.String(); strings.Contains(got, "\x1b[") {
		t.Errorf("plain output must be ANSI-free, it gets piped into grep:\n%q", got)
	}

	// The empty branch renders too, and must stay just as clean.
	var empty strings.Builder
	report.WritePlain(&empty, nil)
	if got := empty.String(); strings.Contains(got, "\x1b[") {
		t.Errorf("empty plain output must be ANSI-free:\n%q", got)
	}
}
