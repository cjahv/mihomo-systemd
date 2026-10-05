package manager

import (
	"bytes"
	"context"
	"encoding/json"
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
)

func TestJournalRecord(t *testing.T) {
	for _, test := range []struct {
		name, input, message, timestamp string
		invalid                         bool
	}{
		{"multiline", `{"MESSAGE":"first\nsecond","__REALTIME_TIMESTAMP":"1000001"}`, "first\nsecond", "1970-01-01T00:00:01.000001Z", false},
		{"mihomo", `{"MESSAGE":"time=\"2026-10-05T20:00:00+08:00\" level=info msg=\"[TCP] example.com\"","__REALTIME_TIMESTAMP":"1000001"}`, "[TCP] example.com", "2026-10-05T12:00:00Z", false},
		{"binary", `{"MESSAGE":[65,255,66]}`, "A�B", "", false},
		{"missing", `{}`, "", "", false},
		{"null", `{"MESSAGE":null}`, "", "", false},
		{"invalid", `{`, "", "", true},
		{"invalid message", `{"MESSAGE":42}`, "", "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			record, err := journalRecord([]byte(test.input))
			if (err != nil) != test.invalid || record.Message != test.message || record.Time != test.timestamp || test.name == "mihomo" && record.Level != "info" {
				t.Fatalf("record=%+v err=%v", record, err)
			}
		})
	}
}

func TestJournalStreamPreservesEventsAndSeparatesDiagnostics(t *testing.T) {
	var out bytes.Buffer
	err := streamJournal(context.Background(), &out, strings.NewReader("{\"MESSAGE\":\"old\"}\n{\"MESSAGE\":\"new\\ncontinuation\"}\n"), strings.NewReader("journal diagnostic\n"))
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(&out)
	var messages, diagnostics []string
	for {
		var record logRecord
		if err := decoder.Decode(&record); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if record.Type == "log" {
			messages = append(messages, record.Message)
		} else {
			diagnostics = append(diagnostics, record.Message)
		}
	}
	if strings.Join(messages, "|") != "old|new\ncontinuation" || strings.Join(diagnostics, "|") != "journal diagnostic" {
		t.Fatalf("messages=%q diagnostics=%q", messages, diagnostics)
	}
}

type failedLogWriter struct{}

func (failedLogWriter) Write([]byte) (int, error) { return 0, errors.New("disconnected") }
func TestJournalStreamReturnsWriteAndReadFailures(t *testing.T) {
	if err := streamJournal(context.Background(), failedLogWriter{}, strings.NewReader("{\"MESSAGE\":\"event\"}\n"), nil); err == nil {
		t.Fatal("ignored disconnected writer")
	}
	var out bytes.Buffer
	if err := streamJournal(context.Background(), &out, strings.NewReader("malformed\n"), nil); err == nil || !strings.Contains(out.String(), `"type":"error"`) {
		t.Fatalf("missing typed read error: %s %v", out.String(), err)
	}
}

func installJournalFixture(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "journalctl"), []byte("#!/bin/sh\n"+script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func TestLogsHTTPProtocolAndCancellation(t *testing.T) {
	dir := installJournalFixture(t, `printf '%s\n' "$@" > "$JOURNAL_ARGS"
printf '%s\n' '{"MESSAGE":"older"}' '{"MESSAGE":"newer\nsecond line"}'
exec tail -f /dev/null
`)
	argsFile := filepath.Join(dir, "args")
	t.Setenv("JOURNAL_ARGS", argsFile)
	saved := currentSecret()
	setSecret("logs-test")
	defer setSecret(saved)
	finished := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(finished)
		(&managerHandler{}).handleLogs(w, r)
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/logs", nil)
	req.Header.Set("X-Mihomo-Secret", "logs-test")
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 || response.Header.Get("Content-Type") != "application/x-ndjson; charset=utf-8" {
		t.Fatalf("status=%d headers=%v", response.StatusCode, response.Header)
	}
	decoder := json.NewDecoder(response.Body)
	for _, want := range []string{"older", "newer\nsecond line"} {
		var record logRecord
		if err := decoder.Decode(&record); err != nil || record.Message != want {
			t.Fatalf("record=%+v err=%v", record, err)
		}
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(args) != "--no-pager\n--quiet\n--all\n--output=json\n--lines=1000\n--follow\n--unit=mihomo.service\n" {
		t.Fatalf("args=%q", args)
	}
	cancel()
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("disconnected browser left journal follower running")
	}
}

func TestLogsStartFailureAndAuthentication(t *testing.T) {
	saved := currentSecret()
	setSecret("logs-test")
	defer setSecret(saved)
	t.Setenv("PATH", t.TempDir())
	for _, authorized := range []bool{false, true} {
		req := httptest.NewRequest(http.MethodGet, "/logs", nil)
		if authorized {
			req.Header.Set("X-Mihomo-Secret", "logs-test")
		}
		response := httptest.NewRecorder()
		(&managerHandler{}).handleLogs(response, req)
		want := http.StatusUnauthorized
		if authorized {
			want = http.StatusInternalServerError
		}
		if response.Code != want {
			t.Fatalf("authorized=%v status=%d", authorized, response.Code)
		}
	}
}

func TestLogUIRegression(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js unavailable for browser controller regression")
	}
	cmd := exec.Command(node, "--test", "testdata/log_view_test.cjs")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("UI regression: %v\n%s", err, out)
	}
}
