package manager

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var errUpdateBusy = errors.New("更新或设置保存正在执行")

const (
	googleWindow   = 5 * time.Second
	readyWindow    = 15 * time.Second
	failedCooldown = 5 * time.Minute
	maxConfigSize  = 16 << 20
)

type updateStatus struct {
	ID            string    `json:"id"`
	Stage         string    `json:"stage"`
	Running       bool      `json:"running"`
	Result        string    `json:"result,omitempty"`
	Message       string    `json:"message"`
	Baseline      *bool     `json:"baseline,omitempty"`
	CandidateHash string    `json:"candidate_hash,omitempty"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// One private, atomically replaced document keeps config bytes and their identity together.
type configSnapshot struct {
	ID            string               `json:"id"`
	Config        []byte               `json:"config"`
	Raw           []byte               `json:"raw"`
	CIDR          []byte               `json:"cidr,omitempty"`
	CIDRPresent   bool                 `json:"cidr_present"`
	CIDRTimestamp []byte               `json:"cidr_timestamp,omitempty"`
	Hash          string               `json:"hash"`
	EnvHash       string               `json:"env_hash"`
	Selections    map[string]string    `json:"selections,omitempty"`
	Verified      bool                 `json:"verified"`
	Resources     map[string][]byte    `json:"resources,omitempty"`
	ResourcesHash string               `json:"resources_hash,omitempty"`
	ResourceTimes map[string]time.Time `json:"resource_times,omitempty"`
}

type updateTransaction struct {
	ID        string         `json:"id"`
	Rollback  configSnapshot `json:"rollback"`
	Candidate configSnapshot `json:"candidate"`
}

type failedCandidate struct {
	Fingerprint string    `json:"fingerprint"`
	At          time.Time `json:"at"`
}

// The updater owns the transaction. Runtime effects are narrow seams for Linux and tests.
type updateRuntime interface {
	Prepare(context.Context, string) error
	Render(context.Context, string, []byte) ([]byte, error)
	Validate(context.Context, string) error
	Resources(string) (map[string][]byte, error)
	ActiveMatches(context.Context, string, string) bool
	Selections(context.Context) map[string]string
	Restart(context.Context) error
	Stop(context.Context) error
	Ready(context.Context) error
	RestoreSelections(context.Context, map[string]string) error
	Probe(context.Context) error
}

type configUpdater struct {
	dir          string
	stateDir     string
	runtime      updateRuntime
	window       time.Duration
	readiness    time.Duration
	status       updateStatus
	output       io.Writer
	prepared     *configSnapshot
	forceRestart bool
	envFile      string
}

func newConfigUpdater(dir string, output io.Writer) *configUpdater {
	return &configUpdater{dir: dir, stateDir: filepath.Join(dir, ".update-state"), runtime: &linuxRuntime{dir: dir, output: output}, window: googleWindow, readiness: readyWindow, output: output}
}

func updateID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprint(time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func digest(b []byte) string                     { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func (u *configUpdater) path(name string) string { return filepath.Join(u.stateDir, name) }

func (u *configUpdater) begin(id string) error {
	u.status = updateStatus{ID: id, Running: true}
	return u.report("baseline", "正在检查当前配置能否访问 Google")
}

func (u *configUpdater) report(stage, message string) error {
	u.status.Stage = stage
	u.status.Message = message
	u.status.UpdatedAt = time.Now().UTC()
	if u.output != nil {
		fmt.Fprintln(u.output, message)
	}
	return writeJSON(u.path("status.json"), u.status)
}

func (u *configUpdater) finish(result, message string) error {
	u.status.Running = false
	u.status.Result = result
	return u.report("finished", message)
}

func readStatus(dir string) (updateStatus, error) {
	var s updateStatus
	err := readJSON(filepath.Join(dir, ".update-state", "status.json"), &s)
	return s, err
}

func (u *configUpdater) snapshot(dir string) (configSnapshot, error) {
	var s configSnapshot
	var err error
	s.Config, err = readOptional(filepath.Join(dir, "config.yaml"))
	if err != nil {
		return s, err
	}
	s.Hash = digest(s.Config)
	s.Raw, err = readOptional(filepath.Join(dir, "subscription.yaml"))
	if err != nil {
		return s, err
	}
	if len(s.Raw) == 0 {
		s.Raw = append([]byte(nil), s.Config...)
	}
	s.CIDR, err = readOptional(filepath.Join(dir, "cn_cidr.txt"))
	if err != nil {
		return s, err
	}
	_, err = os.Stat(filepath.Join(dir, "cn_cidr.txt"))
	s.CIDRPresent = err == nil
	s.CIDRTimestamp, err = readOptional(filepath.Join(dir, ".cidr_timestamp"))
	if err != nil {
		return s, err
	}
	env, err := os.ReadFile(u.environmentPath())
	if err != nil {
		return s, err
	}
	s.EnvHash = digest(env)
	s.Resources, err = u.runtime.Resources(filepath.Join(dir, "config.yaml"))
	if err != nil {
		return s, err
	}
	s.ResourcesHash = resourceHash(s.Resources)
	s.ResourceTimes, err = providerResourceTimes(dir, s.Resources)
	return s, err
}

func (u *configUpdater) knownGood() (configSnapshot, error) {
	var s configSnapshot
	if err := readJSON(u.path("confirmed.json"), &s); err != nil {
		return s, err
	}
	if !s.Verified || len(s.Config) == 0 || s.Hash != digest(s.Config) || s.ResourcesHash != resourceHash(s.Resources) {
		return s, errors.New("可用恢复点的完整性检查失败")
	}
	return s, nil
}

func (u *configUpdater) validateBytes(ctx context.Context, dir string, b []byte) error {
	if len(b) == 0 {
		return errors.New("配置为空")
	}
	path := filepath.Join(dir, "config.yaml")
	if err := atomicWrite(path, b, 0600); err != nil {
		return err
	}
	return u.runtime.Validate(ctx, path)
}

// RunLocked is called only after the shared file lock is acquired. A lost browser
// connection has no influence on ctx; crash recovery relies on transaction.json.
func (u *configUpdater) RunLocked(ctx context.Context, retry bool) (retErr error) {
	defer func() {
		if retErr != nil && u.status.Running {
			// Preserve an outstanding journal; never claim a partial mutation was rejected.
			result := "rejected"
			if _, err := os.Stat(u.path("transaction.json")); err == nil || u.status.Stage == "rolling_back" || u.status.Stage == "verifying_rollback" {
				result = "recovery_failed"
			}
			_ = u.finish(result, retErr.Error())
		}
	}()
	stage, err := os.MkdirTemp(u.stateDir, "candidate-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	before, err := u.snapshot(u.dir)
	if err != nil {
		return err
	}
	currentMatches := len(before.Config) > 0 && u.runtime.ActiveMatches(ctx, filepath.Join(u.dir, "config.yaml"), before.Hash)
	probeCtx, cancel := context.WithTimeout(ctx, u.window)
	baselineErr := u.runtime.Probe(probeCtx)
	cancel()
	baseline := baselineErr == nil
	u.status.Baseline = &baseline
	if !baseline {
		currentMatches = false
	}
	if baseline && currentMatches {
		currentMatches = u.runtime.ActiveMatches(ctx, filepath.Join(u.dir, "config.yaml"), before.Hash)
		after, readErr := readOptional(filepath.Join(u.dir, "config.yaml"))
		currentMatches = currentMatches && readErr == nil && digest(after) == before.Hash
	}
	before.Selections = u.runtime.Selections(ctx)
	good, goodErr := u.knownGood()
	if goodErr != nil && !errors.Is(goodErr, os.ErrNotExist) {
		return goodErr
	}
	appliedEnvBytes, err := readOptional(filepath.Join(u.dir, ".env_hash"))
	if err != nil {
		return err
	}
	appliedEnvHash := strings.TrimSpace(string(appliedEnvBytes))
	if goodErr == nil && good.Hash == before.Hash {
		appliedEnvHash = good.EnvHash
	}
	if baseline && currentMatches {
		if err := writeProviderSnapshot(stage, before.Resources, before.ResourceTimes); err == nil {
			if err := u.validateBytes(ctx, stage, before.Config); err == nil {
				before.ID = u.status.ID + "-baseline"
				before.Verified = true
				// Reuse the original subscription only when its committed runtime bytes match.
				if goodErr == nil && good.Hash == before.Hash {
					before.Raw = good.Raw
				}
				confirmedBaseline := before
				// Reachability verifies config bytes, not that current .env options
				// have already been applied by scripts/entrypoint.sh.
				confirmedBaseline.EnvHash = appliedEnvHash
				if err := writeJSON(u.path("confirmed.json"), confirmedBaseline); err != nil {
					return err
				}
				good = confirmedBaseline
				goodErr = nil
			}
		}
	}
	if baseline {
		err = u.report("preparing", "当前配置可访问 Google；正在准备候选订阅")
	} else {
		err = u.report("preparing", "更新前 Google 已不可达；保留已有可用恢复点并尝试新订阅")
	}
	if err != nil {
		return err
	}
	var candidate configSnapshot
	if u.prepared != nil {
		candidate = *u.prepared
		if candidate.Hash != digest(candidate.Config) || candidate.ResourcesHash != resourceHash(candidate.Resources) {
			return errors.New("预检候选快照完整性检查失败")
		}
		if err = u.materialize(stage, candidate); err != nil {
			return err
		}
	} else {
		if err = u.runtime.Prepare(ctx, stage); err != nil {
			return fmt.Errorf("准备候选配置失败: %w", err)
		}
		candidate, err = u.snapshot(stage)
		if err != nil {
			return err
		}
	}
	// A manual .env edit must not mix generations halfway through a transaction.
	if candidate.EnvHash != before.EnvHash {
		return errors.New("准备期间 .env 已变化，请重新更新")
	}
	if err = u.runtime.Validate(ctx, filepath.Join(stage, "config.yaml")); err != nil {
		return fmt.Errorf("候选配置校验失败: %w", err)
	}
	candidate.ID = u.status.ID
	candidate.Selections = before.Selections
	u.status.CandidateHash = candidate.Hash
	if !u.forceRestart && baseline && currentMatches && before.Hash == candidate.Hash && appliedEnvHash == candidate.EnvHash && equalAux(before, candidate) {
		if err = refreshProviderTimes(u.dir, candidate.Resources, candidate.ResourceTimes); err != nil {
			return err
		}
		candidate.ResourceTimes, err = providerResourceTimes(u.dir, candidate.Resources)
		if err != nil {
			return err
		}
		candidate.Verified = true
		if err = writeJSON(u.path("confirmed.json"), candidate); err != nil {
			return err
		}
		if err = u.saveHashes(candidate); err != nil {
			return err
		}
		return u.finish("unchanged", "配置未变化，当前配置可以访问 Google")
	}
	fingerprint := digest([]byte(candidate.Hash + candidate.EnvHash + digest(candidate.CIDR) + candidate.ResourcesHash))
	var failed failedCandidate
	if !retry && readJSON(u.path("failed.json"), &failed) == nil && failed.Fingerprint == fingerprint && time.Since(failed.At) < failedCooldown {
		return errors.New("同一候选配置近期验收失败，自动更新冷却 5 分钟；人工更新可立即重试")
	}
	rollback := before
	if goodErr == nil {
		rollback = good
	}
	if len(rollback.Config) > 0 {
		// Keep current administrator overrides (including the API secret) on rollback.
		restoreDir, err := os.MkdirTemp(u.stateDir, "restore-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(restoreDir)
		if err := writeProviderSnapshot(restoreDir, rollback.Resources, rollback.ResourceTimes); err != nil {
			return err
		}
		rendered, renderErr := u.runtime.Render(ctx, restoreDir, rollback.Raw)
		if renderErr != nil {
			return fmt.Errorf("准备恢复点失败: %w", renderErr)
		}
		if err = u.validateBytes(ctx, restoreDir, rendered); err != nil {
			return fmt.Errorf("恢复点校验失败: %w", err)
		}
		rollback.Resources, err = u.runtime.Resources(filepath.Join(restoreDir, "config.yaml"))
		if err != nil {
			return err
		}
		rollback.ResourcesHash = resourceHash(rollback.Resources)
		rollback.ResourceTimes, err = providerResourceTimes(restoreDir, rollback.Resources)
		if err != nil {
			return err
		}
		rollback.Config = rendered
		rollback.Hash = digest(rendered)
		rollback.EnvHash = before.EnvHash
		// Auxiliary files belong to this update attempt, not to an older environment.
		rollback.CIDR = before.CIDR
		rollback.CIDRPresent = before.CIDRPresent
		rollback.CIDRTimestamp = before.CIDRTimestamp
	}
	// Reject uncoordinated manual edits instead of overwriting a changed source.
	current, err := u.snapshot(u.dir)
	if err != nil {
		return err
	}
	if current.Hash != before.Hash || current.EnvHash != before.EnvHash || !equalAux(current, before) {
		return errors.New("准备期间活动配置或环境已变化，请重新更新")
	}
	// Apply only captured bytes; rollback rendering has its own dependency directory.
	tx := updateTransaction{ID: u.status.ID, Rollback: rollback, Candidate: candidate}
	if err = writeJSON(u.path("transaction.json"), tx); err != nil {
		return err
	}
	if err = u.report("applying", "正在加载候选配置"); err != nil {
		return err
	}
	if len(candidate.Resources) > 0 || len(rollback.Resources) > 0 {
		err = u.runtime.Stop(ctx)
	}
	if err == nil {
		err = u.apply(candidate)
	}
	if err == nil {
		err = u.loadAndProbe(ctx, candidate.Selections)
	}
	if err != nil {
		_ = writeJSON(u.path("failed.json"), failedCandidate{Fingerprint: fingerprint, At: time.Now().UTC()})
		return u.rollback(tx, fmt.Sprintf("候选配置未通过验收: %v", err))
	}
	candidate.Verified = true
	// confirmed.json is the commit record. Recovery recognizes a crash after commit.
	if err = writeJSON(u.path("confirmed.json"), candidate); err != nil {
		return u.rollback(tx, fmt.Sprintf("无法持久化已确认版本: %v", err))
	}
	if err = u.saveHashes(candidate); err != nil {
		return err
	}
	if err = removeDurable(u.path("transaction.json")); err != nil {
		return err
	}
	_ = removeDurable(u.path("failed.json"))
	return u.finish("updated", "更新成功：新配置已通过 Google 检测")
}

func equalAux(a, b configSnapshot) bool {
	return a.CIDRPresent == b.CIDRPresent && digest(a.CIDR) == digest(b.CIDR) && a.ResourcesHash == b.ResourcesHash
}

func (u *configUpdater) loadAndProbe(ctx context.Context, selections map[string]string) error {
	restartCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err := u.runtime.Restart(restartCtx)
	cancel()
	if err != nil {
		return fmt.Errorf("重启失败: %w", err)
	}
	readyCtx, cancel := context.WithTimeout(ctx, u.readiness)
	err = u.runtime.Ready(readyCtx)
	cancel()
	if err != nil {
		return fmt.Errorf("加载未就绪: %w", err)
	}
	// The Google budget starts at readiness, including selection restoration.
	probeCtx, cancel := context.WithTimeout(ctx, u.window)
	defer cancel()
	stage, message := "probing", "新配置已就绪，正在进行 5 秒 Google 检测"
	if u.status.Stage == "rolling_back" {
		stage, message = "verifying_rollback", "旧配置已就绪，正在检查 Google 是否恢复"
	}
	if err = u.report(stage, message); err != nil {
		return err
	}
	if err = u.runtime.RestoreSelections(probeCtx, selections); err != nil {
		return fmt.Errorf("恢复策略选择失败: %w", err)
	}
	return u.runtime.Probe(probeCtx)
}

func (u *configUpdater) rollback(tx updateTransaction, reason string) error {
	// Rollback gets its own bounded lifetime even if the update deadline expired.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_ = u.report("rolling_back", reason+"；正在恢复本地配置")
	if len(tx.Rollback.Resources) > 0 || len(tx.Candidate.Resources) > 0 {
		if err := u.runtime.Stop(ctx); err != nil {
			return fmt.Errorf("%s\n停止核心以恢复 provider 失败: %w", reason, err)
		}
	}
	if err := u.apply(tx.Rollback); err != nil {
		return fmt.Errorf("%s\n恢复配置文件失败: %w", reason, err)
	}
	var runtimeErr error
	if len(tx.Rollback.Config) == 0 {
		runtimeErr = u.runtime.Stop(ctx)
	} else {
		runtimeErr = u.loadAndProbe(ctx, tx.Rollback.Selections)
	}
	// Files are restored even if Google is still down. Only a successful
	// runtime acceptance may advance the applied hashes.
	if len(tx.Rollback.Config) > 0 && runtimeErr == nil {
		if err := u.saveHashes(tx.Rollback); err != nil {
			return err
		}
	}
	if len(tx.Rollback.Config) == 0 {
		if err := u.finish("recovery_failed", "新配置失败，首次安装没有可回滚配置，已撤销候选配置\n失败原因："+reason); err != nil {
			return err
		}
		if err := removeDurable(u.path("transaction.json")); err != nil {
			return err
		}
		return errors.New(u.status.Message)
	}
	if runtimeErr != nil {
		if err := u.finish("recovery_failed", fmt.Sprintf("旧配置文件已恢复，但运行验收失败: %v\n原更新失败原因：%s", runtimeErr, reason)); err != nil {
			return err
		}
		if err := removeDurable(u.path("transaction.json")); err != nil {
			return err
		}
		return errors.New(u.status.Message)
	}
	restored := tx.Rollback
	restored.ID = tx.ID + "-rollback"
	restored.Verified = true
	if err := writeJSON(u.path("confirmed.json"), restored); err != nil {
		return err
	}
	if err := removeDurable(u.path("transaction.json")); err != nil {
		return err
	}
	if err := u.finish("rolled_back", "更新失败，已恢复旧配置，Google 访问已恢复\n失败原因："+reason); err != nil {
		return err
	}
	return errors.New(u.status.Message)
}

// RecoverLocked runs on manager startup and before each update under the same lock.
func (u *configUpdater) RecoverLocked() error {
	var tx updateTransaction
	err := readJSON(u.path("transaction.json"), &tx)
	if errors.Is(err, os.ErrNotExist) {
		s, e := readStatus(u.dir)
		if e == nil && s.Running {
			u.status = s
			good, goodErr := u.knownGood()
			active, readErr := readOptional(filepath.Join(u.dir, "config.yaml"))
			if goodErr == nil && readErr == nil && good.Hash == digest(active) {
				if good.ID == s.ID {
					return u.finish("updated", "上次更新已完成验收，已恢复任务结果")
				}
				if good.ID == s.ID+"-rollback" {
					return u.finish("rolled_back", "上次更新失败，旧配置已恢复并通过 Google 检测")
				}
			}
			if s.Stage == "verifying_rollback" || s.Stage == "rolling_back" {
				return u.finish("recovery_failed", "旧配置文件已恢复，上次恢复验收结果未持久化，请重新检查")
			}
			return u.finish("rejected", "上次任务在应用配置前中断，当前配置保持不变")
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("读取未完成事务失败: %w", err)
	}
	if tx.ID == "" || tx.Rollback.Hash != digest(tx.Rollback.Config) || tx.Candidate.Hash != digest(tx.Candidate.Config) || tx.Rollback.ResourcesHash != resourceHash(tx.Rollback.Resources) || tx.Candidate.ResourcesHash != resourceHash(tx.Candidate.Resources) {
		return errors.New("未完成事务完整性检查失败")
	}
	u.status = updateStatus{ID: tx.ID, Running: true}
	good, e := u.knownGood()
	active, readErr := readOptional(filepath.Join(u.dir, "config.yaml"))
	if e == nil && (good.ID == tx.ID || good.ID == tx.ID+"-rollback") && readErr == nil && digest(active) == good.Hash {
		if err := u.saveHashes(good); err != nil {
			return err
		}
		if err := removeDurable(u.path("transaction.json")); err != nil {
			return err
		}
		if good.ID == tx.ID+"-rollback" {
			return u.finish("rolled_back", "上次更新失败，已恢复完成验收的回滚记录")
		}
		return u.finish("updated", "已恢复上次完成验收的提交记录")
	}
	// A restart may follow a settings edit; render old subscription with current overrides.
	if len(tx.Rollback.Config) > 0 {
		stage, err := os.MkdirTemp(u.stateDir, "recovery-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(stage)
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := writeProviderSnapshot(stage, tx.Rollback.Resources, tx.Rollback.ResourceTimes); err != nil {
			return err
		}
		rendered, err := u.runtime.Render(ctx, stage, tx.Rollback.Raw)
		if err != nil {
			return err
		}
		if err = u.validateBytes(ctx, stage, rendered); err != nil {
			return err
		}
		tx.Rollback.Resources, err = u.runtime.Resources(filepath.Join(stage, "config.yaml"))
		if err != nil {
			return err
		}
		tx.Rollback.ResourcesHash = resourceHash(tx.Rollback.Resources)
		tx.Rollback.ResourceTimes, err = providerResourceTimes(stage, tx.Rollback.Resources)
		if err != nil {
			return err
		}
		tx.Rollback.Config = rendered
		tx.Rollback.Hash = digest(rendered)
		env, err := os.ReadFile(u.environmentPath())
		if err != nil {
			return err
		}
		tx.Rollback.EnvHash = digest(env)
	}
	return u.rollback(tx, "检测到上次更新未确认")
}

func (u *configUpdater) apply(s configSnapshot) error {
	if s.Hash != digest(s.Config) || s.ResourcesHash != resourceHash(s.Resources) {
		return errors.New("配置快照 hash 不匹配")
	}
	if err := writeProviderSnapshot(u.dir, s.Resources, s.ResourceTimes); err != nil {
		return err
	}
	files := []struct {
		name    string
		data    []byte
		present bool
	}{
		{"config.yaml", s.Config, len(s.Config) > 0},
		{"cn_cidr.txt", s.CIDR, s.CIDRPresent},
		{".cidr_timestamp", s.CIDRTimestamp, len(s.CIDRTimestamp) > 0},
	}
	for _, f := range files {
		var err error
		if f.present {
			err = atomicWrite(filepath.Join(u.dir, f.name), f.data, 0600)
		} else {
			err = removeDurable(filepath.Join(u.dir, f.name))
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (u *configUpdater) saveHashes(s configSnapshot) error {
	for name, value := range map[string]string{".config_hash": s.Hash, ".env_hash": s.EnvHash, ".cidr_hash": digest(s.CIDR)} {
		if err := atomicWrite(filepath.Join(u.dir, name), []byte(value+"\n"), 0600); err != nil {
			return err
		}
	}
	return nil
}

func atomicWrite(path string, b []byte, mode os.FileMode) error {
	return atomicWriteAt(path, b, mode, time.Time{})
}

func atomicWriteAt(path string, b []byte, mode os.FileMode, modified time.Time) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".write-")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	defer f.Close()
	if err = f.Chmod(mode); err != nil {
		return err
	}
	if _, err = f.Write(b); err != nil {
		return err
	}
	if !modified.IsZero() {
		if err = os.Chtimes(name, modified, modified); err != nil {
			return err
		}
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}
func syncDir(path string) error {
	d, e := os.Open(path)
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}
func removeDurable(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}
func writeJSON(path string, v interface{}) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	return atomicWrite(path, b, 0600)
}
func readJSON(path string, v interface{}) error {
	b, e := os.ReadFile(path)
	if e != nil {
		return e
	}
	return json.Unmarshal(b, v)
}
func readOptional(path string) ([]byte, error) {
	f, e := os.Open(path)
	if errors.Is(e, os.ErrNotExist) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, maxConfigSize+1))
	if e == nil && len(b) > maxConfigSize {
		e = errors.New("配置或资源文件超过 16 MiB")
	}
	return b, e
}

func readEnvironment(path string) (map[string]string, error) {
	b, e := os.ReadFile(path)
	if e != nil {
		return nil, e
	}
	env := make(map[string]string)
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '\'' && v[len(v)-1] == '\'' || v[0] == '"' && v[len(v)-1] == '"') {
			v = v[1 : len(v)-1]
		}
		env[k] = v
	}
	return env, nil
}

func (u *configUpdater) environmentPath() string {
	if u.envFile != "" {
		return u.envFile
	}
	return filepath.Join(u.dir, ".env")
}
func (u *configUpdater) materialize(dir string, s configSnapshot) error {
	if err := writeProviderSnapshot(dir, s.Resources, s.ResourceTimes); err != nil {
		return err
	}
	for name, data := range map[string][]byte{"config.yaml": s.Config, "subscription.yaml": s.Raw, "cn_cidr.txt": s.CIDR} {
		if len(data) > 0 {
			if err := atomicWrite(filepath.Join(dir, name), data, 0600); err != nil {
				return err
			}
		}
	}
	return nil
}

// Preparation stores both sides before any executable, service or live config is replaced.
func (u *configUpdater) prepareDeployment(ctx context.Context, dir string) error {
	if _, err := os.Stat(u.path("transaction.json")); !errors.Is(err, os.ErrNotExist) {
		return errors.New("存在未完成配置更新，请先执行 update --recover-only 后重新发布")
	}
	before, err := u.snapshot(u.dir)
	if err != nil {
		return err
	}
	probeCtx, cancel := context.WithTimeout(ctx, u.window)
	before.Verified = len(before.Config) > 0 && u.runtime.ActiveMatches(ctx, filepath.Join(u.dir, "config.yaml"), before.Hash) && u.runtime.Probe(probeCtx) == nil
	cancel()
	if before.Verified {
		before.Verified = u.runtime.ActiveMatches(ctx, filepath.Join(u.dir, "config.yaml"), before.Hash)
	}
	before.Selections = u.runtime.Selections(ctx)
	if err = writeJSON(filepath.Join(dir, "before.json"), before); err != nil {
		return err
	}
	if len(before.Config) > 0 {
		oldDir := filepath.Join(dir, "baseline")
		if err = os.Mkdir(oldDir, 0700); err != nil {
			return err
		}
		if err = u.materialize(oldDir, before); err != nil {
			return err
		}
		if err = u.runtime.Validate(ctx, filepath.Join(oldDir, "config.yaml")); err != nil {
			return fmt.Errorf("新核心无法校验现有恢复配置: %w", err)
		}
	}
	if err = u.runtime.Prepare(ctx, dir); err != nil {
		return err
	}
	candidate, err := u.snapshot(dir)
	if err != nil {
		return err
	}
	if err = u.runtime.Validate(ctx, filepath.Join(dir, "config.yaml")); err != nil {
		return err
	}
	if err = u.checkDeploymentSource(before); err != nil {
		return err
	}
	return writeJSON(filepath.Join(dir, "candidate.json"), candidate)
}

func (u *configUpdater) checkDeploymentSource(before configSnapshot) error {
	current, err := u.snapshot(u.dir)
	if err != nil {
		return err
	}
	// An existing production .env must also match the staged environment copy.
	if env, readErr := os.ReadFile(filepath.Join(u.dir, ".env")); readErr == nil {
		current.EnvHash = digest(env)
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	if before.Hash != current.Hash || before.EnvHash != current.EnvHash || !equalAux(before, current) ||
		digest(before.Raw) != digest(current.Raw) || digest(before.CIDRTimestamp) != digest(current.CIDRTimestamp) {
		return errors.New("预检期间活动文件已变化，尚未安装，请重新发布")
	}
	return nil
}
