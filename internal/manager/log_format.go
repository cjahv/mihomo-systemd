package manager

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

var coloredLogPrefix = regexp.MustCompile(`^(TRAC|DEBU|INFO|WARN|ERRO|FATA|PANI)\[([^\]]+)\] ?([\s\S]*)$`)

// Mihomo uses Logrus TextFormatter. Normalize its plain logfmt and colored
// terminal formats here; the browser receives domain fields, not terminal text.
// Unknown or malformed records retain their message and journal timestamp.
func normalizeMihomoLog(record logRecord) logRecord {
	text := terminalEscape.ReplaceAllString(record.Message, "")
	record.Message = text
	if match := coloredLogPrefix.FindStringSubmatch(text); match != nil {
		stamp, err := time.Parse(time.RFC3339Nano, match[2])
		if err != nil {
			return record
		}
		record.Time = stamp.UTC().Format(time.RFC3339Nano)
		record.Level = logLevel(match[1])
		record.Message = strings.TrimRight(match[3], " ")
		return record
	}
	fields, ok := readLogFields(text)
	if !ok {
		return record
	}
	var message, level, timestamp string
	seen := make(map[string]bool, 3)
	var extra []string
	for _, field := range fields {
		switch field.key {
		case "time", "level", "msg":
			if seen[field.key] {
				return record
			}
			seen[field.key] = true
			switch field.key {
			case "time":
				timestamp = field.value
			case "level":
				level = logLevel(field.value)
			case "msg":
				message = field.value
			}
		default:
			extra = append(extra, field.raw)
		}
	}
	if level == "" || !seen["msg"] {
		return record
	}
	if seen["time"] {
		stamp, err := time.Parse(time.RFC3339Nano, timestamp)
		if err != nil {
			return record
		}
		record.Time = stamp.UTC().Format(time.RFC3339Nano)
	}
	record.Level = level
	record.Message = message
	if len(extra) > 0 {
		record.Message += " " + strings.Join(extra, " ")
	}
	return record
}

func logLevel(level string) string {
	switch strings.ToLower(level) {
	case "trace", "trac":
		return "trace"
	case "debug", "debu":
		return "debug"
	case "info":
		return "info"
	case "warning", "warn":
		return "warning"
	case "error", "erro":
		return "error"
	case "fatal", "fata":
		return "fatal"
	case "panic", "pani":
		return "panic"
	default:
		return ""
	}
}

type logField struct{ key, value, raw string }

// A single linear scan recognizes Logrus's Go-quoted values, including escaped
// quotes, backslashes, Unicode and multiline messages. Splitting on spaces would
// corrupt msg="..."; partial parsing would silently lose malformed suffixes.
func readLogFields(text string) ([]logField, bool) {
	var fields []logField
	for pos := 0; pos < len(text); {
		for pos < len(text) && (text[pos] == ' ' || text[pos] == '\t') {
			pos++
		}
		if pos == len(text) {
			break
		}
		start := pos
		for pos < len(text) && text[pos] != '=' {
			ch := text[pos]
			if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '_' || ch == '.' || ch == '-') {
				return nil, false
			}
			pos++
		}
		if pos == start || pos == len(text) {
			return nil, false
		}
		key := text[start:pos]
		pos++
		valueStart := pos
		var value string
		if pos < len(text) && text[pos] == '"' {
			pos++
			for pos < len(text) && text[pos] != '"' {
				if text[pos] == '\\' {
					pos++
				}
				pos++
			}
			if pos >= len(text) {
				return nil, false
			}
			pos++
			var err error
			value, err = strconv.Unquote(text[valueStart:pos])
			if err != nil {
				return nil, false
			}
		} else {
			for pos < len(text) && text[pos] != ' ' && text[pos] != '\t' {
				pos++
			}
			value = text[valueStart:pos]
		}
		if pos < len(text) && text[pos] != ' ' && text[pos] != '\t' {
			return nil, false
		}
		fields = append(fields, logField{key: key, value: value, raw: text[start:pos]})
	}
	return fields, len(fields) > 0
}
