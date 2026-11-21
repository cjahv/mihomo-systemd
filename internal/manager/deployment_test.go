package manager

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestDeploymentRejectsPendingConfigTransactionAndChangedFiles(t *testing.T) {
	u, _ := setupUpdater(t)
	before, err := u.snapshot(u.dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(u.dir, ".env"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if u.checkDeploymentSource(before) == nil {
		t.Fatal("accepted manual environment change")
	}
	if err = writeJSON(u.path("transaction.json"), map[string]string{"id": "pending"}); err != nil {
		t.Fatal(err)
	}
	if u.prepareDeployment(context.Background(), t.TempDir()) == nil {
		t.Fatal("prepared while config recovery was pending")
	}
}

func TestPendingDeploymentBlocksRegularUpdates(t *testing.T) {
	dir := t.TempDir()
	lock, err := acquireUpdateLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, ".update-state", "deployment.pending"), []byte("pending"), 0600); err != nil {
		t.Fatal(err)
	}
	lock.Close()
	if other, err := acquireUpdateLock(dir); err == nil {
		other.Close()
		t.Fatal("allowed config update before deployment recovery")
	}
}

func TestDeploymentPreparationAndForcedActivation(t *testing.T) {
	u, f := setupUpdater(t)
	stage := t.TempDir()
	if err := u.prepareDeployment(context.Background(), stage); err != nil {
		t.Fatal(err)
	}
	if readFile(t, filepath.Join(u.dir, "config.yaml")) != "old" || f.restarts != 0 || f.stops != 0 {
		t.Fatal("preflight mutated active runtime")
	}
	var before configSnapshot
	if err := readJSON(filepath.Join(stage, "before.json"), &before); err != nil {
		t.Fatal(err)
	}
	if before.Selections["select"] != "node-a" || !before.Verified {
		t.Fatal("lost known baseline runtime choices")
	}
	var candidate configSnapshot
	if err := readJSON(filepath.Join(stage, "candidate.json"), &candidate); err != nil {
		t.Fatal(err)
	}
	u.prepared = &candidate
	u.forceRestart = true
	if err := u.RunLocked(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if f.restarts != 1 {
		t.Fatal("deployment did not activate installed kernel")
	}
}

func TestInheritedUpdateLockHelper(t *testing.T) {
	dir := os.Getenv("INHERITED_UPDATE_LOCK_TEST")
	if dir == "" {
		return
	}
	lock, err := inheritedUpdateLock(dir, 3)
	if err != nil {
		t.Fatal(err)
	}
	lock.Close()
}

func TestInheritedLockDoesNotUnlockPublisher(t *testing.T) {
	dir := t.TempDir()
	lock, err := acquireUpdateLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestInheritedUpdateLockHelper$")
	cmd.ExtraFiles = []*os.File{lock.file}
	cmd.Env = append(os.Environ(), "INHERITED_UPDATE_LOCK_TEST="+dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child failed: %v %s", err, out)
	}
	if other, err := acquireUpdateLock(dir); !errors.Is(err, errUpdateBusy) {
		if other != nil {
			other.Close()
		}
		t.Fatalf("publisher lock released: %v", err)
	}
}
