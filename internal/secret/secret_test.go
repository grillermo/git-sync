package secret_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/grillermo/git-sync/internal/secret"
	"github.com/grillermo/git-sync/internal/testutil"
)

func TestSetGetDelete(t *testing.T) {
	testutil.NewSandbox(t) // sets GITSYNC_SECRET_BACKEND=file
	if err := secret.Set("tester@peer.example", []byte("hunter2")); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := secret.Get("tester@peer.example")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "hunter2" {
		t.Errorf("Get = %q, want %q", got, "hunter2")
	}
	if err := secret.Delete("tester@peer.example"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := secret.Get("tester@peer.example"); !secret.IsNotFound(err) {
		t.Errorf("err = %v, want not-found after Delete", err)
	}
}

func TestGetMissingIsDistinguishable(t *testing.T) {
	// "no password stored" is the normal key-auth case, not a failure.
	testutil.NewSandbox(t)
	_, err := secret.Get("nobody@nowhere")
	if !secret.IsNotFound(err) {
		t.Errorf("err = %v, want IsNotFound", err)
	}
}

func TestSetOverwrites(t *testing.T) {
	testutil.NewSandbox(t)
	_ = secret.Set("a@b", []byte("old"))
	if err := secret.Set("a@b", []byte("new")); err != nil {
		t.Fatalf("Set should update an existing item: %v", err)
	}
	got, _ := secret.Get("a@b")
	if string(got) != "new" {
		t.Errorf("Get = %q, want the updated password", got)
	}
}

func TestDeleteMissingIsNotAnError(t *testing.T) {
	testutil.NewSandbox(t)
	if err := secret.Delete("nobody@nowhere"); err != nil {
		t.Errorf("deleting what is not there should be a no-op: %v", err)
	}
}

func TestFileBackendIsRestrictedToTheOwner(t *testing.T) {
	// The file backend is for tests, but it still holds a real password on
	// disk while they run.
	sb := testutil.NewSandbox(t)
	_ = secret.Set("a@b", []byte("hunter2"))
	fi, err := os.Stat(filepath.Join(sb.GitsyncHome, "secrets"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("mode = %v, want owner-only", fi.Mode().Perm())
	}
}

func TestBlackholeBackendAcceptsWritesAndStoresNothing(t *testing.T) {
	// Exists purely to let EnsureAuth's tests prove it verifies by reading
	// back the store, rather than assuming a write persisted.
	testutil.NewSandbox(t)
	t.Setenv("GITSYNC_SECRET_BACKEND", "blackhole")

	if err := secret.Set("a@b", []byte("hunter2")); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, err := secret.Get("a@b"); !secret.IsNotFound(err) {
		t.Errorf("err = %v, want IsNotFound: the blackhole must not persist", err)
	}
}
