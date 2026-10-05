package deployment

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate repository fixture")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
}

func TestSystemTarget(t *testing.T) {
	for _, tc := range []struct{ os, machine, kernel, want string }{
		{"Linux", "x86_64", "6.1.0", "linux/amd64/5,softfloat"},
		{"Linux", "aarch64", "5.15.0", "linux/arm64/5,softfloat"},
		{"Linux", "armv6l", "4.19.0", "linux/arm/6,softfloat"},
		{"Linux", "armv7l", "3.2.0", "linux/arm/7,softfloat"},
		{"Linux", "i686", "6.0.0", "linux/386/5,softfloat"},
		{"Darwin", "arm64", "25.0.0", ""},
		{"Linux", "mips", "6.1.0", ""},
		{"Linux", "x86_64", "2.6.32", ""},
		{"Linux", "x86_64", "3.1.9", ""},
		{"Linux", "x86_64", "unknown", ""},
	} {
		t.Run(tc.os+"/"+tc.machine+"/"+tc.kernel, func(t *testing.T) {
			cmd := exec.Command("bash", "-c", `source "$4"; configure_target "$1" "$2" "$3" || exit 1; printf '%s/%s/%s' "$TARGET_GOOS" "$TARGET_GOARCH" "$TARGET_GOARM"`, "target", tc.os, tc.machine, tc.kernel, filepath.Join(repositoryRoot(t), "deploy", "lib", "target.sh"))
			out, err := cmd.CombinedOutput()
			if tc.want == "" {
				if err == nil {
					t.Fatalf("unsupported target accepted: %s", out)
				}
			} else if err != nil || string(out) != tc.want {
				t.Fatalf("target=%s error=%v want=%s", out, err, tc.want)
			}
		})
	}
}

func writeDeploymentFixture(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0755); err != nil {
		t.Fatal(err)
	}
}

func copyDeploymentFile(t *testing.T, dir, name string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repositoryRoot(t), name))
	if err != nil {
		t.Fatal(err)
	}
	writeDeploymentFixture(t, filepath.Join(dir, name), string(data))
}

func TestInstallRejectsMissingOrIncompatibleBinary(t *testing.T) {
	for _, scenario := range []string{"no-argument", "relative-path", "missing", "cannot-run", "wrong-arch"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			for _, name := range []string{"deploy/install.sh", "deploy/lib/target.sh", "deploy/lib/env.sh", "deploy/lib/http.sh", "deploy/lib/release.sh"} {
				copyDeploymentFile(t, dir, name)
			}
			writeDeploymentFixture(t, filepath.Join(dir, "uname"), "#!/bin/bash\ncase \"$1\" in -s) echo Linux;; -m) echo x86_64;; -r) echo 6.1.0;; esac\n")
			if scenario == "cannot-run" {
				writeDeploymentFixture(t, filepath.Join(dir, "mihomo-manager"), "#!/bin/bash\nexit 1\n")
			}
			if scenario == "wrong-arch" {
				writeDeploymentFixture(t, filepath.Join(dir, "mihomo-manager"), "#!/bin/bash\necho 'mihomo-manager go1.26.8 linux/arm64'\n")
			}
			// Any system mutation/download would leave an event, even on hosts with sudo installed.
			log := filepath.Join(dir, "mutations")
			for _, name := range []string{"sudo", "curl", "git", "go"} {
				writeDeploymentFixture(t, filepath.Join(dir, name), "#!/bin/bash\necho unexpected >> \"$TEST_LOG\"\nexit 1\n")
			}
			args := []string{"./deploy/install.sh", "--prepare", filepath.Join(dir, "mihomo-manager")}
			if scenario == "no-argument" {
				args = args[:2]
			}
			if scenario == "relative-path" {
				args[2] = "./mihomo-manager"
			}
			cmd := exec.Command("bash", args...)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "TEST_LOG="+log)
			if out, err := cmd.CombinedOutput(); err == nil {
				t.Fatalf("invalid binary accepted: %s", out)
			}
			if _, err := os.Stat(log); !os.IsNotExist(err) {
				t.Fatal("mutated system before binary validation")
			}
		})
	}
}

func TestUpdateDoesNotInstallRuntime(t *testing.T) {
	for _, scenario := range []string{"missing-manager", "missing-mihomo"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			script, err := os.ReadFile(filepath.Join(repositoryRoot(t), "deploy", "scripts", "update.sh"))
			if err != nil {
				t.Fatal(err)
			}
			// Redirect the fixed production binary location into the isolated fixture.
			writeDeploymentFixture(t, filepath.Join(dir, "deploy", "scripts", "update.sh"), strings.ReplaceAll(string(script), "/usr/local/bin/mihomo-manager", filepath.Join(dir, "manager")))
			copyDeploymentFile(t, dir, "deploy/lib/env.sh")
			copyDeploymentFile(t, dir, "deploy/lib/http.sh")
			writeDeploymentFixture(t, filepath.Join(dir, "deploy", ".env"), "MIHOMO_SECRET=fixture\nCONFIG_URL=https://example.invalid/config\n")
			log := filepath.Join(dir, "mutations")
			for _, name := range []string{"deploy/install.sh", "curl", "sudo"} {
				writeDeploymentFixture(t, filepath.Join(dir, name), "#!/bin/bash\necho unexpected >> \"$TEST_LOG\"\nexit 1\n")
			}
			writeDeploymentFixture(t, filepath.Join(dir, "mihomo"), "#!/bin/bash\nexit 1\n")
			args := []string{"./deploy/scripts/update.sh"}
			if scenario == "missing-mihomo" {
				args = append(args, "--prepare", dir)
			}
			cmd := exec.Command("bash", args...)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "TEST_LOG="+log, "MIHOMO_UPDATE_INTERNAL=1", "MIHOMO_TEST_KERNEL="+filepath.Join(dir, "mihomo"))
			out, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(out), "mise run publish") {
				t.Fatalf("expected redeployment error: %v %s", err, out)
			}
			if _, err := os.Stat(log); !os.IsNotExist(err) {
				t.Fatal("configuration update attempted runtime installation")
			}
		})
	}
}

// Execute the production installer while mapping privileged paths to a fixture.
func TestRuntimeInstallerOwnsBinaryAndSystemdUnits(t *testing.T) {
	for _, scenario := range []string{"current", "dns-failure", "offline", "invalid-release", "artifact-failure", "unusable-core", "unusable-core-offline", "verified-release", "corrupt-release", "special-path", "same-release", "same-release-no-assets", "force-same-release", "path-shadow", "path-newer-shadow", "unknown-banner", "prerelease-banner", "apply-changed", "repaired-core", "first-install"} {
		t.Run(scenario, func(t *testing.T) { testRuntimeInstaller(t, scenario) })
	}
}

func testRuntimeInstaller(t *testing.T, scenario string) {
	t.Helper()
	dir := t.TempDir()
	if scenario == "special-path" {
		dir = filepath.Join(dir, `runtime space "quote" 'single' %n $PATH \backslash`)
	}
	for _, name := range []string{"deploy/install.sh", "deploy/lib/target.sh", "deploy/lib/env.sh", "deploy/lib/http.sh", "deploy/lib/release.sh", "deploy/scripts/update.sh", "deploy/scripts/entrypoint.sh", "deploy/systemd/mihomo.service.in", "deploy/systemd/mihomo-manager.service.in"} {
		copyDeploymentFile(t, dir, name)
	}
	deployDir := filepath.Join(dir, "deploy")
	writeDeploymentFixture(t, filepath.Join(deployDir, "ui", "index.html"), "panel")
	writeDeploymentFixture(t, filepath.Join(deployDir, ".env"), "GITHUB_PROXY=\nGITHUB_API_PROXY=\n")
	artifact := filepath.Join(dir, "prebuilt-manager")
	writeDeploymentFixture(t, artifact, "#!/bin/bash\necho 'mihomo-manager go1.26.8 linux/amd64'\n")
	downloadVersion := "v1.0.1"
	if scenario == "force-same-release" {
		downloadVersion = "v1.0.0"
	}
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	_, _ = writer.Write([]byte("#!/bin/bash\necho 'Mihomo Meta " + downloadVersion + " linux amd64 with go1.26.8\nUse tags: with_gvisor'\n"))
	_ = writer.Close()
	coreArchive := filepath.Join(dir, "core.gz")
	if err := os.WriteFile(coreArchive, compressed.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(compressed.Bytes())
	checksum := hex.EncodeToString(hash[:])
	if scenario == "corrupt-release" {
		checksum = strings.Repeat("a", 64)
	}
	bin := filepath.Join(dir, "tools")
	runtimeBin := filepath.Join(dir, "installed-bin")
	units := filepath.Join(dir, "installed-systemd")
	events := filepath.Join(dir, "events")
	curlEvents := filepath.Join(dir, "curl-events")
	// Rewrite only fixture-owned paths; the real installer still runs in both phases.
	installerPath := filepath.Join(deployDir, "install.sh")
	installer, err := os.ReadFile(installerPath)
	if err != nil {
		t.Fatal(err)
	}
	writeDeploymentFixture(t, installerPath, strings.ReplaceAll(string(installer), "/usr/local/bin", "${TEST_RUNTIME_BIN}"))
	installedCore := filepath.Join(runtimeBin, "mihomo")
	currentBanner := "Mihomo Meta v1.0.0 linux amd64 with go1.26.8\nUse tags: with_gvisor"
	if scenario == "path-shadow" {
		currentBanner = strings.ReplaceAll(currentBanner, "v1.0.0", "v1.0.1")
	} else if scenario == "unknown-banner" {
		currentBanner = "custom build v1.0.1"
	} else if scenario == "prerelease-banner" {
		currentBanner = strings.ReplaceAll(currentBanner, "v1.0.0", "v1.0.1-alpha")
	}
	currentBody := "#!/bin/bash\nprintf '%s\\n' '" + currentBanner + "'\n"
	if strings.HasPrefix(scenario, "unusable-core") || scenario == "repaired-core" {
		currentBody = "#!/bin/bash\nexit 1\n"
	}
	if scenario != "first-install" {
		writeDeploymentFixture(t, installedCore, currentBody)
	}
	writeDeploymentFixture(t, filepath.Join(bin, "sudo"), `#!/bin/bash
set -eu
args=()
for arg in "$@"; do
    arg="${arg//\/usr\/local\/bin/$TEST_RUNTIME_BIN}"
    arg="${arg//\/etc\/systemd\/system/$TEST_SYSTEMD_DIR}"
    args+=("$arg")
done
exec "${args[@]}"
`)
	writeDeploymentFixture(t, filepath.Join(bin, "uname"), "#!/bin/bash\ncase \"$1\" in -s) echo Linux;; -m) echo x86_64;; -r) echo 6.1.0;; esac\n")
	writeDeploymentFixture(t, filepath.Join(bin, "mihomo"), `#!/bin/bash
[[ "$TEST_CASE" != unusable-core* && "$TEST_CASE" != first-install ]] || exit 1
if [ "$TEST_CASE" = path-newer-shadow ]; then echo 'Mihomo Meta v1.0.1 linux amd64'; else echo 'Mihomo v1.0.0 linux/amd64'; fi
`)
	writeDeploymentFixture(t, filepath.Join(bin, "curl"), `#!/bin/bash
set -eu
doh=false
url="${@: -1}"
printf '%s\n' "$url" >> "$TEST_CURL_LOG"
while [ "$1" != --output ]; do
    [ "$1" != --doh-url ] || doh=true
    shift
done
case "$TEST_CASE" in
    verified-release|corrupt-release|same-release|force-same-release|path-shadow|path-newer-shadow|unknown-banner|prerelease-banner|apply-changed|repaired-core|first-install)
        if [[ "$url" = *releases/download* ]]; then cp "$TEST_CORE_ARCHIVE" "$2"; else
        printf '{"tag_name":"%s","assets":[{"name":"mihomo-linux-amd64-v1-%s.gz","digest":"sha256:%s"}]}' "$TEST_RELEASE_TAG" "$TEST_RELEASE_TAG" "$TEST_CORE_SHA" > "$2"; fi
        exit 0 ;;
    offline|unusable-core-offline) exit 6 ;;
    dns-failure) [ "$doh" = true ] || exit 6 ;;
    invalid-release|unusable-core) printf '{}' > "$2"; exit 0 ;;
    artifact-failure)
        [[ "$url" != *releases/download* ]] || exit 28
        printf '{"tag_name":"v1.0.1","assets":[{"name":"mihomo-linux-amd64-v1-v1.0.1.gz","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}' > "$2"
        exit 0 ;;
esac
printf '{"tag_name":"v1.0.0"}' > "$2"
`)
	writeDeploymentFixture(t, filepath.Join(bin, "systemctl"), "#!/bin/bash\nprintf '%s\\n' \"$*\" >> \"$TEST_LOG\"\n")
	for _, name := range []string{"git", "go", "nft", "dpkg", "rpm"} {
		writeDeploymentFixture(t, filepath.Join(bin, name), "#!/bin/bash\necho unexpected >> \"$TEST_LOG\"\nexit 1\n")
	}
	cmd := exec.Command("bash", filepath.Join(deployDir, "install.sh"), "--prepare", artifact)
	cmd.Dir = dir // Installer must resolve its own package root.
	releaseTag := "v1.0.1"
	if scenario == "same-release" || scenario == "force-same-release" || scenario == "apply-changed" {
		releaseTag = "v1.0.0"
	}
	forceCore := "false"
	if scenario == "force-same-release" {
		forceCore = "true"
	}
	cmd.Env = append(os.Environ(), "MIHOMO_FORCE_CORE_INSTALL="+forceCore, "TEST_RELEASE_TAG="+releaseTag, "TEST_CURL_LOG="+curlEvents, "PATH="+bin+":"+os.Getenv("PATH"), "TEST_RUNTIME_BIN="+runtimeBin, "TEST_SYSTEMD_DIR="+units, "TEST_LOG="+events, "TEST_CASE="+scenario, "TEST_CORE_ARCHIVE="+coreArchive, "TEST_CORE_SHA="+checksum)
	out, err := cmd.CombinedOutput()
	if scenario != "first-install" {
		unchanged, readErr := os.ReadFile(installedCore)
		if readErr != nil || string(unchanged) != currentBody {
			t.Fatalf("preparation changed the installed core: %v", readErr)
		}
	}
	if strings.HasPrefix(scenario, "unusable-core") || scenario == "corrupt-release" {
		if err == nil {
			t.Fatalf("unusable core accepted: %s", out)
		}
		if _, err := os.Stat(filepath.Join(runtimeBin, "mihomo-manager")); !os.IsNotExist(err) {
			t.Fatal("installed manager after failed core acquisition")
		}
		if _, err := os.Stat(events); !os.IsNotExist(err) {
			t.Fatal("modified services after failed core acquisition")
		}
		return
	}
	if err != nil {
		t.Fatalf("installer failed: %v\n%s", err, out)
	}
	if scenario == "verified-release" && (!strings.Contains(string(out), "SHA256 校验成功") || !strings.Contains(string(out), "amd64-v1-v1.0.1.gz")) {
		t.Fatalf("did not install verified baseline: %s", out)
	}
	if (scenario == "offline" || scenario == "artifact-failure" || scenario == "invalid-release") && !strings.Contains(string(out), "保留") {
		t.Fatalf("missing truthful core fallback: %s", out)
	}
	if _, err := os.Stat(filepath.Join(runtimeBin, "mihomo-manager")); !os.IsNotExist(err) {
		t.Fatal("preparation installed system binaries")
	}
	downloads, err := os.ReadFile(curlEvents)
	if err != nil {
		t.Fatal(err)
	}
	wantDownload := scenario == "verified-release" || scenario == "force-same-release" || scenario == "path-newer-shadow" || scenario == "unknown-banner" || scenario == "prerelease-banner" || scenario == "artifact-failure" || scenario == "repaired-core" || scenario == "first-install"
	if strings.Contains(string(downloads), "releases/download") != wantDownload {
		t.Fatalf("unexpected core download decision: %s\n%s", downloads, out)
	}
	if scenario == "same-release" || scenario == "same-release-no-assets" || scenario == "path-shadow" || scenario == "apply-changed" {
		if !strings.Contains(string(out), "跳过下载") {
			t.Fatalf("missing version reuse message: %s", out)
		}
	}
	if scenario == "apply-changed" {
		writeDeploymentFixture(t, installedCore, "#!/bin/bash\necho changed-after-prepare\n")
	}
	beforeApply, err := os.Stat(installedCore)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	preparedCore, err := os.ReadFile(filepath.Join(deployDir, ".release", "mihomo"))
	if err != nil {
		t.Fatal(err)
	}
	beforeContent, _ := os.ReadFile(installedCore)
	wantSkipInstall := forceCore == "false" && bytes.Equal(beforeContent, preparedCore)
	apply := exec.Command("bash", filepath.Join(deployDir, "install.sh"), "--apply", artifact)
	apply.Dir, apply.Env = cmd.Dir, cmd.Env
	applyOut, err := apply.CombinedOutput()
	if err != nil {
		t.Fatalf("apply failed: %v %s", err, applyOut)
	}
	afterApply, err := os.Stat(installedCore)
	if err != nil {
		t.Fatal(err)
	}
	if (beforeApply != nil && os.SameFile(beforeApply, afterApply)) != wantSkipInstall || strings.Contains(string(applyOut), "跳过重复安装") != wantSkipInstall {
		t.Fatalf("incorrect replacement decision (skip=%v): %s", wantSkipInstall, applyOut)
	}
	afterContent, _ := os.ReadFile(installedCore)
	if !bytes.Equal(afterContent, preparedCore) {
		t.Fatal("installed core differs from the preflight candidate")
	}
	wantBinary, _ := os.ReadFile(artifact)
	installed, err := os.ReadFile(filepath.Join(runtimeBin, "mihomo-manager"))
	if err != nil || string(installed) != string(wantBinary) {
		t.Fatalf("prebuilt artifact not installed: %v", err)
	}
	for _, unit := range []string{"mihomo", "mihomo-manager"} {
		data, err := os.ReadFile(filepath.Join(units, unit+".service"))
		if err != nil {
			t.Fatal(err)
		}
		workingDir := strings.ReplaceAll(deployDir, "%", "%%")
		if !strings.Contains(string(data), "\nWorkingDirectory="+workingDir+"\n") || strings.Contains(string(data), "@DEPLOY_DIR@") {
			t.Fatalf("incorrect service working directory: %s", data)
		}
		execDir := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%", "$", "$$").Replace(deployDir)
		if unit == "mihomo" && !strings.Contains(string(data), `ExecStart=/bin/bash "`+execDir+`/scripts/entrypoint.sh"`) {
			t.Fatalf("incorrect service entrypoint: %s", data)
		}
		// On Linux, also use systemd's real parser. The fixture owns executables
		// under its temporary directory; verification never starts either service.
		if runtime.GOOS == "linux" {
			if analyzer, err := exec.LookPath("systemd-analyze"); err == nil {
				verifyDir := filepath.Join(dir, "verify-systemd")
				execArtifact := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%", "$", "$$").Replace(artifact)
				candidate := strings.ReplaceAll(string(data), "ExecStart=/usr/local/bin/mihomo-manager", `ExecStart=/bin/bash "`+execArtifact+`"`)
				path := filepath.Join(verifyDir, unit+".service")
				writeDeploymentFixture(t, path, candidate)
				if out, err := exec.Command(analyzer, "verify", "--man=no", path).CombinedOutput(); err != nil {
					t.Fatalf("systemd rejected rendered unit: %v %s", err, out)
				}
			}
		}
	}
	log, _ := os.ReadFile(events)
	if string(log) != "daemon-reload\nenable mihomo.service mihomo-manager.service\n" {
		t.Fatalf("unexpected installer actions: %s", log)
	}
	pending, _ := filepath.Glob(filepath.Join(runtimeBin, ".mihomo-manager.*"))
	if len(pending) != 0 {
		t.Fatalf("incomplete binary replacement: %v", pending)
	}
}

func TestSharedEnvironmentParserTreatsValuesAsData(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, ".env")
	marker := filepath.Join(dir, "executed")
	literal := "$(touch '" + marker + "')"
	writeDeploymentFixture(t, envFile, "export MIHOMO_SECRET='example'\nCONFIG_URL=\"https://example.invalid/?a=b\"\nGITHUB_PROXY="+literal+"\n")
	script := filepath.Join(repositoryRoot(t), "deploy", "lib", "env.sh")
	cmd := exec.Command("bash", "-c", `source "$1"; load_env_file "$2"; printf '%s\n' "$MIHOMO_SECRET" "$CONFIG_URL" "$GITHUB_PROXY"`, "env", script, envFile)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil || string(out) != "example\nhttps://example.invalid/?a=b\n"+literal+"\n" {
		t.Fatalf("unexpected parsing: %v %s", err, out)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("executed environment data")
	}
}
