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
	for name, data := range map[string]string{"config.yaml": "old", "cn_cidr.txt": "old-cidr", ".env": "MIHOMO_SECRET=example\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	saved := currentSecret()
	setSecret("example")
	defer setSecret(saved)
	h := &managerHandler{updateDir: dir, updaterFactory: func(dir string, _ io.Writer) *configUpdater {
		u := newConfigUpdater(dir, io.Discard)
		u.runtime = &uiAcceptanceRuntime{fakeRuntime: &fakeRuntime{dir: dir, matches: true, candidate: []byte("new"), newCIDR: []byte("new-cidr")}}
		return u
	}}
	server := httptest.NewServer(h)
	defer server.Close()
	fmt.Println("UI_ACCEPTANCE_URL=" + server.URL)
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(stop); err == nil {
			s, e := readStatus(dir)
			if e != nil || s.Running || s.Result != "rolled_back" {
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

type uiAcceptanceRuntime struct{ *fakeRuntime }

func (r *uiAcceptanceRuntime) Prepare(ctx context.Context, dir string) error {
	if err := waitContext(ctx, time.Second); err != nil {
		return err
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
