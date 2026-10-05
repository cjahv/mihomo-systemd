package manager

import (
	"context"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Optional interactive acceptance serves the production UI and update handler,
// using isolated files and a predictable 5-second candidate failure.
func TestUIAcceptanceServer(t *testing.T) {
	if os.Getenv("MIHOMO_UI_ACCEPTANCE") != "1" {
		t.Skip("interactive UI acceptance is opt-in")
	}
	stop := os.Getenv("MIHOMO_UI_ACCEPTANCE_STOP")
	if stop == "" {
		t.Fatal("MIHOMO_UI_ACCEPTANCE_STOP is required")
	}
	dir := t.TempDir()
	for name, data := range map[string]string{"config.yaml": "old", "cn_cidr.txt": "old-cidr", ".env": "MIHOMO_SECRET=example\nCONFIG_URL=https://example.com/subscription/config.yaml\nSKIP_CNIP=true\nQUIC=false\nLOCAL_LOOPBACK_PROXY=false\nGITHUB_PROXY=\nGITHUB_API_PROXY=\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(dir)
	installJournalFixture(t, `printf '%s\n' '{"__REALTIME_TIMESTAMP":"1791171000000000","MESSAGE":"time=\"2026-10-05T11:30:00.123+08:00\" level=debug msg=\"[DNS] example.com --> 192.0.2.1\""}' '{"__REALTIME_TIMESTAMP":"1791171001000000","MESSAGE":"time=\"2026-10-05T11:30:01.456+08:00\" level=warning msg=\"[UDP] dial example.com:443 failed: timeout\\nretry scheduled\""}'
while true; do
    seconds=$(date +%s)
    timestamp=$(date -u +%Y-%m-%dT%H:%M:%S.123Z)
    printf '{"__REALTIME_TIMESTAMP":"%s000000","MESSAGE":"time=\\"%s\\" level=info msg=\\"[TCP] 192.168.1.10:52480 --> api.example.com:443 match Domain using DIRECT\\""}\n' "$seconds" "$timestamp"
    sleep 1
done
`)
	prepareFailure := os.Getenv("MIHOMO_UI_ACCEPTANCE_FAILURE") == "1"
	if prepareFailure {
		if err := os.Mkdir(filepath.Join(dir, "scripts"), 0700); err != nil {
			t.Fatal(err)
		}
		script := "#!/bin/bash\nprintf '[下载] https://example.invalid/sub?token=example\\n'\nprintf 'curl: (6) Could not resolve host: example.invalid\\n' >&2\nprintf '\033[31m[ERROR] 订阅下载失败，当前配置保持不变\033[0m\\n'\nexit 1\n"
		if err := os.WriteFile(filepath.Join(dir, "scripts", "update.sh"), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	saved := currentSecret()
	setSecret("example")
	defer setSecret(saved)
	h := &managerHandler{updateDir: dir, updaterFactory: func(dir string, _ io.Writer) *configUpdater {
		u := newConfigUpdater(dir, io.Discard)
		u.runtime = &uiAcceptanceRuntime{fakeRuntime: &fakeRuntime{dir: dir, matches: true, candidate: []byte("new"), newCIDR: []byte("new-cidr")}, prepareFailure: prepareFailure}
		return u
	}}
	server := httptest.NewServer(h)
	defer server.Close()
	fmt.Println("UI_ACCEPTANCE_URL=" + server.URL)
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(stop); err == nil {
			s, e := readStatus(dir)
			want := "rolled_back"
			if prepareFailure {
				want = "rejected"
			}
			if e != nil || s.Running || s.Result != want {
				t.Fatalf("interactive acceptance not completed: %+v %v", s, e)
			}
			if readFile(t, filepath.Join(dir, "config.yaml")) != "old" {
				t.Fatal("UI result does not match restored config")
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("interactive acceptance timed out")
}

type uiAcceptanceRuntime struct {
	*fakeRuntime
	prepareFailure bool
}

func (r *uiAcceptanceRuntime) Prepare(ctx context.Context, dir string) error {
	if err := waitContext(ctx, time.Second); err != nil {
		return err
	}
	if r.prepareFailure {
		return (&linuxRuntime{dir: r.dir, output: io.Discard}).Prepare(ctx, dir)
	}
	return r.fakeRuntime.Prepare(ctx, dir)
}
func (r *uiAcceptanceRuntime) Probe(ctx context.Context) error {
	r.probes++
	if r.probes == 2 {
		<-ctx.Done()
		return ctx.Err()
	}
	return waitContext(ctx, 300*time.Millisecond)
}
