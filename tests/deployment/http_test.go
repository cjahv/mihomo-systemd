package deployment

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Replay resolver, origin and partial-transfer failures without public requests.
func TestHTTPDownloadFallbackAndAtomicReplacement(t *testing.T) {
	for _, tc := range []struct {
		name     string
		attempts int
		failure  bool
	}{
		{"system", 1, false},
		{"dns-failure", 2, false},
		{"first-doh-failure", 3, false},
		{"proxy-failure", 5, false},
		{"all-failure", 6, true},
		{"certificate-failure", 6, true},
		{"disabled", 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			output, events := filepath.Join(dir, "subscription.yaml"), filepath.Join(dir, "events")
			if err := os.WriteFile(output, []byte("active"), 0600); err != nil {
				t.Fatal(err)
			}
			writeDeploymentFixture(t, filepath.Join(dir, "curl"), `#!/bin/bash
set -eu
[ "$1" = --disable ] || exit 99
output=""; resolver=system; resolve=""; budget=""; direct=false; redirect=false
url="${@: -1}"
while [ "$#" -gt 0 ]; do
    case "$1" in
        --output) output="$2"; shift ;;
        --doh-url) resolver="$2"; shift ;;
        --resolve) resolve="$2"; shift ;;
        --max-time) budget="$2"; shift ;;
        --noproxy) [ "$2" = '*' ] || exit 99; direct=true; shift ;;
        --location) redirect=true ;;
        -k|--insecure|--doh-insecure) exit 99 ;;
    esac
    shift
done
[ "$direct" = true ] && [ "$redirect" = true ] && [ "$budget" -gt 0 ] && [ "$budget" -le 60 ] || exit 99
if [ "$resolver" != system ]; then
    [[ "$resolve" = dns.alidns.com:443:223.5.5.5 || "$resolve" = cloudflare-dns.com:443:1.1.1.1 ]] || exit 99
fi
printf '%s %s\n' "$url" "$resolver" >> "$TEST_EVENTS"
printf partial > "$output"
case "$TEST_CASE" in
    all-failure|disabled) exit 6 ;;
    certificate-failure) exit 60 ;;
    proxy-failure) [[ "$url" != https://proxy.invalid/* ]] || exit 22 ;;
esac
if [ "$TEST_CASE" != system ]; then
    [ "$resolver" != system ] || exit 6
fi
if [ "$TEST_CASE" = first-doh-failure ] && [[ "$resolver" = *alidns* ]]; then exit 28; fi
printf complete > "$output"
`)
			module := filepath.Join(repositoryRoot(t), "deploy", "lib", "http.sh")
			cmd := exec.Command("bash", "-c", `source "$1"; github_download "$2" 60 1000 https://proxy.invalid 'https://official.invalid/config?token=sensitive-fixture'`, "download", module, output)
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "TEST_EVENTS="+events, "TEST_CASE="+tc.name, "DOWNLOAD_DOH_SERVERS=dns.alidns.com:223.5.5.5 cloudflare-dns.com:1.1.1.1")
			if tc.name == "disabled" {
				cmd.Env = append(cmd.Env, "DOWNLOAD_DOH_SERVERS=")
			}
			logs, err := cmd.CombinedOutput()
			if (err != nil) != tc.failure {
				t.Fatalf("error=%v logs=%s", err, logs)
			}
			if strings.Contains(string(logs), "sensitive-fixture") {
				t.Fatal("logged private subscription token")
			}
			data, _ := os.ReadFile(output)
			want := "complete"
			if tc.failure {
				want = "active"
			}
			if string(data) != want {
				t.Fatalf("output=%s want=%s", data, want)
			}
			attempts, _ := os.ReadFile(events)
			if count := strings.Count(string(attempts), "\n"); count != tc.attempts {
				t.Fatalf("attempts=%d want=%d: %s", count, tc.attempts, attempts)
			}
			if !strings.HasPrefix(string(attempts), "https://proxy.invalid/") {
				t.Fatal("did not prefer configured origin")
			}
			pending, _ := filepath.Glob(output + ".http.*")
			if len(pending) != 0 {
				t.Fatalf("partial files retained: %v", pending)
			}
		})
	}
}

func TestHTTPDownloadSharesDeadlineAcrossAttempts(t *testing.T) {
	dir := t.TempDir()
	output, events := filepath.Join(dir, "candidate"), filepath.Join(dir, "events")
	// Advancing Bash's SECONDS simulates elapsed transfer time without sleeping.
	module := filepath.Join(repositoryRoot(t), "deploy", "lib", "http.sh")
	cmd := exec.Command("bash", "-c", `source "$1"
curl() {
    while [ "$1" != --max-time ]; do shift; done
    printf '%s\n' "$2" >> "$TEST_EVENTS"
    SECONDS=$((SECONDS + 20))
    return 28
}
http_download "$2" 60 1000 https://example.invalid`, "download", module, output)
	cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "TEST_EVENTS="+events, "DOWNLOAD_DOH_SERVERS=dns.alidns.com:223.5.5.5 cloudflare-dns.com:1.1.1.1")
	if err := cmd.Run(); err == nil {
		t.Fatal("accepted failed transfers")
	}
	attempts, _ := os.ReadFile(events)
	// Remaining budget is divided across remaining attempts, not reset to 60 each time.
	if string(attempts) != "20\n20\n20\n" {
		t.Fatalf("unexpected budget allocation: %s", attempts)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("created target after all attempts failed")
	}
}

func TestPrepareKeepsActiveConfigAfterDNSFailure(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"deploy/scripts/update.sh", "deploy/scripts/entrypoint.sh", "deploy/lib/env.sh", "deploy/lib/http.sh"} {
		copyDeploymentFile(t, dir, name)
	}
	deployDir := filepath.Join(dir, "deploy")
	writeDeploymentFixture(t, filepath.Join(deployDir, ".env"), "CONFIG_URL=https://subscription.invalid/config?token=private-fixture\nSKIP_CNIP=true\nMIHOMO_SECRET=fixture\n")
	writeDeploymentFixture(t, filepath.Join(deployDir, "config.yaml"), "active configuration")
	writeDeploymentFixture(t, filepath.Join(deployDir, "cn_cidr.txt"), "1.0.1.0/24\n")
	stage := filepath.Join(dir, "stage")
	if err := os.Mkdir(stage, 0700); err != nil {
		t.Fatal(err)
	}
	writeDeploymentFixture(t, filepath.Join(dir, "mihomo"), "#!/bin/bash\necho 'Mihomo v1.0.0 linux/amd64'\n")
	writeDeploymentFixture(t, filepath.Join(dir, "curl"), `#!/bin/bash
while [ "$1" != --output ]; do shift; done
printf partial > "$2"
exit 6
`)
	cmd := exec.Command("bash", filepath.Join(deployDir, "scripts", "update.sh"), "--prepare", stage)
	cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "MIHOMO_UPDATE_INTERNAL=1", "DOWNLOAD_DOH_SERVERS=dns.alidns.com:223.5.5.5 cloudflare-dns.com:1.1.1.1")
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "订阅下载失败") {
		t.Fatalf("expected failed subscription: %v %s", err, out)
	}
	if strings.Contains(string(out), "private-fixture") {
		t.Fatal("leaked subscription token")
	}
	active, _ := os.ReadFile(filepath.Join(deployDir, "config.yaml"))
	if string(active) != "active configuration" {
		t.Fatal("overwrote active configuration on failed download")
	}
	cache, _ := os.ReadFile(filepath.Join(stage, "cn_cidr.txt"))
	if string(cache) != "1.0.1.0/24\n" {
		t.Fatal("failed to preserve CIDR cache")
	}
	for _, name := range []string{"subscription.yaml", "config.yaml"} {
		if _, err := os.Stat(filepath.Join(stage, name)); !os.IsNotExist(err) {
			t.Fatalf("retained partial candidate: %s", name)
		}
	}
	partials, _ := filepath.Glob(filepath.Join(stage, "*.http.*"))
	if len(partials) != 0 {
		t.Fatalf("partial files retained: %v", partials)
	}
}

func TestHTTPDownloadRejectsInvalidPayloadAndResolver(t *testing.T) {
	for _, scenario := range []string{"empty", "oversized", "invalid-resolver"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			output := filepath.Join(dir, "candidate")
			writeDeploymentFixture(t, output, "active")
			writeDeploymentFixture(t, filepath.Join(dir, "curl"), `#!/bin/bash
while [ "$1" != --output ]; do shift; done
if [ "$TEST_CASE" = oversized ]; then printf 'oversized' > "$2"; else : > "$2"; fi
`)
			module := filepath.Join(repositoryRoot(t), "deploy", "lib", "http.sh")
			cmd := exec.Command("bash", "-c", `source "$1"; http_download "$2" 60 5 https://example.invalid`, "download", module, output)
			resolver := ""
			if scenario == "invalid-resolver" {
				resolver = "invalid"
			}
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "TEST_CASE="+scenario, "DOWNLOAD_DOH_SERVERS="+resolver)
			if err := cmd.Run(); err == nil {
				t.Fatal("accepted invalid download")
			}
			data, _ := os.ReadFile(output)
			if string(data) != "active" {
				t.Fatal("replaced target with invalid download")
			}
		})
	}
}

// Real libcurl follows internal HTTP redirects but rejects HTTPS downgrades.
func TestHTTPDownloadRedirectProtocols(t *testing.T) {
	curlPath, err := exec.LookPath("curl")
	if err != nil {
		t.Fatal("curl is required for deployment checks")
	}
	content := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("complete")) }))
	defer content.Close()
	for _, secure := range []bool{false, true} {
		name := "http-redirect"
		if secure {
			name = "https-downgrade"
		}
		t.Run(name, func(t *testing.T) {
			redirect := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, content.URL, http.StatusFound) })
			var origin *httptest.Server
			if secure {
				origin = httptest.NewTLSServer(redirect)
			} else {
				origin = httptest.NewServer(redirect)
			}
			defer origin.Close()
			dir := t.TempDir()
			output := filepath.Join(dir, "candidate")
			writeDeploymentFixture(t, output, "active")
			cmd := exec.Command("bash", "-c", `source "$1"; http_download "$2" 10 1000 "$3"`, "download", filepath.Join(repositoryRoot(t), "deploy", "lib", "http.sh"), output, origin.URL)
			cmd.Env = append(os.Environ(), "DOWNLOAD_DOH_SERVERS=")
			if secure {
				caPath := filepath.Join(dir, "ca.pem")
				ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: origin.Certificate().Raw})
				if err := os.WriteFile(caPath, ca, 0600); err != nil {
					t.Fatal(err)
				}
				writeDeploymentFixture(t, filepath.Join(dir, "curl"), "#!/bin/bash\nexec \"$TEST_REAL_CURL\" \"$@\" --cacert \"$TEST_CA\"\n")
				cmd.Env = append(cmd.Env, "PATH="+dir+":"+os.Getenv("PATH"), "TEST_REAL_CURL="+curlPath, "TEST_CA="+caPath)
			}
			logs, err := cmd.CombinedOutput()
			if (err != nil) != secure {
				t.Fatalf("unexpected redirect result: %v %s", err, logs)
			}
			data, _ := os.ReadFile(output)
			want := "complete"
			if secure {
				want = "active"
				if !strings.Contains(string(logs), "curl 1") {
					t.Fatalf("expected protocol rejection, got %s", logs)
				}
			}
			if string(data) != want {
				t.Fatalf("download=%s want=%s", data, want)
			}
		})
	}
}
