package manager

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Each record is a complete journal event. Embedded newlines must not be
// reversed when the browser places new events above older events.
type logRecord struct {
	Type    string `json:"type"`
	Time    string `json:"time,omitempty"`
	Level   string `json:"level,omitempty"`
	Message string `json:"message"`
}

func journalRecord(line []byte) (logRecord, error) {
	var entry struct {
		Message   json.RawMessage `json:"MESSAGE"`
		Timestamp string          `json:"__REALTIME_TIMESTAMP"`
	}
	if err := json.Unmarshal(line, &entry); err != nil {
		return logRecord{}, fmt.Errorf("解析 journal 日志失败: %w", err)
	}
	if len(entry.Message) == 0 || string(entry.Message) == "null" {
		return logRecord{}, nil
	}
	var message string
	if err := json.Unmarshal(entry.Message, &message); err != nil {
		// journalctl encodes binary/non-UTF8 fields as numeric byte arrays.
		var data []byte
		if err := json.Unmarshal(entry.Message, &data); err != nil {
			return logRecord{}, fmt.Errorf("解析 journal MESSAGE 失败: %w", err)
		}
		message = strings.ToValidUTF8(string(data), "�")
	}
	record := logRecord{Type: "log", Message: message}
	if micros, err := strconv.ParseInt(entry.Timestamp, 10, 64); err == nil {
		record.Time = time.UnixMicro(micros).UTC().Format(time.RFC3339Nano)
	}
	return normalizeMihomoLog(record), nil
}

func (h *managerHandler) handleLogs(w http.ResponseWriter, r *http.Request) {
	if !h.isAuthorized(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	cmd := exec.CommandContext(ctx, "journalctl", "--no-pager", "--quiet", "--all", "--output=json", "--lines=1000", "--follow", "--unit=mihomo.service")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		http.Error(w, "无法读取 Mihomo 日志: "+err.Error(), http.StatusInternalServerError)
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		http.Error(w, "无法读取 journal 诊断: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := cmd.Start(); err != nil {
		http.Error(w, "无法启动 journalctl: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	if err := streamJournal(ctx, w, stdout, stderr); err != nil {
		// A failed write or read must terminate the follower before waiting for it.
		cancel()
	}
	if err := cmd.Wait(); err != nil && ctx.Err() == nil {
		_ = json.NewEncoder(w).Encode(logRecord{Type: "error", Message: "journalctl 已退出: " + err.Error()})
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
}

func streamJournal(ctx context.Context, w io.Writer, stdout, stderr io.Reader) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type event struct {
		record logRecord
		err    error
	}
	// Backpressure keeps a slow client from accumulating large journal events.
	events := make(chan event)
	var readers sync.WaitGroup
	for index, reader := range []io.Reader{stdout, stderr} {
		if reader == nil {
			continue
		}
		readers.Add(1)
		go func(diagnostic bool, reader io.Reader) {
			defer readers.Done()
			send := func(e event) bool {
				select {
				case events <- e:
					return true
				case <-ctx.Done():
					return false
				}
			}
			scanner := bufio.NewScanner(reader)
			scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
			for scanner.Scan() {
				var e event
				if diagnostic {
					e.record = logRecord{Type: "error", Message: scanner.Text()}
				} else {
					e.record, e.err = journalRecord(scanner.Bytes())
					if e.err != nil {
						e.record = logRecord{Type: "error", Message: e.err.Error()}
					}
				}
				if e.record.Type != "" && !send(e) {
					return
				}
				if e.err != nil {
					return
				}
			}
			if err := scanner.Err(); err != nil && ctx.Err() == nil {
				send(event{record: logRecord{Type: "error", Message: "读取 journal 失败: " + err.Error()}, err: err})
			}
		}(index == 1, reader)
	}
	go func() { readers.Wait(); close(events) }()
	encoder := json.NewEncoder(w)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case e, ok := <-events:
			if !ok {
				return nil
			}
			if err := encoder.Encode(e.record); err != nil {
				return err
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			if e.err != nil {
				return e.err
			}
		}
	}
}
