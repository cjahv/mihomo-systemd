package manager

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestCommandTailRetainsExactSuffixAcrossWrites(t *testing.T) {
	tail := &commandTail{}
	var all []byte
	for _, size := range []int{3, 12000, 30000, 9, 100000, 20000, 400} {
		data := bytes.Repeat([]byte{byte('a' + size%20)}, size)
		all = append(all, data...)
		if n, err := tail.Write(data); err != nil || n != size {
			t.Fatalf("write: %d %v", n, err)
		}
		got := tail.details()
		if len(all) > maxCommandDiagnostics {
			if !strings.Contains(got, "已截断") || !strings.HasSuffix(got, string(all[len(all)-maxCommandDiagnostics:])) {
				t.Fatal("tail differs from exact suffix")
			}
		} else if !strings.HasSuffix(got, string(all)) {
			t.Fatal("lost initial output")
		}
	}
}

func TestDiagnosticCommandStreamsOutputAndPreservesFailure(t *testing.T) {
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, "bash", "-c", "printf 'https://example.invalid/sub?token=example\\n'; printf '\033[31mERROR: missing yq\033[0m\\n' >&2; exit 7")
	var output bytes.Buffer
	err := runDiagnosticCommand(ctx, "更新脚本（update.sh --prepare）", cmd, &output)
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 7 {
		t.Fatalf("lost process cause: %v", err)
	}
	for _, want := range []string{"update.sh --prepare", "exit status 7", "https://example.invalid/sub?token=example", "ERROR: missing yq"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("missing %q in %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "\x1b") || !strings.Contains(output.String(), "\x1b[31m") {
		t.Fatal("diagnostics should be plain text, original journal output should remain intact")
	}
}

func TestDiagnosticCommandBoundsLargeOutputWithoutTruncatingOriginalStream(t *testing.T) {
	ctx := context.Background()
	var output bytes.Buffer
	cmd := exec.CommandContext(ctx, "bash", "-c", "printf '%100000s' ''; printf '\nfinal error: subscription download failed\n' >&2; exit 1")
	err := runDiagnosticCommand(ctx, "准备订阅", cmd, &output)
	if err == nil || !strings.Contains(err.Error(), "已截断") || !strings.Contains(err.Error(), "final error: subscription download failed") {
		t.Fatalf("missing bounded failure details: %v", err)
	}
	if len(err.Error()) > maxCommandDiagnostics+512 || output.Len() < 100000 {
		t.Fatalf("error=%d original=%d", len(err.Error()), output.Len())
	}
	tail := &commandTail{}
	_, _ = tail.Write(bytes.Repeat([]byte("中文"), maxCommandDiagnostics))
	if !utf8.ValidString(tail.details()) {
		t.Fatal("truncation created invalid UTF-8")
	}
}

func TestDiagnosticOutputSeparatesMachineDataAndContextFailure(t *testing.T) {
	ctx := context.Background()
	data, err := diagnosticCommandOutput(ctx, "读取 PID", exec.CommandContext(ctx, "bash", "-c", "printf 42; printf warning >&2"))
	if err != nil || string(data) != "42" {
		t.Fatalf("stderr contaminated stdout: %q %v", data, err)
	}
	_, err = diagnosticCommandOutput(ctx, "systemctl restart mihomo.service", exec.CommandContext(ctx, "bash", "-c", "echo 'Unit has a bad unit file setting' >&2; exit 1"))
	if err == nil || !strings.Contains(err.Error(), "bad unit file setting") {
		t.Fatalf("lost systemctl reason: %v", err)
	}
	timeout, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	err = runDiagnosticCommand(timeout, "加载配置", exec.CommandContext(timeout, "bash", "-c", "echo 'begin validation'; exec sleep 30"), io.Discard)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "begin validation") {
		t.Fatalf("lost timeout or context: %v", err)
	}
}

type failingPrepareRuntime struct {
	*fakeRuntime
	script *linuxRuntime
}

func (r *failingPrepareRuntime) Prepare(ctx context.Context, dir string) error {
	return r.script.Prepare(ctx, dir)
}

func TestPreparationCommandFailureReachesPersistedTaskAndHTTPStatus(t *testing.T) {
	u, fake := setupUpdater(t)
	if err := os.Mkdir(filepath.Join(u.dir, "scripts"), 0700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/bash\nprintf '[下载] https://example.invalid/sub?token=example\\n'\nprintf 'curl: (6) Could not resolve host: example.invalid\\n' >&2\nprintf '[ERROR] 订阅下载失败，当前配置保持不变\\n'\nexit 1\n"
	if err := os.WriteFile(filepath.Join(u.dir, "scripts", "update.sh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	u.runtime = &failingPrepareRuntime{fakeRuntime: fake, script: &linuxRuntime{dir: u.dir, output: io.Discard}}
	err := u.RunLocked(context.Background(), true)
	if err == nil {
		t.Fatal("expected preparation rejection")
	}
	assertResult(t, u, "rejected", "old")
	status, err := readStatus(u.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"准备候选配置失败", "update.sh --prepare", "Could not resolve host", "https://example.invalid/sub?token=example", "订阅下载失败"} {
		if !strings.Contains(status.Message, want) {
			t.Fatalf("status missing %q: %s", want, status.Message)
		}
	}
	secret := currentSecret()
	setSecret("example")
	defer setSecret(secret)
	req := httptest.NewRequest(http.MethodGet, "/update_status", nil)
	req.Header.Set("X-Mihomo-Secret", "example")
	response := httptest.NewRecorder()
	(&managerHandler{updateDir: u.dir}).handleUpdateStatus(response, req)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "Could not resolve host") {
		t.Fatalf("HTTP lost failure: %d %s", response.Code, response.Body.String())
	}
	if fake.restarts != 0 {
		t.Fatal("preparation failure restarted active service")
	}
}

// Also run this test binary directly under a PTY to cover a real terminal;
// go test normally gives the test process a pipe, even in an interactive shell.
func TestDiagnosticTerminalProgress(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "curl-args")
	t.Setenv("DIAGNOSTIC_CURL_ARGS", marker)
	script := `#!/bin/bash
printf '%s\n' "$@" > "$DIAGNOSTIC_CURL_ARGS"
while [ "$1" != --output ]; do shift; done
printf payload > "$2"
printf '\r##### 100%%\r\n' >&2
`
	if err := os.WriteFile(filepath.Join(dir, "curl"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	probe := exec.Command("bash", "-c", "[ -t 1 ]")
	probe.Stdout = os.Stdout
	terminal := probe.Run() == nil
	httpHelper, err := filepath.Abs(filepath.Join("..", "..", "deploy", "lib", "http.sh"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, "bash", "-c", `source "$1"; http_download "$2" 5 1024 https://example.invalid/file`, "test", httpHelper, filepath.Join(dir, "payload"))
	cmd.Env = append(os.Environ(), "DOWNLOAD_PROGRESS=auto", "DOWNLOAD_DOH_SERVERS=", "MIHOMO_DOWNLOAD_TERMINAL=0", "MIHOMO_SOURCE_DIR="+dir)
	if err := runDiagnosticCommand(ctx, "下载进度探测", cmd, os.Stdout); err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Contains(string(args), "--progress-bar"); got != terminal {
		t.Fatalf("progress=%v original terminal=%v args=%s", got, terminal, args)
	}
	t.Logf("original terminal=%v, automatic progress preserved", terminal)
}

func TestDiagnosticCancellationBoundsInheritedPipeDrain(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	dir := t.TempDir()
	childPID := filepath.Join(dir, "child-pid")
	cmd := exec.CommandContext(ctx, "bash", "-c", `echo 'begin'; sleep 30 & echo "$!" > "$1"; wait`, "test", childPID)
	t.Cleanup(func() {
		data, err := os.ReadFile(childPID)
		if err != nil {
			return
		}
		kill := exec.Command("kill", strings.TrimSpace(string(data)))
		_ = kill.Run()
	})
	start := time.Now()
	err := runDiagnosticCommand(ctx, "准备候选", cmd, io.Discard)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "begin") {
		t.Fatalf("lost canceled process details: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("inherited pipe blocked cancellation: %v", elapsed)
	}
}
