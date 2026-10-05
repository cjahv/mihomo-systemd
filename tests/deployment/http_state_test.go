package deployment

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const aliDNS = "dns.alidns.com:223.5.5.5"
const cloudDNS = "cloudflare-dns.com:1.1.1.1"

func dnsDownload(t *testing.T, root, success, configured string) *exec.Cmd {
	t.Helper()
	// Simulate separate installer/update processes with different working directories.
	work := t.TempDir()
	cmd := exec.Command("bash", "-euc", `source "$1"; http_download "$2" 30 1000 https://source.invalid/file`, "download", filepath.Join(repositoryRoot(t), "deploy/lib/http.sh"), filepath.Join(root, "candidate"))
	cmd.Dir = work
	cmd.Env = append(os.Environ(), "PATH="+root+":"+os.Getenv("PATH"), "MIHOMO_SOURCE_DIR="+root, "DOWNLOAD_PROGRESS=off", "DOWNLOAD_DOH_SERVERS="+configured, "TEST_EVENTS="+filepath.Join(root, "events"), "TEST_DNS_SUCCESS="+success)
	return cmd
}

func dnsFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeDeploymentFixture(t, filepath.Join(root, "curl"), `#!/bin/bash
set -eu
mode=system
while [ "$#" -gt 0 ]; do
 case "$1" in
  --doh-url) mode="$2"; shift ;;
  --output) output="$2"; shift ;;
 esac
 shift
done
echo "$mode" >> "$TEST_EVENTS"
printf partial > "$output"
case "$TEST_DNS_SUCCESS" in
 system) [ "$mode" = system ] || exit 6 ;;
 ali) [[ "$mode" = *alidns* ]] || exit 6 ;;
 cloud) [[ "$mode" = *cloudflare* ]] || exit 6 ;;
 none) exit 6 ;;
 invalid-body) : > "$output"; exit 0 ;;
esac
printf complete > "$output"
`)
	return root
}

func TestDNSPreferencePersistsAcrossDownloadsAndProcesses(t *testing.T) {
	root := dnsFixture(t)
	configured := aliDNS + " " + cloudDNS
	first, err := dnsDownload(t, root, "ali", configured).CombinedOutput()
	if err != nil {
		t.Fatalf("first download: %v %s", err, first)
	}
	cache := filepath.Join(root, ".update-state/download-dns")
	data, _ := os.ReadFile(cache)
	if string(data) != aliDNS+"\n" {
		t.Fatalf("cache=%s", data)
	}
	if err = os.WriteFile(filepath.Join(root, "events"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	second, err := dnsDownload(t, root, "ali", configured).CombinedOutput()
	if err != nil {
		t.Fatalf("second download: %v %s", err, second)
	}
	attempts, _ := os.ReadFile(filepath.Join(root, "events"))
	if string(attempts) != "https://dns.alidns.com/dns-query\n" {
		t.Fatalf("did not reuse working resolver directly: %s %s", attempts, second)
	}
	want := "[下载] https://source.invalid/file（开始）\n[下载] https://source.invalid/file（完成，8 字节）\n"
	if string(second) != want {
		t.Fatalf("successful download must describe the URL, without DNS chatter: %q", second)
	}
	if !strings.Contains(string(first), "[下载] https://source.invalid/file（请求失败，curl 6；解析：系统 DNS）") {
		t.Fatalf("fallback diagnostic lost its download URL: %q", first)
	}
	st, err := os.Stat(cache)
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatalf("cache permissions: %v %v", st, err)
	}
	leftovers, _ := filepath.Glob(filepath.Join(root, ".update-state/.download-dns.*"))
	if len(leftovers) != 0 {
		t.Fatalf("cache temporaries retained: %v", leftovers)
	}
}

func TestDownloadURLLoggingPreservesFullAddress(t *testing.T) {
	for _, shell := range []string{"bash", "/bin/bash"} {
		for _, tc := range []struct {
			name, url, display string
		}{
			{"public-artifact", "https://gh.invalid/https://github.com/MetaCubeX/mihomo/releases/download/v1.2.3/mihomo.gz", "https://gh.invalid/https://github.com/MetaCubeX/mihomo/releases/download/v1.2.3/mihomo.gz"},
			{"credentials", "https://fixture-user:fixture-pass@source.invalid:8443/file?token=fixture-query#fixture-fragment", "https://fixture-user:fixture-pass@source.invalid:8443/file?token=fixture-query#fixture-fragment"},
			{"subscription-path", "https://source.invalid/link/fixture-path?opaque=fixture-query", "https://source.invalid/link/fixture-path?opaque=fixture-query"},
			{"ipv6", "https://fixture-user:fixture-pass@[::1]:8443/fixture-path", "https://fixture-user:fixture-pass@[::1]:8443/fixture-path"},
			{"authority-only", "https://source.invalid?fixture-query", "https://source.invalid?fixture-query"},
			{"control-characters", "https://source.invalid/file\n\033[31m", "https://source.invalid/file[31m"},
		} {
			t.Run(shell+"/"+tc.name, func(t *testing.T) {
				root := t.TempDir()
				writeDeploymentFixture(t, filepath.Join(root, "curl"), `#!/bin/bash
set -eu
while [ "$#" -gt 0 ]; do
 case "$1" in --output) output="$2"; shift ;; --url) printf '%s' "$2" > "$TEST_URL_CAPTURE"; shift ;; esac
 shift
done
printf complete > "$output"
`)
				cmd := exec.Command(shell, "-euc", `source "$1"; http_download "$2" 30 1000 "$3"`, "download", filepath.Join(repositoryRoot(t), "deploy/lib/http.sh"), filepath.Join(root, "candidate"), tc.url)
				cmd.Dir = root
				cmd.Env = append(os.Environ(), "PATH="+root+":"+os.Getenv("PATH"), "MIHOMO_SOURCE_DIR="+root, "DOWNLOAD_DOH_SERVERS=", "DOWNLOAD_PROGRESS=off", "TEST_URL_CAPTURE="+filepath.Join(root, "url"))
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("download: %v %s", err, out)
				}
				want := "[下载] " + tc.display + "（开始）\n[下载] " + tc.display + "（完成，8 字节）\n"
				if string(out) != want {
					t.Fatalf("incomplete URL logging: got %q want %q", out, want)
				}
				actual, _ := os.ReadFile(filepath.Join(root, "url"))
				if string(actual) != tc.url {
					t.Fatalf("log formatting modified the actual request: %q", actual)
				}
			})
		}
	}
}

func TestDNSPreferenceRefreshAndInvalidation(t *testing.T) {
	for _, tc := range []struct {
		name, cached, configured, success, attempts, want string
		failed                                            bool
	}{
		{"system-recovers", aliDNS, aliDNS + " " + cloudDNS, "system", "https://dns.alidns.com/dns-query\nsystem\n", "system", false},
		{"switch-to-cloud", aliDNS, aliDNS + " " + cloudDNS, "cloud", "https://dns.alidns.com/dns-query\nsystem\nhttps://cloudflare-dns.com/dns-query\n", cloudDNS, false},
		{"disabled-doh", aliDNS, "", "system", "system\n", "system", false},
		{"changed-bootstrap", aliDNS, "dns.alidns.com:8.8.8.8", "ali", "system\nhttps://dns.alidns.com/dns-query\n", "dns.alidns.com:8.8.8.8", false},
		{"invalid-cache", "$(invalid)", aliDNS, "ali", "system\nhttps://dns.alidns.com/dns-query\n", aliDNS, false},
		{"all-failed", aliDNS, aliDNS + " " + cloudDNS, "none", "https://dns.alidns.com/dns-query\nsystem\nhttps://cloudflare-dns.com/dns-query\n", aliDNS, true},
		{"invalid-body", aliDNS, aliDNS, "invalid-body", "https://dns.alidns.com/dns-query\nsystem\n", aliDNS, true},
		{"duplicate-config", aliDNS, aliDNS + " " + aliDNS, "none", "https://dns.alidns.com/dns-query\nsystem\n", aliDNS, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := dnsFixture(t)
			cache := filepath.Join(root, ".update-state/download-dns")
			writeDeploymentFixture(t, cache, tc.cached+"\n")
			writeDeploymentFixture(t, filepath.Join(root, "candidate"), "active")
			out, err := dnsDownload(t, root, tc.success, tc.configured).CombinedOutput()
			if (err != nil) != tc.failed {
				t.Fatalf("err=%v output=%s", err, out)
			}
			attempts, _ := os.ReadFile(filepath.Join(root, "events"))
			if string(attempts) != tc.attempts {
				t.Fatalf("attempts=%s want=%s", attempts, tc.attempts)
			}
			data, _ := os.ReadFile(cache)
			if string(data) != tc.want+"\n" {
				t.Fatalf("cache=%s want=%s", data, tc.want)
			}
			if tc.failed {
				data, _ := os.ReadFile(filepath.Join(root, "candidate"))
				if string(data) != "active" {
					t.Fatal("failed download changed target")
				}
			}
		})
	}
}

func TestDNSCacheWriteFailureDoesNotFailValidDownload(t *testing.T) {
	root := dnsFixture(t)
	writeDeploymentFixture(t, filepath.Join(root, ".update-state"), "blocked")
	out, err := dnsDownload(t, root, "system", "").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "DNS 优选记录无法保存") {
		t.Fatalf("err=%v out=%s", err, out)
	}
	data, _ := os.ReadFile(filepath.Join(root, "candidate"))
	if string(data) != "complete" {
		t.Fatal("lost successful download")
	}
}

func TestPreferredDNSRouteUsesRemainingDeadline(t *testing.T) {
	root := t.TempDir()
	writeDeploymentFixture(t, filepath.Join(root, ".update-state/download-dns"), aliDNS+"\n")
	cmd := exec.Command("bash", "-euc", `source "$1"
curl() {
 local limit="" output="" doh=false
 while [ "$#" -gt 0 ]; do
  case "$1" in --max-time) limit="$2"; shift ;; --output) output="$2"; shift ;; --doh-url) doh=true; shift ;; esac
  shift
 done
 echo "$limit" >> "$TEST_EVENTS"
 if [ "$doh" = true ]; then SECONDS=$((SECONDS + 2)); return 6; fi
 printf complete > "$output"
}
http_download "$2" 60 1000 https://source.invalid`, "download", filepath.Join(repositoryRoot(t), "deploy/lib/http.sh"), filepath.Join(root, "candidate"))
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "MIHOMO_SOURCE_DIR="+root, "DOWNLOAD_PROGRESS=off", "DOWNLOAD_DOH_SERVERS="+aliDNS+" "+cloudDNS, "TEST_EVENTS="+filepath.Join(root, "events"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("err=%v out=%s", err, out)
	}
	data, _ := os.ReadFile(filepath.Join(root, "events"))
	if string(data) != "60\n29\n" {
		t.Fatalf("reset or unnecessarily divided cached route budget: %s", data)
	}
}

func TestDownloadProgressModesAndPrivateErrors(t *testing.T) {
	for _, tc := range []struct {
		name, setting, terminal string
		bar, failed             bool
	}{
		{"forced", "bar", "0", true, false}, {"ssh-auto", "auto", "1", true, false},
		{"log-auto", "auto", "0", false, false}, {"off", "off", "1", false, false},
		{"failed-bar", "bar", "0", true, true}, {"invalid-setting", "invalid", "0", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeDeploymentFixture(t, filepath.Join(root, "curl"), `#!/bin/bash
set -eu
echo invoked >> "$TEST_EVENTS"
printf '\r#### 50.0%%\rcurl: (6) Could not resolve https://private.invalid/?token=private-fixture Authorization: fixture-secret\r' >&2
while [ "$1" != --output ]; do shift; done
printf partial > "$2"
[ "$TEST_FAIL" != true ] || exit 6
printf complete > "$2"
printf '\r######## 100.0%%\n' >&2
`)
			output := filepath.Join(root, "candidate")
			writeDeploymentFixture(t, output, "active")
			cmd := exec.Command("bash", "-euc", `source "$1"; http_download "$2" 30 1000 'https://source.invalid/?token=private-fixture'`, "download", filepath.Join(repositoryRoot(t), "deploy/lib/http.sh"), output)
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "PATH="+root+":"+os.Getenv("PATH"), "MIHOMO_SOURCE_DIR="+root, "DOWNLOAD_PROGRESS="+tc.setting, "DOWNLOAD_DOH_SERVERS=", "MIHOMO_DOWNLOAD_TERMINAL="+tc.terminal, "TEST_EVENTS="+filepath.Join(root, "events"), "TEST_FAIL="+strconv.FormatBool(tc.failed))
			out, err := cmd.CombinedOutput()
			if (err != nil) != tc.failed {
				t.Fatalf("err=%v out=%s", err, out)
			}
			if strings.Contains(string(out), "50.0%") != tc.bar {
				t.Fatalf("wrong progress mode: %q", out)
			}
			for _, secret := range []string{"fixture-secret", "private.invalid"} {
				if strings.Contains(string(out), secret) {
					t.Fatalf("leaked private curl error: %q", out)
				}
			}
			if tc.name == "invalid-setting" {
				if _, err := os.Stat(filepath.Join(root, "events")); !os.IsNotExist(err) {
					t.Fatal("started transfer with invalid progress setting")
				}
			} else if !strings.Contains(string(out), "[下载] https://source.invalid/?token=private-fixture（开始）") {
				t.Fatalf("progress mode hid the download URL: %q", out)
			}
			if tc.failed {
				data, _ := os.ReadFile(output)
				if string(data) != "active" {
					t.Fatal("failed progress download changed target")
				}
			}
		})
	}
}

func TestNativeCurlDownloadProgress(t *testing.T) {
	content := bytes.Repeat([]byte("progress-fixture\n"), 8192)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/known" {
			w.Header().Set("Content-Length", strconv.Itoa(len(content)))
			_, _ = w.Write(content)
		} else {
			w.(http.Flusher).Flush() // Preserve unknown size instead of Go inferring Content-Length.
			// curl samples unknown-length progress periodically; an instantaneous
			// stream may finish before the first sample and produce no activity bar.
			for offset := 0; offset < len(content); offset += 16384 {
				end := min(offset+16384, len(content))
				_, _ = w.Write(content[offset:end])
				w.(http.Flusher).Flush()
				time.Sleep(30 * time.Millisecond)
			}
		}
	}))
	defer server.Close()
	for _, shell := range []string{"bash", "/bin/bash"} {
		for _, route := range []string{"known", "stream"} {
			t.Run(shell+"/"+route, func(t *testing.T) {
				root := t.TempDir()
				output := filepath.Join(root, "candidate")
				cmd := exec.Command(shell, "-euc", `source "$1"; http_download "$2" 10 1048576 "$3"`, "download", filepath.Join(repositoryRoot(t), "deploy/lib/http.sh"), output, server.URL+"/"+route)
				cmd.Dir = root
				cmd.Env = append(os.Environ(), "MIHOMO_SOURCE_DIR="+root, "DOWNLOAD_PROGRESS=bar", "DOWNLOAD_DOH_SERVERS=", "HTTP_DOWNLOAD_HEADERS_FILE=")
				out, err := cmd.CombinedOutput()
				if err != nil || (!strings.Contains(string(out), "#") && !strings.Contains(string(out), "=O")) || (route == "known" && !strings.Contains(string(out), "100.0%")) {
					t.Fatalf("native progress missing: err=%v out=%q", err, out)
				}
				data, _ := os.ReadFile(output)
				if !bytes.Equal(data, content) {
					t.Fatal("native transfer corrupted output")
				}
				cache, _ := os.ReadFile(filepath.Join(root, ".update-state/download-dns"))
				if string(cache) != "system\n" {
					t.Fatal("native transfer did not persist DNS preference")
				}
			})
		}
	}
}
