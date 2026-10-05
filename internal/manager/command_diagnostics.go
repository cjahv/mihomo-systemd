package manager

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Shared destinations may be buffers or journal writers; serialize parallel commands.
type synchronizedWriter struct {
	mu     sync.Mutex
	writer io.Writer
}

func (w *synchronizedWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writer.Write(data)
}

func diagnosticDestinationFile(writer io.Writer) *os.File {
	if locked, ok := writer.(*synchronizedWriter); ok {
		writer = locked.writer
	}
	file, _ := writer.(*os.File)
	return file
}

const maxCommandDiagnostics = 32 << 10

var terminalEscape = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)

// A bounded ring retains the last command output without buffering an entire
// download. The original stream remains available in the manager journal.
type commandTail struct {
	mu       sync.Mutex
	data     []byte
	position int
	total    uint64
}

func (t *commandTail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := len(p)
	if n == 0 {
		return 0, nil
	}
	if t.data == nil {
		t.data = make([]byte, maxCommandDiagnostics)
	}
	t.total += uint64(n)
	if len(p) > len(t.data) {
		p = p[len(p)-len(t.data):]
	}
	first := copy(t.data[t.position:], p)
	copy(t.data, p[first:])
	t.position = (t.position + len(p)) % len(t.data)
	return n, nil
}

func (t *commandTail) details() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.total == 0 {
		return ""
	}
	var data []byte
	truncated := t.total > uint64(len(t.data))
	if t.total < uint64(len(t.data)) {
		data = t.data[:t.position]
	} else {
		data = make([]byte, len(t.data))
		n := copy(data, t.data[t.position:])
		copy(data[n:], t.data[:t.position])
	}
	text := strings.TrimSpace(terminalEscape.ReplaceAllString(strings.ToValidUTF8(string(data), "�"), ""))
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	if text == "" {
		return ""
	}
	if truncated {
		return "\n命令输出（已截断，仅保留末尾 32 KiB）：\n" + text
	}
	return "\n命令输出：\n" + text
}

func commandFailure(ctx context.Context, operation string, err error, tail *commandTail) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		err = errors.Join(ctx.Err(), err)
	}
	return fmt.Errorf("%s失败: %w%s", operation, err, tail.details())
}

// Stream human-readable commands as before and attach their bounded diagnostics
// only on failure. Sharing the writer preserves stdout/stderr ordering.
func runDiagnosticCommand(ctx context.Context, operation string, cmd *exec.Cmd, output io.Writer) error {
	tail := &commandTail{}
	var writer io.Writer = tail
	if output != nil {
		writer = io.MultiWriter(tail, output)
	}
	// The capture pipe is not a terminal. Let our download helper inspect a
	// duplicate of the original destination, retaining auto-progress semantics.
	if file := diagnosticDestinationFile(output); file != nil {
		fd := len(cmd.ExtraFiles) + 3
		cmd.ExtraFiles = append(cmd.ExtraFiles, file)
		if cmd.Env == nil {
			cmd.Env = os.Environ()
		}
		cmd.Env = append(cmd.Env, "MIHOMO_DOWNLOAD_TERMINAL_FD="+strconv.Itoa(fd))
	}
	cmd.Stdout, cmd.Stderr = writer, writer
	// A descendant may retain the capture pipe after cancellation. Bound the
	// drain so command deadlines still terminate task observation promptly.
	cmd.WaitDelay = time.Second
	return commandFailure(ctx, operation, cmd.Run(), tail)
}

// Machine-readable stdout must remain separate from stderr (JSON, MainPID, etc.).
func diagnosticCommandOutput(ctx context.Context, operation string, cmd *exec.Cmd) ([]byte, error) {
	var stdout bytes.Buffer
	tail := &commandTail{}
	cmd.Stdout, cmd.Stderr = &stdout, tail
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	return stdout.Bytes(), commandFailure(ctx, operation, err, tail)
}
