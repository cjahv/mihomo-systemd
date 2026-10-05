package manager

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Opt-in acceptance uses the real yq and Mihomo, with a local TLS Google stand-in.
// It changes no systemd services or firewall rules and sends no public requests.
func TestNativeMihomoAcceptance(t *testing.T) {
	kernel, yq := os.Getenv("MIHOMO_TEST_BINARY"), os.Getenv("YQ_TEST_BINARY")
	if kernel == "" || yq == "" {
		t.Skip("set MIHOMO_TEST_BINARY and YQ_TEST_BINARY for real kernel acceptance")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"mihomo": kernel, "yq": yq} {
		if err := os.Symlink(path, filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	runtime := &linuxRuntime{dir: dir, output: io.Discard}
	script, err := os.ReadFile(filepath.Join("..", "..", "deploy", "scripts", "update.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(filepath.Join(dir, "scripts"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "scripts", "update.sh"), script, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(filepath.Join(dir, "lib"), 0700); err != nil {
		t.Fatal(err)
	}
	envScript, err := os.ReadFile(filepath.Join("..", "..", "deploy", "lib", "env.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "lib", "env.sh"), envScript, 0600); err != nil {
		t.Fatal(err)
	}
	httpScript, err := os.ReadFile(filepath.Join("..", "..", "deploy", "lib", "http.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "lib", "http.sh"), httpScript, 0600); err != nil {
		t.Fatal(err)
	}

	if err = os.WriteFile(filepath.Join(dir, ".env"), []byte("MIHOMO_SECRET=example-new-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(dir, "stage")
	if err = os.Mkdir(stage, 0700); err != nil {
		t.Fatal(err)
	}
	raw := []byte("mode: rule\nproxies: []\nproxy-groups: []\nrules:\n  - MATCH,DIRECT\nsecret: example-old-secret\n")
	rendered, err := runtime.Render(context.Background(), stage, raw)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(stage, "config.yaml")
	if err = runtime.Validate(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	var parsed map[string]interface{}
	jsonBytes, err := exec.Command(yq, "-o=json", ".", path).Output()
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(jsonBytes, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["secret"] != "example-new-secret" || parsed["mixed-port"] != float64(7890) {
		t.Fatal("real yq did not keep current administrator overrides")
	}
	if strings.Contains(string(rendered), "example-old-secret") {
		t.Fatal("old secret retained")
	}
	// Undefined rule targets are rejected by the actual kernel parser.
	bad := []byte("mode: rule\nproxies: []\nproxy-groups: []\nrules:\n  - MATCH,undefined-proxy\n")
	if err = os.WriteFile(path, bad, 0600); err != nil {
		t.Fatal(err)
	}
	if err = runtime.Validate(context.Background(), path); err == nil {
		t.Fatal("real kernel accepted undefined rule target")
	}

	cert, root := testGoogleCertificate(t)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/generate_204" {
			w.WriteHeader(404)
			return
		}
		w.WriteHeader(204)
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	defer server.Close()
	targetURL, _ := url.Parse(server.URL)
	targetPort := targetURL.Port()
	for _, rule := range []string{"DIRECT", "REJECT"} {
		t.Run(rule, func(t *testing.T) {
			port := freePort(t)
			work := t.TempDir()
			config := fmt.Sprintf("mixed-port: %d\nbind-address: 127.0.0.1\nmode: rule\nlog-level: silent\nhosts:\n  www.google.com: 127.0.0.1\nproxies: []\nproxy-groups: []\nrules:\n  - MATCH,%s\n", port, rule)
			configPath := filepath.Join(work, "config.yaml")
			if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(kernel, "-d", work, "-f", configPath)
			logs, err := os.Create(filepath.Join(work, "kernel.log"))
			if err != nil {
				t.Fatal(err)
			}
			defer logs.Close()
			cmd.Stdout = logs
			cmd.Stderr = logs
			if err = cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
			for {
				c, e := (&net.Dialer{Timeout: 100 * time.Millisecond}).DialContext(ctx, "tcp", address)
				if e == nil {
					c.Close()
					break
				}
				if e = waitContext(ctx, 30*time.Millisecond); e != nil {
					t.Fatal("real kernel listener did not become ready")
				}
			}
			proxy, _ := url.Parse("http://" + address)
			transport := &http.Transport{Proxy: http.ProxyURL(proxy), DisableKeepAlives: true, TLSClientConfig: &tls.Config{RootCAs: root, MinVersion: tls.VersionTLS12}}
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			probeCtx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
			defer cancel()
			err = probeGoogle(probeCtx, client, "https://www.google.com:"+targetPort+"/generate_204")
			if rule == "DIRECT" && err != nil {
				t.Fatal(err)
			}
			if rule == "REJECT" && err == nil {
				t.Fatal("real rule rejection passed Google probe")
			}
		})
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
func testGoogleCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	pub, key, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "local acceptance"}, DNSNames: []string{"www.google.com"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true}
	der, e := x509.CreateCertificate(rand.Reader, template, template, pub, key)
	if e != nil {
		t.Fatal(e)
	}
	keyDER, e := x509.MarshalPKCS8PrivateKey(key)
	if e != nil {
		t.Fatal(e)
	}
	cert, e := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	if e != nil {
		t.Fatal(e)
	}
	root := x509.NewCertPool()
	root.AppendCertsFromPEM(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	return cert, root
}
