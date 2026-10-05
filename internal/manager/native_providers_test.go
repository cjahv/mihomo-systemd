package manager

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// This acceptance uses real yq/curl/kernel processes and only loopback HTTP sources.
func TestNativeProviderAcceptance(t *testing.T) {
	kernel, yq := os.Getenv("MIHOMO_TEST_BINARY"), os.Getenv("YQ_TEST_BINARY")
	if kernel == "" || yq == "" {
		t.Skip("set native binaries for provider acceptance")
	}
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(yq, filepath.Join(bin, "yq")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	if err := os.Mkdir(filepath.Join(root, "lib"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"env.sh", "http.sh"} {
		b, err := os.ReadFile(filepath.Join("..", "..", "deploy", "lib", name))
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(root, "lib", name), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("DOWNLOAD_DOH_SERVERS=\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch r.URL.Path {
		case "/rules.yaml":
			fmt.Fprint(w, "payload:\n  - example.com\n")
		case "/rules.txt":
			fmt.Fprint(w, "DOMAIN-SUFFIX,example.net\n")
		case "/proxies.yaml":
			fmt.Fprint(w, "proxies:\n  - name: local-fixture\n    type: socks5\n    server: 127.0.0.1\n    port: 9\n")
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	stage := filepath.Join(root, "candidate")
	if err := os.Mkdir(stage, 0700); err != nil {
		t.Fatal(err)
	}
	controlPort := freePort(t)
	if err := writeProviders(root, map[string][]byte{"rules/local": []byte("payload:\n  - example.local\n")}); err != nil {
		t.Fatal(err)
	}
	cfg := map[string]interface{}{
		"external-controller": "127.0.0.1:" + strconv.Itoa(controlPort), "bind-address": "127.0.0.1", "log-level": "silent", "mode": "rule",
		"rule-providers": map[string]interface{}{
			"local":     map[string]interface{}{"type": "file", "path": "rules/local", "behavior": "domain", "format": "yaml"},
			"domain":    map[string]interface{}{"type": "http", "url": server.URL + "/rules.yaml", "path": "rules/domains", "behavior": "domain", "format": "yaml", "interval": 3600},
			"classical": map[string]interface{}{"type": "http", "url": server.URL + "/rules.txt", "path": "rules/classical", "behavior": "classical", "format": "text", "interval": 3600},
			"inline":    map[string]interface{}{"type": "inline", "behavior": "domain", "payload": []string{"example.org"}},
		},
		"proxy-providers": map[string]interface{}{"airport": map[string]interface{}{"type": "http", "url": server.URL + "/proxies.yaml", "path": "proxies/airport", "interval": 3600, "health-check": map[string]interface{}{"enable": false}}},
		"proxy-groups":    []map[string]interface{}{{"name": "select", "type": "select", "use": []string{"airport"}}},
		"rules":           []string{"RULE-SET,domain,DIRECT", "RULE-SET,classical,DIRECT", "RULE-SET,inline,DIRECT", "MATCH,DIRECT"},
	}
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(stage, "config.yaml"), data, 0600); err != nil {
		t.Fatal(err)
	}
	runtime := &linuxRuntime{dir: root, kernel: kernel, output: io.Discard}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := runtime.stageProviders(ctx, stage, true); err != nil {
		t.Fatal(err)
	}
	resources, err := runtime.Resources(filepath.Join(stage, "config.yaml"))
	if err != nil || len(resources) != 4 {
		t.Fatalf("resources=%d err=%v", len(resources), err)
	}
	if err = runtime.Validate(ctx, filepath.Join(stage, "config.yaml")); err != nil {
		t.Fatal(err)
	}
	times, err := providerResourceTimes(stage, resources)
	if err != nil {
		t.Fatal(err)
	}
	if err = writeProviderSnapshot(root, resources, times); err != nil {
		t.Fatal(err)
	}
	prepared, err := os.ReadFile(filepath.Join(stage, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "config.yaml"), prepared, 0600); err != nil {
		t.Fatal(err)
	}
	// A second preparation must reuse all fresh files without any network request.
	beforeRequests := requests.Load()
	secondStage := t.TempDir()
	if err = os.WriteFile(filepath.Join(secondStage, "config.yaml"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err = runtime.stageProviders(ctx, secondStage, true); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != beforeRequests || beforeRequests != 3 {
		t.Fatalf("fresh providers fetched again: before=%d after=%d", beforeRequests, requests.Load())
	}
	secondResources, err := runtime.Resources(filepath.Join(secondStage, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	secondTimes, err := providerResourceTimes(secondStage, secondResources)
	if err != nil {
		t.Fatal(err)
	}
	for name, modified := range times {
		if !secondTimes[name].Equal(modified) {
			t.Fatalf("cache copy refreshed time for %s", name)
		}
	}
	process := exec.CommandContext(ctx, kernel, "-d", root, "-f", filepath.Join(root, "config.yaml"))
	if err = process.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = process.Process.Kill(); _ = process.Wait() }()
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: time.Second}
	for {
		ready := true
		for _, endpoint := range []string{"rules", "proxies"} {
			response, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/providers/%s", controlPort, endpoint))
			if err != nil {
				ready = false
				break
			}
			var result struct {
				Providers map[string]struct {
					UpdatedAt time.Time `json:"updatedAt"`
					RuleCount int       `json:"ruleCount"`
				} `json:"providers"`
			}
			err = json.NewDecoder(response.Body).Decode(&result)
			response.Body.Close()
			if err != nil {
				ready = false
				break
			}
			if endpoint == "rules" {
				for _, name := range []string{"domain", "classical", "local"} {
					p := providerSpec{kind: "rule-providers", name: name}
					if !result.Providers[name].UpdatedAt.Equal(times[p.managedPath()]) || result.Providers[name].RuleCount != 1 {
						ready = false
					}
				}
			} else if !result.Providers["airport"].UpdatedAt.Equal(times[(providerSpec{kind: "proxy-providers", name: "airport"}).managedPath()]) {
				ready = false
			}
		}
		if ready {
			if requests.Load() != beforeRequests {
				t.Fatalf("kernel fetched fresh staged resources on startup: requests=%d", requests.Load())
			}
			break
		}
		if err = waitContext(ctx, 30*time.Millisecond); err != nil {
			t.Fatal("prepared providers did not initialize in real kernel")
		}
	}
}
