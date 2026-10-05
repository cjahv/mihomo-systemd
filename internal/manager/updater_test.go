package manager

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeRuntime struct {
	dir           string
	probeErrors   []error
	probes        int
	restarts      int
	stops         int
	readyErrors   []error
	prepareError  error
	validateError error
	renderError   error
	restartErrors []error
	matches       bool
	candidate     []byte
	newCIDR       []byte
	prepareGate   chan struct{}
	readyDeadline time.Duration
	probeDeadline time.Duration
}

func (f *fakeRuntime) Prepare(ctx context.Context, dir string) error {
	if f.prepareGate != nil {
		select {
		case <-f.prepareGate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if f.prepareError != nil {
		return f.prepareError
	}
	if err := atomicWrite(filepath.Join(dir, "config.yaml"), f.candidate, 0600); err != nil {
		return err
	}
	if err := atomicWrite(filepath.Join(dir, "subscription.yaml"), f.candidate, 0600); err != nil {
		return err
	}
	return atomicWrite(filepath.Join(dir, "cn_cidr.txt"), f.newCIDR, 0600)
}
func (f *fakeRuntime) Render(_ context.Context, _ string, b []byte) ([]byte, error) {
	return b, f.renderError
}
func (f *fakeRuntime) Validate(_ context.Context, path string) error {
	b, _ := os.ReadFile(path)
	if string(b) == string(f.candidate) {
		return f.validateError
	}
	return nil
}
func (f *fakeRuntime) ActiveMatches(context.Context, string, string) bool { return f.matches }
func (f *fakeRuntime) Selections(context.Context) map[string]string {
	return map[string]string{"select": "node-a"}
}
func (f *fakeRuntime) Restart(context.Context) error {
	f.restarts++
	if len(f.restartErrors) >= f.restarts {
		return f.restartErrors[f.restarts-1]
	}
	return nil
}
func (f *fakeRuntime) Stop(context.Context) error { f.stops++; return nil }
func (f *fakeRuntime) Ready(ctx context.Context) error {
	if d, ok := ctx.Deadline(); ok {
		f.readyDeadline = time.Until(d)
	}
	if len(f.readyErrors) >= f.restarts {
		return f.readyErrors[f.restarts-1]
	}
	return nil
}
func (f *fakeRuntime) RestoreSelections(context.Context, map[string]string) error { return nil }
func (f *fakeRuntime) Probe(ctx context.Context) error {
	f.probes++
	if d, ok := ctx.Deadline(); ok {
		f.probeDeadline = time.Until(d)
	}
	if len(f.probeErrors) >= f.probes {
		return f.probeErrors[f.probes-1]
	}
	return nil
}

func setupUpdater(t *testing.T) (*configUpdater, *fakeRuntime) {
	t.Helper()
	dir := t.TempDir()
	for n, v := range map[string]string{"config.yaml": "old", "cn_cidr.txt": "old-cidr", ".env": "MIHOMO_SECRET=example\n"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(v), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, ".env_hash"), []byte(digest([]byte("MIHOMO_SECRET=example\n"))+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	lock, err := acquireUpdateLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lock.Close)
	u := newConfigUpdater(dir, io.Discard)
	f := &fakeRuntime{dir: dir, matches: true, candidate: []byte("new"), newCIDR: []byte("new-cidr")}
	u.runtime = f
	if err = u.begin("test-update"); err != nil {
		t.Fatal(err)
	}
	return u, f
}
func readFile(t *testing.T, path string) string {
	t.Helper()
	b, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	return string(b)
}
func assertResult(t *testing.T, u *configUpdater, result, config string) {
	t.Helper()
	s, e := readStatus(u.dir)
	if e != nil {
		t.Fatal(e)
	}
	if s.Running || s.Result != result {
		t.Fatalf("status=%+v, want %s", s, result)
	}
	if config != "" && readFile(t, filepath.Join(u.dir, "config.yaml")) != config {
		t.Fatal("wrong active config")
	}
}
func seedGood(t *testing.T, u *configUpdater, config string) {
	t.Helper()
	s, e := u.snapshot(u.dir)
	if e != nil {
		t.Fatal(e)
	}
	s.Config = []byte(config)
	s.Raw = []byte(config)
	s.Hash = digest(s.Config)
	s.Verified = true
	s.ID = "old-confirmed"
	if e = writeJSON(u.path("confirmed.json"), s); e != nil {
		t.Fatal(e)
	}
}

func TestUpdateSuccessCommitsAfterProbe(t *testing.T) {
	u, f := setupUpdater(t)
	if e := u.RunLocked(context.Background(), false); e != nil {
		t.Fatal(e)
	}
	assertResult(t, u, "updated", "new")
	good, e := u.knownGood()
	if e != nil || string(good.Raw) != "new" || good.ID != "test-update" {
		t.Fatalf("good=%+v error=%v", good, e)
	}
	if f.probes != 2 || f.restarts != 1 {
		t.Fatalf("probes=%d restarts=%d", f.probes, f.restarts)
	}
	if f.probeDeadline > googleWindow || f.probeDeadline < googleWindow-time.Second {
		t.Fatal("wrong Google deadline")
	}
	if f.readyDeadline > readyWindow || f.readyDeadline < readyWindow-time.Second {
		t.Fatal("wrong readiness deadline")
	}
	if readFile(t, filepath.Join(u.dir, ".config_hash")) != digest([]byte("new"))+"\n" {
		t.Fatal("hash not committed")
	}
	if _, e = os.Stat(u.path("transaction.json")); !os.IsNotExist(e) {
		t.Fatal("journal not removed")
	}
}
func TestUpdateFailureRestoresConfirmedConfigAndCIDR(t *testing.T) {
	u, f := setupUpdater(t)
	f.probeErrors = []error{nil, errors.New("blocked"), nil}
	if e := u.RunLocked(context.Background(), true); e == nil {
		t.Fatal("rollback must retain failure exit semantics")
	}
	assertResult(t, u, "rolled_back", "old")
	if readFile(t, filepath.Join(u.dir, "cn_cidr.txt")) != "old-cidr" {
		t.Fatal("CIDR not restored")
	}
	if readFile(t, filepath.Join(u.dir, ".config_hash")) != digest([]byte("old"))+"\n" {
		t.Fatal("hash does not describe restored bytes")
	}
	if f.restarts != 2 || f.probes != 3 {
		t.Fatal("rollback not verified")
	}
}
func TestUnhealthyBaselineUsesHistoryAndAllowsRepair(t *testing.T) {
	for _, repair := range []bool{false, true} {
		t.Run(map[bool]string{true: "repair", false: "rollback"}[repair], func(t *testing.T) {
			u, f := setupUpdater(t)
			seedGood(t, u, "historical-good")
			f.probeErrors = []error{errors.New("baseline down"), nil, nil}
			if !repair {
				f.probeErrors[1] = errors.New("new down")
			}
			e := u.RunLocked(context.Background(), true)
			if repair {
				if e != nil {
					t.Fatal(e)
				}
				assertResult(t, u, "updated", "new")
			} else {
				if e == nil {
					t.Fatal("missing failure")
				}
				assertResult(t, u, "rolled_back", "historical-good")
			}
			if u.status.Baseline == nil || *u.status.Baseline {
				t.Fatal("baseline must be unhealthy")
			}
		})
	}
}
func TestDiskRuntimeMismatchDoesNotOverwriteGoodSnapshot(t *testing.T) {
	u, f := setupUpdater(t)
	seedGood(t, u, "historical-good")
	f.matches = false
	f.probeErrors = []error{nil, errors.New("blocked"), nil}
	_ = u.RunLocked(context.Background(), true)
	assertResult(t, u, "rolled_back", "historical-good")
}
func TestPreparationFailuresPreserveActiveConfigAndHashes(t *testing.T) {
	for _, kind := range []string{"download", "validation", "render"} {
		t.Run(kind, func(t *testing.T) {
			u, f := setupUpdater(t)
			_ = atomicWrite(filepath.Join(u.dir, ".config_hash"), []byte("sentinel"), 0600)
			switch kind {
			case "download":
				f.prepareError = errors.New("download interrupted")
			case "validation":
				f.validateError = errors.New("undefined DNS proxy")
			case "render":
				f.renderError = errors.New("render failed")
			}
			if e := u.RunLocked(context.Background(), true); e == nil {
				t.Fatal("expected failure")
			}
			assertResult(t, u, "rejected", "old")
			if readFile(t, filepath.Join(u.dir, ".config_hash")) != "sentinel" || f.restarts != 0 {
				t.Fatal("changed active state before validation")
			}
		})
	}
}
func TestLoadingFailuresAlsoRollback(t *testing.T) {
	for _, kind := range []string{"restart", "readiness"} {
		t.Run(kind, func(t *testing.T) {
			u, f := setupUpdater(t)
			if kind == "restart" {
				f.restartErrors = []error{errors.New("start failed"), nil}
			} else {
				f.readyErrors = []error{errors.New("timeout"), nil}
			}
			_ = u.RunLocked(context.Background(), true)
			assertResult(t, u, "rolled_back", "old")
		})
	}
}
func TestRollbackConnectivityFailureHasTruthfulTerminalState(t *testing.T) {
	u, f := setupUpdater(t)
	if err := atomicWrite(filepath.Join(u.dir, ".config_hash"), []byte("previous-applied"), 0600); err != nil {
		t.Fatal(err)
	}
	f.probeErrors = []error{nil, errors.New("new failed"), errors.New("old failed")}
	_ = u.RunLocked(context.Background(), true)
	assertResult(t, u, "recovery_failed", "old")
	if readFile(t, filepath.Join(u.dir, ".config_hash")) != "previous-applied" {
		t.Fatal("failed recovery advanced applied hashes")
	}
	if _, e := os.Stat(u.path("transaction.json")); !os.IsNotExist(e) {
		t.Fatal("restored files must allow another repair update")
	}
}
func TestNoVerifiedHistoryRestoresUnverifiedBackup(t *testing.T) {
	u, f := setupUpdater(t)
	f.probeErrors = []error{errors.New("baseline failed"), errors.New("new failed"), nil}
	_ = u.RunLocked(context.Background(), true)
	assertResult(t, u, "rolled_back", "old")
	good, e := u.knownGood()
	if e != nil || !good.Verified {
		t.Fatal("successful rollback probe should confirm old config")
	}
}
func TestFirstInstallFailureRetractsCandidate(t *testing.T) {
	u, f := setupUpdater(t)
	_ = os.Remove(filepath.Join(u.dir, "config.yaml"))
	f.probeErrors = []error{errors.New("no service"), errors.New("new failed")}
	_ = u.RunLocked(context.Background(), true)
	assertResult(t, u, "recovery_failed", "")
	if _, e := os.Stat(filepath.Join(u.dir, "config.yaml")); !os.IsNotExist(e) {
		t.Fatal("bad initial candidate left on disk")
	}
	if f.stops != 1 {
		t.Fatal("initial failed service not stopped")
	}
}
func TestUnchangedDoesNotRestart(t *testing.T) {
	u, f := setupUpdater(t)
	f.candidate = []byte("old")
	f.newCIDR = []byte("old-cidr")
	if e := u.RunLocked(context.Background(), false); e != nil {
		t.Fatal(e)
	}
	assertResult(t, u, "unchanged", "old")
	if f.restarts != 0 {
		t.Fatal("unchanged restarted")
	}
}
func TestCooldownOnlyBlocksAutomaticRetry(t *testing.T) {
	u, f := setupUpdater(t)
	fingerprint := digest([]byte(digest([]byte("new")) + digest([]byte("MIHOMO_SECRET=example\n")) + digest([]byte("new-cidr"))))
	if e := writeJSON(u.path("failed.json"), failedCandidate{Fingerprint: fingerprint, At: time.Now()}); e != nil {
		t.Fatal(e)
	}
	if e := u.RunLocked(context.Background(), false); e == nil {
		t.Fatal("automatic retry not cooled down")
	}
	assertResult(t, u, "rejected", "old")
	if f.restarts != 0 {
		t.Fatal("cooled candidate restarted")
	}
	if e := u.begin("manual-retry"); e != nil {
		t.Fatal(e)
	}
	if e := u.RunLocked(context.Background(), true); e != nil {
		t.Fatal(e)
	}
	assertResult(t, u, "updated", "new")
}
func TestFailureMetadataCannotPreventRollback(t *testing.T) {
	u, f := setupUpdater(t)
	if e := os.Mkdir(u.path("failed.json"), 0700); e != nil {
		t.Fatal(e)
	}
	f.probeErrors = []error{nil, errors.New("new failed"), nil}
	_ = u.RunLocked(context.Background(), true)
	assertResult(t, u, "rolled_back", "old")
}
func TestRecoveryRollsBackUnconfirmedAndCompletesConfirmedCommit(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{true: "committed", false: "unconfirmed"}[committed], func(t *testing.T) {
			u, f := setupUpdater(t)
			old, e := u.snapshot(u.dir)
			if e != nil {
				t.Fatal(e)
			}
			candidate := old
			candidate.ID = "interrupted"
			candidate.Config = []byte("new")
			candidate.Raw = []byte("new")
			candidate.Hash = digest(candidate.Config)
			tx := updateTransaction{ID: "interrupted", Rollback: old, Candidate: candidate}
			if e = writeJSON(u.path("transaction.json"), tx); e != nil {
				t.Fatal(e)
			}
			if e = u.apply(candidate); e != nil {
				t.Fatal(e)
			}
			if committed {
				candidate.Verified = true
				if e = writeJSON(u.path("confirmed.json"), candidate); e != nil {
					t.Fatal(e)
				}
			}
			e = u.RecoverLocked()
			if committed {
				if e != nil {
					t.Fatal(e)
				}
				assertResult(t, u, "updated", "new")
				if f.restarts != 0 {
					t.Fatal("committed update rolled back")
				}
			} else {
				assertResult(t, u, "rolled_back", "old")
				if f.restarts != 1 {
					t.Fatal("unconfirmed config not reloaded")
				}
			}
		})
	}
}
func TestLockExcludesOtherProcessesAndSettings(t *testing.T) {
	u, _ := setupUpdater(t)
	if _, e := acquireUpdateLock(u.dir); !errors.Is(e, errUpdateBusy) {
		t.Fatalf("lock error=%v", e)
	}
	saved := currentSecret()
	setSecret("example")
	defer setSecret(saved)
	h := &managerHandler{updateDir: u.dir}
	req := httptest.NewRequest("POST", "/save_settings", strings.NewReader(`{"secret":"example","CONFIG_URL":"https://example.invalid/config"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("settings status=%d body=%s", w.Code, w.Body.String())
	}
}
func TestPrivateStatePermissions(t *testing.T) {
	u, _ := setupUpdater(t)
	for name, want := range map[string]os.FileMode{"": 0700, "status.json": 0600, "update.lock": 0600} {
		st, e := os.Stat(u.path(name))
		if e != nil {
			t.Fatal(e)
		}
		if st.Mode().Perm() != want {
			t.Fatalf("%s permission=%o", name, st.Mode().Perm())
		}
	}
}
func TestProbeGoogleDeadlineAndResponseValidation(t *testing.T) {
	for _, kind := range []string{"success", "status", "redirect", "stall", "retry"} {
		t.Run(kind, func(t *testing.T) {
			var mu sync.Mutex
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				calls++
				n := calls
				mu.Unlock()
				switch kind {
				case "success":
					w.WriteHeader(204)
				case "status":
					w.WriteHeader(200)
				case "redirect":
					w.Header().Set("Location", "/ok")
					w.WriteHeader(302)
				case "stall":
					<-r.Context().Done()
				case "retry":
					if n == 1 {
						w.WriteHeader(503)
					} else {
						w.WriteHeader(204)
					}
				}
			}))
			defer server.Close()
			client := server.Client()
			client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
			ctx, cancel := context.WithTimeout(context.Background(), 350*time.Millisecond)
			defer cancel()
			start := time.Now()
			e := probeGoogle(ctx, client, server.URL)
			if kind == "success" || kind == "retry" {
				if e != nil {
					t.Fatal(e)
				}
			} else {
				if e == nil {
					t.Fatal("non-204 accepted")
				}
				if time.Since(start) > time.Second {
					t.Fatal("probe exceeded total window")
				}
			}
		})
	}
}
func TestProbeRejectsUntrustedTLS(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	if e := probeGoogle(ctx, &http.Client{}, server.URL); e == nil {
		t.Fatal("untrusted TLS accepted")
	}
}
func TestBackgroundUpdateSurvivesRequestCancellation(t *testing.T) {
	dir := t.TempDir()
	for name, b := range map[string]string{"config.yaml": "old", "cn_cidr.txt": "old-cidr", ".env": "MIHOMO_SECRET=example\n"} {
		if e := os.WriteFile(filepath.Join(dir, name), []byte(b), 0600); e != nil {
			t.Fatal(e)
		}
	}
	gate := make(chan struct{})
	f := &fakeRuntime{dir: dir, matches: true, candidate: []byte("new"), newCIDR: []byte("new-cidr"), prepareGate: gate}
	h := &managerHandler{updateDir: dir, updaterFactory: func(dir string, _ io.Writer) *configUpdater {
		u := newConfigUpdater(dir, io.Discard)
		u.runtime = f
		return u
	}}
	saved := currentSecret()
	setSecret("example")
	defer setSecret(saved)
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("POST", "/reload", nil).WithContext(ctx)
	req.Header.Set("X-Mihomo-Secret", "example")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	cancel()
	if w.Code != 202 {
		t.Fatalf("status=%d", w.Code)
	}
	second := httptest.NewRecorder()
	h.ServeHTTP(second, req)
	if second.Code != 409 {
		t.Fatal("concurrent web update accepted")
	}
	close(gate)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s, e := readStatus(dir)
		if e == nil && !s.Running && s.Result == "updated" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("request cancellation interrupted background update")
}

func TestRecoveryRefusesCorruptJournal(t *testing.T) {
	u, _ := setupUpdater(t)
	old, e := u.snapshot(u.dir)
	if e != nil {
		t.Fatal(e)
	}
	bad := old
	bad.Config = []byte("tampered")
	if e = writeJSON(u.path("transaction.json"), updateTransaction{ID: "broken", Rollback: old, Candidate: bad}); e != nil {
		t.Fatal(e)
	}
	if e = u.RecoverLocked(); e == nil {
		t.Fatal("corrupt journal accepted")
	}
	if readFile(t, filepath.Join(u.dir, "config.yaml")) != "old" {
		t.Fatal("corrupt journal mutated active file")
	}
}
func TestRecoveryFinishesStatusAfterJournalCleanup(t *testing.T) {
	u, _ := setupUpdater(t)
	seedGood(t, u, "old")
	good, e := u.knownGood()
	if e != nil {
		t.Fatal(e)
	}
	good.ID = u.status.ID
	if e = writeJSON(u.path("confirmed.json"), good); e != nil {
		t.Fatal(e)
	}
	if e = u.RecoverLocked(); e != nil {
		t.Fatal(e)
	}
	assertResult(t, u, "updated", "old")
}
func TestUpdateEndpointsRequireAuthentication(t *testing.T) {
	saved := currentSecret()
	setSecret("example")
	defer setSecret(saved)
	h := &managerHandler{updateDir: t.TempDir()}
	for _, path := range []string{"/reload", "/update_status"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 401 {
			t.Fatalf("%s status=%d", path, w.Code)
		}
	}
}

func TestEnvironmentChangesRequireRestartWithoutSubscriptionChange(t *testing.T) {
	for _, kind := range []string{"changed", "unknown"} {
		t.Run(kind, func(t *testing.T) {
			u, runtime := setupUpdater(t)
			runtime.candidate = []byte("old")
			runtime.newCIDR = []byte("old-cidr")
			if kind == "changed" {
				if err := atomicWrite(filepath.Join(u.dir, ".env"), []byte("MIHOMO_SECRET=example\nQUIC=false\n"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Remove(filepath.Join(u.dir, ".env_hash")); err != nil {
					t.Fatal(err)
				}
			}
			if err := u.RunLocked(context.Background(), true); err != nil {
				t.Fatal(err)
			}
			assertResult(t, u, "updated", "old")
			if runtime.restarts != 1 {
				t.Fatal("environment change was marked applied without restarting entrypoint")
			}
			env, err := os.ReadFile(filepath.Join(u.dir, ".env"))
			if err != nil {
				t.Fatal(err)
			}
			if readFile(t, filepath.Join(u.dir, ".env_hash")) != digest(env)+"\n" {
				t.Fatal("applied environment hash not committed")
			}
		})
	}
}

func TestRecoveryPreservesCompletedRollbackResult(t *testing.T) {
	for _, journalExists := range []bool{false, true} {
		t.Run(map[bool]string{false: "after-cleanup", true: "before-cleanup"}[journalExists], func(t *testing.T) {
			u, runtime := setupUpdater(t)
			old, err := u.snapshot(u.dir)
			if err != nil {
				t.Fatal(err)
			}
			old.ID = u.status.ID + "-rollback"
			old.Verified = true
			if err = writeJSON(u.path("confirmed.json"), old); err != nil {
				t.Fatal(err)
			}
			if err = u.report("verifying_rollback", "checking recovery"); err != nil {
				t.Fatal(err)
			}
			if journalExists {
				candidate := old
				candidate.ID = u.status.ID
				candidate.Config = []byte("new")
				candidate.Hash = digest(candidate.Config)
				if err = writeJSON(u.path("transaction.json"), updateTransaction{ID: u.status.ID, Rollback: old, Candidate: candidate}); err != nil {
					t.Fatal(err)
				}
			}
			if err = u.RecoverLocked(); err != nil {
				t.Fatal(err)
			}
			assertResult(t, u, "rolled_back", "old")
			if runtime.restarts != 0 {
				t.Fatal("verified rollback was applied twice")
			}
		})
	}
}
