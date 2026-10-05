package manager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func providerFixture(t *testing.T) *linuxRuntime {
	t.Helper()
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	for name, script := range map[string]string{
		"yq": "#!/bin/bash\ncat \"$3\"\n",
		"curl": `#!/bin/bash
set -eu
[ "$TEST_DOWNLOAD_FAIL" != true ] || exit 6
while [ "$1" != --output ]; do shift; done
printf '%s' "$TEST_PROVIDER_BODY" > "$2"
if [ -n "${HTTP_DOWNLOAD_HEADERS_FILE:-}" ]; then cat "$HTTP_DOWNLOAD_HEADERS_FILE" > "$TEST_HEADERS_CAPTURE"; fi
`,
		"mihomo": "#!/bin/bash\nexit 0\n",
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("TEST_PROVIDER_BODY", "payload:\n  - example.com\n")
	t.Setenv("TEST_DOWNLOAD_FAIL", "false")
	t.Setenv("TEST_HEADERS_CAPTURE", filepath.Join(root, "headers"))
	if err := os.Mkdir(filepath.Join(root, "lib"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"env.sh", "http.sh"} {
		b, err := os.ReadFile(filepath.Join("..", "..", "deploy", "lib", name))
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(root, "lib", name), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("DOWNLOAD_DOH_SERVERS=\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return &linuxRuntime{dir: root, kernel: filepath.Join(bin, "mihomo")}
}
func writeProviderConfig(t *testing.T, dir string, options map[string]interface{}) {
	t.Helper()
	cfg := map[string]interface{}{"rule-providers": options, "rules": []string{"MATCH,DIRECT"}}
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), data, 0600); err != nil {
		t.Fatal(err)
	}
}
func providerOptions(url, path string) map[string]interface{} {
	return map[string]interface{}{"type": "http", "url": url, "path": path, "behavior": "domain", "format": "yaml", "interval": 3600}
}

func TestProviderPreparationIsolatedWithHeadersAndInline(t *testing.T) {
	r := providerFixture(t)
	stage := filepath.Join(r.dir, "candidate")
	if err := os.Mkdir(stage, 0700); err != nil {
		t.Fatal(err)
	}
	old := providerOptions("https://example.invalid/rules", "rules/original.yaml")
	writeProviderConfig(t, r.dir, map[string]interface{}{"remote": old})
	if err := writeProviders(r.dir, map[string][]byte{"rules/original.yaml": []byte("old-cache")}); err != nil {
		t.Fatal(err)
	}
	options := providerOptions("https://example.invalid/rules", "rules/original.yaml")
	options["header"] = map[string]interface{}{"Authorization": []string{"Bearer fixture-token"}}
	writeProviderConfig(t, stage, map[string]interface{}{"remote": options, "inline": map[string]interface{}{"type": "inline", "behavior": "domain", "payload": []string{"example.net"}}})
	if err := r.stageProviders(context.Background(), stage, true); err != nil {
		t.Fatal(err)
	}
	if readFile(t, filepath.Join(r.dir, "rules", "original.yaml")) != "old-cache" {
		t.Fatal("candidate download changed live cache")
	}
	resources, err := r.Resources(filepath.Join(stage, "config.yaml"))
	if err != nil || len(resources) != 1 {
		t.Fatalf("resources=%v error=%v", resources, err)
	}
	data := readFile(t, filepath.Join(stage, "config.yaml"))
	if !strings.Contains(data, "\"type\": \"http\"") || !strings.Contains(data, "\"interval\": 3600") {
		t.Fatal("changed runtime provider refresh semantics")
	}
	if !strings.Contains(readFile(t, filepath.Join(r.dir, "headers")), "Authorization: Bearer fixture-token") {
		t.Fatal("lost HTTP provider headers")
	}
	if readFile(t, filepath.Join(r.dir, ".update-state", "download-dns")) != "system\n" {
		t.Fatal("provider did not save DNS preference in production state")
	}
	if err = r.Validate(context.Background(), filepath.Join(stage, "config.yaml")); err != nil {
		t.Fatal(err)
	}
}

func TestProviderDownloadLogsFullURL(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[failed], func(t *testing.T) {
			r := providerFixture(t)
			var logs bytes.Buffer
			r.output = &logs
			if failed {
				t.Setenv("TEST_DOWNLOAD_FAIL", "true")
			}
			p := providerSpec{options: providerOptions("https://fixture-user:fixture-pass@provider.invalid/fixture-path?token=fixture-query", "unused")}
			p.options["header"] = map[string]interface{}{"Authorization": []interface{}{"Bearer fixture-header"}}
			err := r.downloadProvider(context.Background(), p, filepath.Join(r.dir, "download"))
			if (err != nil) != failed {
				t.Fatalf("err=%v logs=%s", err, &logs)
			}
			if !strings.HasPrefix(logs.String(), "[下载] https://fixture-user:fixture-pass@provider.invalid/fixture-path?token=fixture-query（开始）\n") {
				t.Fatalf("missing full provider URL: %s", &logs)
			}
			if strings.Contains(logs.String(), "fixture-header") {
				t.Fatalf("unexpected provider header in download log: %s", &logs)
			}
		})
	}
}

func TestProviderOfflineFallbackRequiresSameSource(t *testing.T) {
	for _, same := range []bool{true, false} {
		t.Run(map[bool]string{true: "same", false: "changed"}[same], func(t *testing.T) {
			r := providerFixture(t)
			stage := filepath.Join(r.dir, "candidate")
			if err := os.Mkdir(stage, 0700); err != nil {
				t.Fatal(err)
			}
			writeProviderConfig(t, r.dir, map[string]interface{}{"remote": providerOptions("https://old.invalid/rules", "rules/cache")})
			if err := writeProviders(r.dir, map[string][]byte{"rules/cache": []byte("cached")}); err != nil {
				t.Fatal(err)
			}
			expired := time.Now().Add(-2 * time.Hour)
			if err := os.Chtimes(filepath.Join(r.dir, "rules/cache"), expired, expired); err != nil {
				t.Fatal(err)
			}
			url := "https://old.invalid/rules"
			if !same {
				url = "https://new.invalid/rules"
			}
			writeProviderConfig(t, stage, map[string]interface{}{"remote": providerOptions(url, "rules/cache")})
			t.Setenv("TEST_DOWNLOAD_FAIL", "true")
			err := r.stageProviders(context.Background(), stage, true)
			if (err == nil) != same {
				t.Fatalf("error=%v same=%v", err, same)
			}
			if readFile(t, filepath.Join(r.dir, "rules", "cache")) != "cached" {
				t.Fatal("changed live cache")
			}
		})
	}
}

func TestProviderRestoreUsesSnapshotWithoutNetwork(t *testing.T) {
	r := providerFixture(t)
	stage := filepath.Join(r.dir, "rollback")
	if err := os.Mkdir(stage, 0700); err != nil {
		t.Fatal(err)
	}
	writeProviderConfig(t, stage, map[string]interface{}{"remote": providerOptions("https://unreachable.invalid/rules", filepath.Join(r.dir, "rules/original.yaml"))})
	if err := writeProviders(stage, map[string][]byte{"rules/original.yaml": []byte("snapshot")}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_DOWNLOAD_FAIL", "true")
	if err := r.stageProviders(context.Background(), stage, false); err != nil {
		t.Fatal(err)
	}
	resources, err := r.Resources(filepath.Join(stage, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range resources {
		if string(b) != "snapshot" {
			t.Fatal("did not restore captured dependency")
		}
	}
}

type providerRollbackRuntime struct {
	*fakeRuntime
	resources *linuxRuntime
}

func (r *providerRollbackRuntime) Resources(path string) (map[string][]byte, error) {
	return r.resources.Resources(path)
}
func (r *providerRollbackRuntime) Render(ctx context.Context, dir string, b []byte) ([]byte, error) {
	return r.resources.Render(ctx, dir, b)
}

func TestLegacyProviderRollbackDoesNotReuseCandidateCache(t *testing.T) {
	r := providerFixture(t)
	writeProviderConfig(t, r.dir, map[string]interface{}{"remote": providerOptions("https://example.invalid/rules", "rules/original")})
	oldConfig := []byte(readFile(t, filepath.Join(r.dir, "config.yaml")))
	if err := writeProviders(r.dir, map[string][]byte{"rules/original": []byte("old-rules")}); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(r.dir, "scripts"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.dir, "scripts", "update.sh"), []byte("#!/bin/bash\ncp \"$3\" \"$2/config.yaml\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	lock, err := acquireUpdateLock(r.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	u := newConfigUpdater(r.dir, nil)
	p := providerSpec{kind: "rule-providers", name: "remote"}
	f := &fakeRuntime{dir: r.dir, candidate: oldConfig, matches: true, newCIDR: []byte("cidr"),
		resourceCandidate: map[string][]byte{p.managedPath(): []byte("candidate-rules"), "rules/original": []byte("candidate-rules")},
		probeErrors:       []error{nil, errors.New("candidate blocked"), nil}}
	u.runtime = &providerRollbackRuntime{f, r}
	if err = u.begin("provider-generation"); err != nil {
		t.Fatal(err)
	}
	_ = u.RunLocked(context.Background(), true)
	if u.status.Result != "rolled_back" {
		t.Fatalf("did not rollback: %+v", u.status)
	}
	if got := readFile(t, filepath.Join(r.dir, p.managedPath())); got != "old-rules" {
		t.Fatalf("restored candidate dependency: %s", got)
	}
}

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

func TestProviderPathsRejectTraversalSecretsAndSymlinks(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../outside", ".env", "config.yaml", "scripts/update.sh", ".update-state/transaction.json", "linked/rules.yaml"} {
		if _, err := providerPath(root, name); err == nil {
			t.Fatalf("accepted unsafe path %q", name)
		}
	}
	for _, name := range []string{"rules.yaml", "rules/cache", ".update-state/providers/managed.data"} {
		if _, err := providerPath(root, name); err != nil {
			t.Fatalf("rejected resource %q: %v", name, err)
		}
	}
}

func TestProviderFilesFollowConfigRollbackAndCrashRecovery(t *testing.T) {
	for _, recovering := range []bool{false, true} {
		t.Run(map[bool]string{false: "failed-probe", true: "interrupted"}[recovering], func(t *testing.T) {
			u, f := setupUpdater(t)
			f.resourceNames = []string{"rules/cache"}
			f.resourceCandidate = map[string][]byte{"rules/cache": []byte("new-rules")}
			if err := writeProviders(u.dir, map[string][]byte{"rules/cache": []byte("old-rules")}); err != nil {
				t.Fatal(err)
			}
			originalTime := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
			if err := os.Chtimes(filepath.Join(u.dir, "rules/cache"), originalTime, originalTime); err != nil {
				t.Fatal(err)
			}
			if recovering {
				old, err := u.snapshot(u.dir)
				if err != nil {
					t.Fatal(err)
				}
				candidate := old
				candidate.Config = []byte("new")
				candidate.Hash = digest(candidate.Config)
				candidate.Resources = f.resourceCandidate
				candidate.ResourcesHash = resourceHash(candidate.Resources)
				tx := updateTransaction{ID: "crashed", Rollback: old, Candidate: candidate}
				if err = writeJSON(u.path("transaction.json"), tx); err != nil {
					t.Fatal(err)
				}
				if err = u.apply(candidate); err != nil {
					t.Fatal(err)
				}
				_ = u.RecoverLocked()
			} else {
				f.probeErrors = []error{nil, errors.New("blocked"), nil}
				_ = u.RunLocked(context.Background(), true)
			}
			assertResult(t, u, "rolled_back", "old")
			if readFile(t, filepath.Join(u.dir, "rules", "cache")) != "old-rules" {
				t.Fatal("provider was not restored")
			}
			stat, err := os.Stat(filepath.Join(u.dir, "rules/cache"))
			if err != nil || !stat.ModTime().Equal(originalTime) {
				t.Fatalf("rollback refreshed provider timestamp: %v %v", stat, err)
			}
			if f.stops == 0 {
				t.Fatal("restored provider while kernel could still refresh it")
			}
		})
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

func TestSnapshotRejectsCorruptedProviderBytes(t *testing.T) {
	u, _ := setupUpdater(t)
	s, err := u.snapshot(u.dir)
	if err != nil {
		t.Fatal(err)
	}
	s.Resources = map[string][]byte{"rules/cache": []byte("valid")}
	s.ResourcesHash = resourceHash(s.Resources)
	s.Resources["rules/cache"] = []byte("tampered")
	if err = u.apply(s); err == nil {
		t.Fatal("accepted corrupted dependency snapshot")
	}
	if _, err = os.Stat(filepath.Join(u.dir, "rules", "cache")); !os.IsNotExist(err) {
		t.Fatal("wrote corrupt resource")
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

func TestProviderFailurePreservesDownloadAndCacheReasons(t *testing.T) {
	for _, sameSource := range []bool{false, true} {
		t.Run(map[bool]string{false: "no-cache", true: "empty-cache"}[sameSource], func(t *testing.T) {
			r := providerFixture(t)
			t.Setenv("TEST_DOWNLOAD_FAIL", "true")
			stage := filepath.Join(r.dir, "candidate")
			if err := os.Mkdir(stage, 0700); err != nil {
				t.Fatal(err)
			}
			url := "https://example.invalid/rules?token=example"
			options := providerOptions(url, "rules/original.yaml")
			if sameSource {
				writeProviderConfig(t, r.dir, map[string]interface{}{"remote": options})
			}
			writeProviderConfig(t, stage, map[string]interface{}{"remote": options})
			err := r.stageProviders(context.Background(), stage, true)
			if err == nil || !strings.Contains(err.Error(), "rule-providers[remote]") || !strings.Contains(err.Error(), url) {
				t.Fatalf("lost source/reason: %v", err)
			}
			if sameSource && !strings.Contains(err.Error(), "同来源缓存") {
				t.Fatalf("lost unusable cache context: %v", err)
			}
		})
	}
}
