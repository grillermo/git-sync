package activity_test

import (
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/grillermo/git-sync/internal/activity"
	"github.com/grillermo/git-sync/internal/config"
	"github.com/grillermo/git-sync/internal/testutil"
)

func TestAppendThenRead(t *testing.T) {
	testutil.NewSandbox(t)
	want := activity.Event{
		Repo: "group/proj", Op: activity.OpPush, Status: activity.StatusOK,
		Msg: "pushed", Branch: "main",
	}
	if err := activity.Append(want); err != nil {
		t.Fatalf("Append: %v", err)
	}
	got, err := activity.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1", len(got))
	}
	if got[0].Repo != want.Repo || got[0].Op != want.Op || got[0].Status != want.Status {
		t.Errorf("round trip mismatch: %+v", got[0])
	}
	if got[0].Time.IsZero() {
		t.Error("Append should stamp Time when it is zero")
	}
}

func TestAppendPreservesAnExplicitTime(t *testing.T) {
	testutil.NewSandbox(t)
	ts := time.Date(2026, 8, 22, 14, 2, 0, 0, time.UTC)
	_ = activity.Append(activity.Event{Time: ts, Repo: "a", Op: activity.OpPush})
	got, _ := activity.Read()
	if !got[0].Time.Equal(ts) {
		t.Errorf("Time = %v, want %v", got[0].Time, ts)
	}
}

func TestReadMissingFileIsEmptyNotAnError(t *testing.T) {
	testutil.NewSandbox(t)
	got, err := activity.Read()
	if err != nil {
		t.Fatalf("Read on a fresh install should not error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d events, want 0", len(got))
	}
}

func TestReadSkipsCorruptLines(t *testing.T) {
	testutil.NewSandbox(t)
	_ = activity.Append(activity.Event{Repo: "a", Op: activity.OpPush})
	// Simulate a torn write from a killed process.
	f, _ := os.OpenFile(config.ActivityPath(), os.O_APPEND|os.O_WRONLY, 0o644)
	_, _ = f.WriteString("{not json\n")
	_ = f.Close()
	_ = activity.Append(activity.Event{Repo: "b", Op: activity.OpPush})

	got, err := activity.Read()
	if err != nil {
		t.Fatalf("a corrupt line must not fail the whole read: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("got %d events, want the 2 good ones", len(got))
	}
}

func TestAppendIsAtomicUnderConcurrency(t *testing.T) {
	testutil.NewSandbox(t)
	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = activity.Append(activity.Event{
				Repo: "group/proj", Op: activity.OpReceive, Status: activity.StatusOK,
				Msg: strings.Repeat("x", 200),
			})
		}(i)
	}
	wg.Wait()

	got, err := activity.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != n {
		t.Errorf("got %d events, want %d - concurrent appends interleaved", len(got), n)
	}
}

func TestAppendTruncatesAnOverlongMessage(t *testing.T) {
	testutil.NewSandbox(t)
	_ = activity.Append(activity.Event{Repo: "a", Op: activity.OpPush, Msg: strings.Repeat("y", 10000)})
	b, _ := os.ReadFile(config.ActivityPath())
	if len(b) > activity.MaxLineLen {
		t.Errorf("line is %d bytes, want <= %d so the append stays atomic", len(b), activity.MaxLineLen)
	}
	got, _ := activity.Read()
	if !strings.HasSuffix(got[0].Msg, "...") {
		t.Errorf("a truncated message should be marked with an ellipsis, got %q", got[0].Msg)
	}
}

func TestEventIsProblem(t *testing.T) {
	cases := []struct {
		status activity.Status
		want   bool
	}{
		{activity.StatusOK, false},
		{activity.StatusSkip, false},
		{activity.StatusWarn, true},
		{activity.StatusError, true},
	}
	for _, c := range cases {
		e := activity.Event{Status: c.status}
		if got := e.IsProblem(); got != c.want {
			t.Errorf("Event{%s}.IsProblem() = %v, want %v", c.status, got, c.want)
		}
	}
}
