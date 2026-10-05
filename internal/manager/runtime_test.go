package manager

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeValidationRejectsProvidersAndMalformedConfig(t *testing.T) {
	dir := t.TempDir()
	// These commands model process boundaries; real kernel acceptance is separate.
	yq := `#!/bin/sh
cat "$3"
`
	mihomo := `#!/bin/sh
printf invoked >> "$VALIDATE_MARKER"
exit 0
`
	for name, b := range map[string]string{"yq": yq, "mihomo": mihomo} {
		if e := os.WriteFile(filepath.Join(dir, name), []byte(b), 0700); e != nil {
			t.Fatal(e)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	marker := filepath.Join(dir, "native-called")
	t.Setenv("VALIDATE_MARKER", marker)
	runtime := &linuxRuntime{dir: dir, output: io.Discard}
	path := filepath.Join(dir, "config.yaml")
	for _, b := range []string{`{"proxy-providers":{"airport":{"url":"https://example.invalid/sub"}}}`, `{"rule-providers":{"rules":{"path":"rules.yaml"}}}`, `null`, `[]`, `bad yaml`} {
		if e := os.WriteFile(path, []byte(b), 0600); e != nil {
			t.Fatal(e)
		}
		if e := runtime.Validate(context.Background(), path); e == nil {
			t.Fatalf("accepted %s", b)
		}
	}
	if _, e := os.Stat(marker); !os.IsNotExist(e) {
		t.Fatal("unsupported configs reached native validation")
	}
	if e := os.WriteFile(path, []byte(`{"mixed-port":7890,"proxy-providers":{}}`), 0600); e != nil {
		t.Fatal(e)
	}
	if e := runtime.Validate(context.Background(), path); e != nil {
		t.Fatal(e)
	}
	if b, e := os.ReadFile(marker); e != nil || !strings.Contains(string(b), "invoked") {
		t.Fatal("native validator not invoked")
	}
}

func TestEnvironmentReaderKeepsValuesAsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if e := os.WriteFile(path, []byte("# comment\nexport MIHOMO_SECRET='example'\nCONFIG_URL=\"https://example.invalid/?a=b\"\nGITHUB_PROXY=$(do-not-execute)\n"), 0600); e != nil {
		t.Fatal(e)
	}
	env, e := readEnvironment(path)
	if e != nil {
		t.Fatal(e)
	}
	if env["MIHOMO_SECRET"] != "example" || env["CONFIG_URL"] != "https://example.invalid/?a=b" || env["GITHUB_PROXY"] != "$(do-not-execute)" {
		t.Fatalf("unexpected parsing: %+v", env)
	}
}

func TestRunningConfigMustMatchBeforeBaselinePromotion(t *testing.T) {
	cases := []struct {
		desired, active string
		matches         bool
	}{
		{`{"mode":"rule","mixed-port":7890}`, `{"mode":"Rule","mixed-port":7890}`, true},
		{`{"mode":"rule","mixed-port":7890}`, `{"mode":"Global","mixed-port":7890}`, false},
		{`{"mixed-port":7890}`, `{"mode":"rule","mixed-port":7890}`, true},
		{`{"mode":"rule","mixed-port":7890}`, `{"mode":"rule","mixed-port":7891}`, false},
		{`{"mode":"rule","ipv6":false}`, `{"mode":"rule","ipv6":true}`, false},
	}
	for _, c := range cases {
		if got := matchesRunningConfig([]byte(c.desired), []byte(c.active)); got != c.matches {
			t.Fatalf("runtime identity got=%v want=%v", got, c.matches)
		}
	}
}
