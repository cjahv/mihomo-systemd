package manager

import "testing"

func TestMihomoLogFields(t *testing.T) {
	const journalTime = "2026-10-05T12:00:01Z"
	const appTime = "2026-10-05T20:00:00.123456789+08:00"
	for _, test := range []struct{ name, raw, message, level, timestamp string }{
		{"plain", `time="` + appTime + `" level=info msg="[TCP] 127.0.0.1:123 → example.com:443 match Domain using DIRECT"`, "[TCP] 127.0.0.1:123 → example.com:443 match Domain using DIRECT", "info", "2026-10-05T12:00:00.123456789Z"},
		{"escaped multiline", `time="` + appTime + `" level=warning msg="第一行\nquoted \"value\"\nC:\\logs\t\u4e2d"`, "第一行\nquoted \"value\"\nC:\\logs\t中", "warning", "2026-10-05T12:00:00.123456789Z"},
		{"no timestamp", `level=error msg=timeout`, "timeout", "error", journalTime},
		{"extra fields", `time="` + appTime + `" level=debug msg="lookup" host=example.com latency="2 ms"`, `lookup host=example.com latency="2 ms"`, "debug", "2026-10-05T12:00:00.123456789Z"},
		{"colored", "\x1b[33mWARN\x1b[0m[" + appTime + "] [UDP] dial failed\nsecond line", "[UDP] dial failed\nsecond line", "warning", "2026-10-05T12:00:00.123456789Z"},
		{"unknown", "service startup\nsecond line", "service startup\nsecond line", "", journalTime},
		{"unknown level", `level=custom msg="payload"`, `level=custom msg="payload"`, "", journalTime},
		{"broken quoting", `level=info msg="unterminated`, `level=info msg="unterminated`, "", journalTime},
		{"invalid escape", `level=info msg="bad\q"`, `level=info msg="bad\q"`, "", journalTime},
		{"invalid suffix", `level=info msg="message" truncated`, `level=info msg="message" truncated`, "", journalTime},
		{"invalid timestamp", `time="bad" level=info msg="message"`, `time="bad" level=info msg="message"`, "", journalTime},
		{"duplicate field", `level=info msg=one msg=two`, `level=info msg=one msg=two`, "", journalTime},
		{"empty message", `level=info msg=""`, "", "info", journalTime},
		{"terminal controls", "\x1b[31mservice message\x1b[0m", "service message", "", journalTime},
	} {
		t.Run(test.name, func(t *testing.T) {
			record := normalizeMihomoLog(logRecord{Type: "log", Time: journalTime, Message: test.raw})
			if record.Type != "log" || record.Message != test.message || record.Level != test.level || record.Time != test.timestamp {
				t.Fatalf("record=%+v want message=%q level=%s time=%s", record, test.message, test.level, test.timestamp)
			}
		})
	}
}
