package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const googleProbeURL = "https://www.google.com/generate_204"

type linuxRuntime struct {
	dir         string
	output      io.Writer
	pid         string
	packageRoot string
	envFile     string
	kernel      string
}

func (r *linuxRuntime) script(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "bash", append([]string{filepath.Join(r.packageDir(), "scripts", "update.sh")}, args...)...)
	cmd.Dir = r.dir
	cmd.Env = append(os.Environ(), "MIHOMO_UPDATE_INTERNAL=1", "MIHOMO_SOURCE_DIR="+r.dir, "MIHOMO_ENV_FILE="+r.environmentPath(), "MIHOMO_TEST_KERNEL="+r.kernelPath())
	operation := "更新脚本"
	if len(args) > 0 {
		operation += "（update.sh " + args[0] + "）"
	}
	return runDiagnosticCommand(ctx, operation, cmd, r.output)
}
func (r *linuxRuntime) Prepare(ctx context.Context, dir string) error {
	if err := r.script(ctx, "--prepare", dir); err != nil {
		return err
	}
	return r.stageProviders(ctx, dir, true)
}
func (r *linuxRuntime) Render(ctx context.Context, dir string, raw []byte) ([]byte, error) {
	source := filepath.Join(dir, "rollback-source.yaml")
	if err := atomicWrite(source, raw, 0600); err != nil {
		return nil, err
	}
	if err := r.script(ctx, "--render", dir, source); err != nil {
		return nil, err
	}
	if err := r.stageProviders(ctx, dir, false); err != nil {
		return nil, err
	}
	return readOptional(filepath.Join(dir, "config.yaml"))
}

func (r *linuxRuntime) packageDir() string {
	if r.packageRoot != "" {
		return r.packageRoot
	}
	return r.dir
}
func (r *linuxRuntime) environmentPath() string {
	if r.envFile != "" {
		return r.envFile
	}
	return filepath.Join(r.dir, ".env")
}
func (r *linuxRuntime) kernelPath() string {
	if r.kernel != "" {
		return r.kernel
	}
	return "/usr/local/bin/mihomo"
}

func (r *linuxRuntime) Validate(ctx context.Context, path string) error {
	cfg, err := parseConfig(ctx, path)
	if err != nil {
		return err
	}
	items, err := providers(cfg)
	if err != nil {
		return err
	}
	// Validation references only the prepared resource files. It never points at
	// a live HTTP provider cache, while geodata remains under the configured home.
	for _, p := range items {
		name, err := providerPath(r.dir, p.path)
		if err != nil {
			return err
		}
		prepared := filepath.Join(filepath.Dir(path), name)
		data, err := readProvider(filepath.Dir(path), name)
		if err != nil {
			return fmt.Errorf("%s[%s] 的候选依赖 %s 无法读取: %w", p.kind, p.name, prepared, err)
		}
		if len(data) == 0 {
			return fmt.Errorf("%s[%s] 的候选依赖 %s 为空", p.kind, p.name, prepared)
		}
		p.options["type"], p.options["path"] = "file", prepared
	}
	validation := path + ".validation.json"
	b, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	if err = atomicWrite(validation, b, 0600); err != nil {
		return err
	}
	defer os.Remove(validation)
	cmd := exec.CommandContext(ctx, r.kernelPath(), "-t", "-d", r.dir, "-f", validation)
	return runDiagnosticCommand(ctx, "Mihomo 原生配置校验", cmd, r.output)
}

func (r *linuxRuntime) systemctl(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "systemctl", args...)
	b, e := diagnosticCommandOutput(ctx, "systemctl "+strings.Join(args, " "), cmd)
	return strings.TrimSpace(string(b)), e
}
func (r *linuxRuntime) mainPID(ctx context.Context) (string, error) {
	pid, err := r.systemctl(ctx, "show", "mihomo.service", "--property=MainPID", "--value")
	if err != nil {
		return "", err
	}
	n, err := strconv.Atoi(pid)
	if err != nil || n <= 0 {
		return "", errors.New("Mihomo 主进程未运行")
	}
	return pid, nil
}

func (r *linuxRuntime) ActiveMatches(ctx context.Context, path, hash string) bool {
	pid, e := r.mainPID(ctx)
	if e != nil {
		return false
	}
	// During migration, verify that the exact disk file predates this process and
	// that the kernel was actually started with this project's config directory.
	cmdline, e := os.ReadFile(filepath.Join("/proc", pid, "cmdline"))
	if e != nil {
		return false
	}
	args := strings.Split(string(cmdline), "\x00")
	matchesDir := false
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-d" {
			a, _ := filepath.Abs(args[i+1])
			matchesDir = a == r.dir
		}
		if args[i] == "-f" {
			a, _ := filepath.Abs(args[i+1])
			if a != path {
				return false
			}
		}
	}
	if !matchesDir {
		return false
	}
	started, e := r.systemctl(ctx, "show", "mihomo.service", "--property=ExecMainStartTimestampMonotonic", "--value")
	if e != nil {
		return false
	}
	micros, e := strconv.ParseInt(started, 10, 64)
	if e != nil || micros <= 0 {
		return false
	}
	uptime, e := os.ReadFile("/proc/uptime")
	if e != nil {
		return false
	}
	fields := strings.Fields(string(uptime))
	if len(fields) == 0 {
		return false
	}
	seconds, e := strconv.ParseFloat(fields[0], 64)
	if e != nil {
		return false
	}
	startTime := time.Now().Add(-time.Duration(seconds * float64(time.Second))).Add(time.Duration(micros) * time.Microsecond)
	st, e := os.Stat(path)
	if e != nil || !st.ModTime().Before(startTime.Add(-time.Second)) {
		return false
	}
	b, e := readOptional(path)
	if e != nil || digest(b) != hash {
		return false
	}
	desiredBytes, e := exec.CommandContext(ctx, "yq", "-o=json", ".", path).Output()
	if e != nil {
		return false
	}
	activeBytes, e := r.controller(ctx, http.MethodGet, "/configs", nil)
	if e != nil || !matchesRunningConfig(desiredBytes, activeBytes) {
		return false
	}
	endPID, e := r.mainPID(ctx)
	if e != nil || endPID != pid {
		return false
	}
	r.pid = pid
	return true
}

func (r *linuxRuntime) Restart(ctx context.Context) error {
	oldPID, _ := r.mainPID(ctx)
	if _, err := r.systemctl(ctx, "restart", "mihomo.service"); err != nil {
		return err
	}
	pid, err := r.mainPID(ctx)
	if err != nil {
		return err
	}
	if pid == oldPID {
		return errors.New("重启后主进程未变化")
	}
	r.pid = pid
	return nil
}
func (r *linuxRuntime) Stop(ctx context.Context) error {
	_, err := r.systemctl(ctx, "stop", "mihomo.service")
	return err
}

func (r *linuxRuntime) controller(ctx context.Context, method, path string, body io.Reader) ([]byte, error) {
	env, err := readEnvironment(filepath.Join(r.dir, ".env"))
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://127.0.0.1:9900"+path, body)
	if err != nil {
		return nil, err
	}
	if secret := env["MIHOMO_SECRET"]; secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	req.Header.Set("Content-Type", "application/json")
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("控制 API 返回 HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return b, err
}

func (r *linuxRuntime) Ready(ctx context.Context) error {
	cfg, err := parseConfig(ctx, filepath.Join(r.dir, "config.yaml"))
	if err != nil {
		return err
	}
	items, err := providers(cfg)
	if err != nil {
		return err
	}
	var last error
	for {
		pid, err := r.mainPID(ctx)
		if err == nil && pid != r.pid {
			return errors.New("候选配置加载期间主进程退出或重启")
		}
		if err == nil {
			b, e := r.controller(ctx, http.MethodGet, "/configs", nil)
			err = e
			if err == nil {
				var cfg map[string]interface{}
				err = json.Unmarshal(b, &cfg)
				if err == nil && cfg["mixed-port"] != float64(7890) {
					err = errors.New("运行配置的代理端口不符合本地覆写")
				}
				if err == nil {
					c, e := (&net.Dialer{Timeout: 200 * time.Millisecond}).DialContext(ctx, "tcp", "127.0.0.1:7890")
					if e == nil {
						c.Close()
						e = r.providersReady(ctx, items)
						if e == nil {
							return nil
						}
					}
					err = e
				}
			}
		}
		last = err
		if err = waitContext(ctx, 100*time.Millisecond); err != nil {
			return fmt.Errorf("就绪等待超时: %v: %w", last, err)
		}
	}
}

func (r *linuxRuntime) Selections(ctx context.Context) map[string]string {
	b, err := r.controller(ctx, http.MethodGet, "/proxies", nil)
	if err != nil {
		return nil
	}
	var response struct {
		Proxies map[string]struct {
			Type string `json:"type"`
			Now  string `json:"now"`
		}
	}
	if json.Unmarshal(b, &response) != nil {
		return nil
	}
	selected := make(map[string]string)
	for name, p := range response.Proxies {
		if p.Type == "Selector" && p.Now != "" {
			selected[name] = p.Now
		}
	}
	return selected
}
func (r *linuxRuntime) RestoreSelections(ctx context.Context, selected map[string]string) error {
	if len(selected) == 0 {
		return nil
	}
	b, err := r.controller(ctx, http.MethodGet, "/proxies", nil)
	if err != nil {
		return err
	}
	var response struct {
		Proxies map[string]struct {
			Type string   `json:"type"`
			All  []string `json:"all"`
			Now  string   `json:"now"`
		}
	}
	if err = json.Unmarshal(b, &response); err != nil {
		return err
	}
	for name, node := range selected {
		p, ok := response.Proxies[name]
		if !ok || p.Type != "Selector" {
			continue
		}
		exists := false
		for _, n := range p.All {
			if n == node {
				exists = true
				break
			}
		}
		if !exists || p.Now == node {
			continue
		}
		body, _ := json.Marshal(map[string]string{"name": node})
		if _, err = r.controller(ctx, http.MethodPut, "/proxies/"+url.PathEscape(name), strings.NewReader(string(body))); err != nil {
			return err
		}
	}
	return nil
}

func (r *linuxRuntime) Probe(ctx context.Context) error {
	proxy, _ := url.Parse("http://127.0.0.1:7890")
	transport := &http.Transport{Proxy: http.ProxyURL(proxy), DisableKeepAlives: true, TLSHandshakeTimeout: 2 * time.Second}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if err := probeGoogle(ctx, client, googleProbeURL); err != nil {
		return err
	}
	if r.pid != "" {
		pid, err := r.mainPID(ctx)
		if err != nil || pid != r.pid {
			return errors.New("Google 检测期间 Mihomo 主进程已变化")
		}
	}
	return nil
}

func probeGoogle(ctx context.Context, client *http.Client, target string) error {
	var last error
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("Google 检测截止: %v: %w", last, err)
		}
		attempt, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
		req, err := http.NewRequestWithContext(attempt, http.MethodGet, target, nil)
		if err != nil {
			cancel()
			return err
		}
		req.Header.Set("Cache-Control", "no-cache")
		req.Close = true
		resp, err := client.Do(req)
		if err == nil {
			code := resp.StatusCode
			resp.Body.Close()
			if code == http.StatusNoContent && ctx.Err() == nil {
				cancel()
				return nil
			}
			err = fmt.Errorf("HTTP %d，期望 204", code)
		}
		// Do not include request URLs or proxy credentials in persisted diagnostics.
		if err != nil {
			var uerr *url.Error
			if errors.As(err, &uerr) {
				err = uerr.Err
			}
		}
		cancel()
		last = err
		if err = waitContext(ctx, 150*time.Millisecond); err != nil {
			return fmt.Errorf("Google 检测失败: %v: %w", last, err)
		}
	}
}
func waitContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// API changes such as switching Rule to Global can make a broken file appear
// reachable. Do not promote that file as a verified recovery point.
func matchesRunningConfig(desiredJSON, activeJSON []byte) bool {
	var desired, active map[string]interface{}
	if json.Unmarshal(desiredJSON, &desired) != nil || json.Unmarshal(activeJSON, &active) != nil {
		return false
	}
	mode, ok := desired["mode"].(string)
	if !ok {
		mode = "rule"
	}
	actualMode, ok := active["mode"].(string)
	if !ok || !strings.EqualFold(mode, actualMode) {
		return false
	}
	for _, key := range []string{"mixed-port", "port", "socks-port", "tproxy-port", "allow-lan", "ipv6"} {
		if value, exists := desired[key]; exists && value != active[key] {
			return false
		}
	}
	return true
}

func (r *linuxRuntime) providersReady(ctx context.Context, items []providerSpec) error {
	for _, kind := range []string{"rule-providers", "proxy-providers"} {
		var wanted []string
		for _, p := range items {
			if p.kind == kind {
				wanted = append(wanted, p.name)
			}
		}
		if len(wanted) == 0 {
			continue
		}
		endpoint := "/providers/rules"
		if kind == "proxy-providers" {
			endpoint = "/providers/proxies"
		}
		b, err := r.controller(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return err
		}
		var result struct {
			Providers map[string]struct {
				UpdatedAt time.Time `json:"updatedAt"`
			} `json:"providers"`
		}
		if err = json.Unmarshal(b, &result); err != nil {
			return err
		}
		for _, name := range wanted {
			p, ok := result.Providers[name]
			if !ok || p.UpdatedAt.IsZero() {
				return fmt.Errorf("%s 尚未成功初始化", kind)
			}
		}
	}
	return nil
}
