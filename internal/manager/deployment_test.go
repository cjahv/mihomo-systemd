package manager

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	for _, scenario := range []struct {
		name string
		want []string
	}{
		{"reused", []string{"old", "new", "old"}},
		{"inputs-changed", []string{"old", "new", "old", "new", "old"}},
		{"missing-record", []string{"old", "new", "old", "new", "old"}},
		{"foreign-stage", []string{"old", "new", "old", "new", "old"}},
		{"candidate-changed", []string{"old", "new", "changed", "old"}},
		{"external-inputs", []string{"old", "new", "new", "old"}},
		{"rollback", []string{"old", "new", "old"}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			u, f := setupUpdater(t)
			f.inputsHash = "inputs"
			if scenario.name == "external-inputs" {
				f.nonReusable = "new"
			}
			stage, err := os.MkdirTemp(u.dir, ".deploy.")
			if err != nil {
				t.Fatal(err)
			}
			preparedDir := filepath.Join(stage, "prepared")
			if err := os.Mkdir(preparedDir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := u.prepareDeployment(context.Background(), preparedDir); err != nil {
				t.Fatal(err)
			}
			if readFile(t, filepath.Join(u.dir, "config.yaml")) != "old" || f.restarts != 0 || f.stops != 0 {
				t.Fatal("preflight mutated active runtime")
			}
			var before configSnapshot
			if err := readJSON(filepath.Join(preparedDir, "before.json"), &before); err != nil {
				t.Fatal(err)
			}
			if before.Selections["select"] != "node-a" || !before.Verified {
				t.Fatal("lost known baseline runtime choices")
			}
			pending := stage
			if scenario.name == "foreign-stage" {
				pending = filepath.Join(u.dir, ".deploy.other")
			}
			if err := os.WriteFile(u.path("deployment.pending"), []byte(pending+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if scenario.name == "missing-record" {
				if err := os.Remove(filepath.Join(preparedDir, deploymentValidationFile)); err != nil {
					t.Fatal(err)
				}
			}
			candidatePath := filepath.Join(preparedDir, "candidate.json")
			var candidate configSnapshot
			if err := readJSON(candidatePath, &candidate); err != nil {
				t.Fatal(err)
			}
			u.readDeploymentValidation(candidatePath)
			u.prepared = &candidate
			u.forceRestart = true
			switch scenario.name {
			case "inputs-changed":
				f.inputsHash = "different inputs"
			case "candidate-changed":
				candidate.Config = []byte("changed")
				candidate.Hash = digest(candidate.Config)
			case "rollback":
				f.probeErrors = []error{nil, nil, errors.New("candidate offline"), nil}
			}
			err = u.RunLocked(context.Background(), true)
			if scenario.name == "rollback" {
				if err == nil || f.restarts != 2 || readFile(t, filepath.Join(u.dir, "config.yaml")) != "old" {
					t.Fatalf("failed activation did not restore old config: %v (restarts=%d)", err, f.restarts)
				}
			} else if err != nil || f.restarts != 1 || f.probes != 3 {
				t.Fatalf("deployment missed restart or health check: %v (restarts=%d, probes=%d)", err, f.restarts, f.probes)
			}
			if !slices.Equal(f.validated, scenario.want) {
				t.Fatalf("validated %q, want %q", f.validated, scenario.want)
			}
		})
	}
}

func TestDeploymentValidationHashesRuntimeInputs(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	t.Setenv("CLASH_CONFIG_STRING", "")
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	write("yq", "parser")
	write("staged-core", "core")
	write("installed-core", "core")
	write("GeoSite.dat", "geosite")
	r := &linuxRuntime{dir: dir, kernel: filepath.Join(dir, "staged-core")}
	initial := r.validationInputsHash()
	if initial == "" {
		t.Fatal("could not fingerprint the staged core")
	}
	r.kernel = filepath.Join(dir, "installed-core")
	write("cache.db", "runtime cache")
	if got := r.validationInputsHash(); got != initial {
		t.Fatal("identical installed core and changed runtime cache invalidated preflight")
	}
	write("GeoSite.dat", "another geosite")
	if got := r.validationInputsHash(); got == "" || got == initial {
		t.Fatal("changed GeoSite data did not invalidate preflight")
	}
	write("GeoSite.dat", "geosite")
	write("installed-core", "new core")
	if got := r.validationInputsHash(); got == "" || got == initial {
		t.Fatal("changed kernel did not invalidate preflight")
	}
}

func TestDeploymentValidationRecognizesExternalInputs(t *testing.T) {
	for _, config := range []string{
		`{"proxies":[{"type":"ssh","private-key":"keys/id_rsa"}]}`,
		`{"listeners":[{"certificate":"cert.pem","private-key":"key.pem"}]}`,
		`{"proxies":[{"type":"zerotier","planet":"planet"}]}`,
	} {
		dir := t.TempDir()
		t.Setenv("PATH", dir)
		yq := "#!/bin/sh\nprintf '%s' '" + config + "'\n"
		if err := os.WriteFile(filepath.Join(dir, "yq"), []byte(yq), 0700); err != nil {
			t.Fatal(err)
		}
		kernel := filepath.Join(dir, "mihomo")
		if err := os.WriteFile(kernel, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
			t.Fatal(err)
		}
		r := &linuxRuntime{dir: dir, kernel: kernel}
		reusable, err := r.validateForDeployment(context.Background(), filepath.Join(dir, "config.yaml"))
		if err != nil || reusable {
			t.Fatalf("validation may depend on an external file, reusable=%v, err=%v", reusable, err)
		}
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
