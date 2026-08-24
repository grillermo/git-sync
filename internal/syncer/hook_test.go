package syncer_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/grillermo/git-sync/internal/activity"
	"github.com/grillermo/git-sync/internal/config"
	"github.com/grillermo/git-sync/internal/syncer"
	"github.com/grillermo/git-sync/internal/testutil"
)

func TestHookRecordsTheRepoItFiredIn(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfig(t, sb, "peer.example", "tester")

	// A spawner that records instead of forking, so this test is synchronous.
	var gotRel string
	spawn := func(rel string) error { gotRel = rel; return nil }

	if err := syncer.Hook(repo, spawn); err != nil {
		t.Fatalf("Hook: %v", err)
	}
	if gotRel != "group/proj" {
		t.Errorf("spawned for %q, want %q", gotRel, "group/proj")
	}
}

func TestHookIgnoresAnUnselectedRepo(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	// base_dir is configured, but this repo was never ticked in the picker.
	testutil.SaveConfigWithRepos(t, sb, "peer.example", "tester", []string{"other/thing"})

	spawned := false
	if err := syncer.Hook(repo, func(string) error { spawned = true; return nil }); err != nil {
		t.Fatalf("an unselected repo is not an error: %v", err)
	}
	if spawned {
		t.Error("must not push a repo the user did not select")
	}
	// And it must not be recorded: this fires on every commit in every
	// unselected repo, and would drown the log.
	events, _ := activity.Read()
	if len(events) != 0 {
		t.Errorf("expected no events, got %+v", events)
	}
}

func TestHookSyncsASelectedRepo(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	testutil.SaveConfigWithRepos(t, sb, "peer.example", "tester", []string{"group/proj"})

	var gotRel string
	if err := syncer.Hook(repo, func(rel string) error { gotRel = rel; return nil }); err != nil {
		t.Fatal(err)
	}
	if gotRel != "group/proj" {
		t.Errorf("spawned for %q, want group/proj", gotRel)
	}
}

func TestHookSkipsARepoOutsideBaseDir(t *testing.T) {
	sb := testutil.NewSandbox(t)
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	outside := filepath.Join(sb.Home, "elsewhere", "proj")
	testutil.MkdirAll(t, outside)
	sb.Git(sb.Home, "init", "-q", outside)

	spawned := false
	spawn := func(string) error { spawned = true; return nil }

	if err := syncer.Hook(outside, spawn); err != nil {
		t.Fatalf("a repo outside base_dir is not an error: %v", err)
	}
	if spawned {
		t.Error("should not have spawned a push for a repo outside base_dir")
	}
	testutil.AssertEvent(t, activity.OpHook, activity.StatusSkip, "base_dir")
}

func TestHookIsANoOpWhenNotInstalled(t *testing.T) {
	sb := testutil.NewSandbox(t)
	repo := sb.MakeRepo("group/proj")
	_ = os.Remove(config.Path())

	spawned := false
	if err := syncer.Hook(repo, func(string) error { spawned = true; return nil }); err != nil {
		t.Fatalf("an uninstalled git-sync must not break commits: %v", err)
	}
	if spawned {
		t.Error("should not spawn without config")
	}
}

func TestSpawnDetachedReturnsImmediately(t *testing.T) {
	sb := testutil.NewSandbox(t)
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	// A fake "self" binary that sleeps, standing in for a slow push.
	self := testutil.WriteScript(t, sb, "slow-self", "#!/bin/sh\nsleep 5\n")

	start := time.Now()
	if err := syncer.SpawnDetached(self, "group/proj"); err != nil {
		t.Fatalf("SpawnDetached: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("SpawnDetached blocked for %v; the commit would have stalled", elapsed)
	}
}

func TestSpawnDetachedSurvivesItsParent(t *testing.T) {
	sb := testutil.NewSandbox(t)
	testutil.SaveConfig(t, sb, "peer.example", "tester")
	marker := filepath.Join(sb.Home, "child-ran")
	self := testutil.WriteScript(t, sb, "slow-self",
		"#!/bin/sh\nsleep 1\ntouch "+marker+"\n")

	if err := syncer.SpawnDetached(self, "group/proj"); err != nil {
		t.Fatal(err)
	}
	// The parent (this test) does not wait; the child must still finish.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("the detached child never completed")
}
