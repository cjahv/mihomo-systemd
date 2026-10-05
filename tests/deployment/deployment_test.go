package deployment

import (
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

// SSH runs only against a temporary local filesystem; all service/installer commands are mocks.
func TestPublishDeployment(t *testing.T) {
	for _, scenario := range []string{"build-only", "existing-env", "new-env", "probe-failure", "unsupported", "build-failure", "env-check-failure", "upload-failure", "checksum-failure", "install-failure"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			if err := os.Mkdir(bin, 0755); err != nil {
				t.Fatal(err)
			}
			remote := filepath.Join(dir, "remote dir's")
			if err := os.Mkdir(remote, 0755); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"scripts/publish.sh", "deploy/lib/target.sh", "deploy/lib/env.sh", "deploy/lib/http.sh", "deploy/systemd/mihomo.service.in", "deploy/systemd/mihomo-manager.service.in"} {
				copyDeploymentFile(t, dir, name)
			}
			writeDeploymentFixture(t, filepath.Join(dir, ".env"), "REMOTE_USER=root\nREMOTE_HOST=example.invalid\nREMOTE_DIR="+remote+"\nMIHOMO_SECRET=local-fixture\n")
			if scenario != "new-env" {
				writeDeploymentFixture(t, filepath.Join(remote, ".env"), "remote-fixture\n")
			}
			writeDeploymentFixture(t, filepath.Join(dir, "deploy", "install.sh"), "#!/bin/bash\necho install >> \"$TEST_LOG\"\n[ \"$TEST_CASE\" != install-failure ]\n")
			writeDeploymentFixture(t, filepath.Join(dir, "deploy", "scripts", "update.sh"), "#!/bin/bash\necho update >> \"$TEST_LOG\"\n")
			writeDeploymentFixture(t, filepath.Join(dir, "deploy", "scripts", "entrypoint.sh"), "runtime fixture")
			writeDeploymentFixture(t, filepath.Join(bin, "ssh"), `#!/bin/bash
set -eu
remote_command="${@: -1}"
printf 'ssh %s\n' "$remote_command" >> "$TEST_LOG"
if [ "$remote_command" = 'sh -s' ]; then
    cat >/dev/null
    [ "$TEST_CASE" != probe-failure ] || exit 255
    if [ "$TEST_CASE" = unsupported ]; then echo 'Linux|mips|6.1.0'; else echo 'Linux|x86_64|6.1.0'; fi
    exit 0
fi
if [[ "$remote_command" == test* && "$TEST_CASE" = env-check-failure ]]; then exit 255; fi
if [[ "$remote_command" == *'tar -xf'* && "$TEST_CASE" = upload-failure ]]; then cat >/dev/null; exit 1; fi
if [[ "$remote_command" == *'tar -xf'* && "$TEST_CASE" = checksum-failure ]]; then
    bash -c "$remote_command"
    for stage in "$TEST_REMOTE"/.deploy.*; do printf corrupted > "$stage/mihomo-manager"; done
    exit 0
fi
exec bash -c "$remote_command"
`)
			writeDeploymentFixture(t, filepath.Join(bin, "mise"), "#!/bin/bash\n[ \"$1\" = exec ] && [ \"$2\" = -- ] || exit 1\nshift 2\nexec \"$@\"\n")
			writeDeploymentFixture(t, filepath.Join(bin, "go"), `#!/bin/bash
set -eu
printf 'build %s %s %s %s %s\n' "$GOOS" "$GOARCH" "$CGO_ENABLED" "$GOTOOLCHAIN" "$GOAMD64" >> "$TEST_LOG"
[ "$TEST_CASE" != build-failure ] || exit 1
while [ "$1" != -o ]; do shift; done
cat > "$2" <<'BINARY'
#!/bin/bash
echo 'mihomo-manager go1.26.8 linux/amd64'
BINARY
chmod +x "$2"
`)
			for _, name := range []string{"systemctl", "journalctl"} {
				writeDeploymentFixture(t, filepath.Join(bin, name), "#!/bin/bash\necho service >> \"$TEST_LOG\"\n")
			}
			logPath := filepath.Join(dir, "events")
			args := []string{"./scripts/publish.sh"}
			if scenario == "build-only" {
				args = append(args, "--build-only")
			}
			cmd := exec.Command("bash", args...)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "TEST_LOG="+logPath, "TEST_CASE="+scenario, "TEST_REMOTE="+remote)
			out, err := cmd.CombinedOutput()
			failed := strings.HasSuffix(scenario, "failure") || scenario == "unsupported"
			if (err != nil) != failed {
				t.Fatalf("err=%v output=%s", err, out)
			}
			logData, _ := os.ReadFile(logPath)
			log := string(logData)
			if scenario == "probe-failure" || scenario == "unsupported" {
				if strings.Contains(log, "build") {
					t.Fatal("built after failed probe")
				}
			} else if !strings.Contains(log, "build linux amd64 0 local v1") {
				t.Fatalf("missing pinned cross-build: %s", log)
			}
			if scenario == "build-only" || scenario == "probe-failure" || scenario == "unsupported" || scenario == "build-failure" || scenario == "env-check-failure" {
				if strings.Contains(log, "mkdir") || strings.Contains(log, "install\n") || strings.Contains(log, "service\n") {
					t.Fatalf("unexpected remote mutation: %s", log)
				}
			}
			if (scenario == "upload-failure" || scenario == "checksum-failure") && strings.Contains(log, "install\n") {
				t.Fatal("installed after upload failure")
			}
			if scenario == "install-failure" && strings.Contains(log, "service\n") {
				t.Fatal("restarted after install failure")
			}
			if !failed && scenario != "build-only" {
				if !strings.Contains(log, "install\nupdate\nservice\n") {
					t.Fatalf("deployment incomplete: %s", log)
				}
				for _, name := range []string{"cmd", "internal", "tests", "go.mod", "mise.toml", "ui.html"} {
					if _, err := os.Stat(filepath.Join(remote, name)); !os.IsNotExist(err) {
						t.Fatalf("uploaded development file %s", name)
					}
				}
			}
			env, _ := os.ReadFile(filepath.Join(remote, ".env"))
			if scenario != "new-env" && string(env) != "remote-fixture\n" {
				t.Fatal("overwrote remote environment")
			}
			if scenario == "new-env" && !strings.Contains(string(env), "local-fixture") {
				t.Fatal("missing initial environment")
			}
			stages, _ := filepath.Glob(filepath.Join(remote, ".deploy.*"))
			if len(stages) != 0 {
				t.Fatalf("remote staging not cleaned: %v", stages)
			}
		})
	}
}

func TestInstallRejectsMissingOrIncompatibleBinary(t *testing.T) {
	for _, scenario := range []string{"no-argument", "relative-path", "missing", "cannot-run", "wrong-arch"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			for _, name := range []string{"deploy/install.sh", "deploy/lib/target.sh", "deploy/lib/env.sh", "deploy/lib/http.sh"} {
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
			args := []string{"./deploy/install.sh", filepath.Join(dir, "mihomo-manager")}
			if scenario == "no-argument" {
				args = args[:1]
			}
			if scenario == "relative-path" {
				args[1] = "./mihomo-manager"
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
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "TEST_LOG="+log, "MIHOMO_UPDATE_INTERNAL=1")
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
	for _, scenario := range []string{"current", "dns-failure", "offline", "invalid-release", "artifact-failure", "unusable-core", "unusable-core-offline"} {
		t.Run(scenario, func(t *testing.T) { testRuntimeInstaller(t, scenario) })
	}
}

func testRuntimeInstaller(t *testing.T, scenario string) {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"deploy/install.sh", "deploy/lib/target.sh", "deploy/lib/env.sh", "deploy/lib/http.sh", "deploy/scripts/update.sh", "deploy/scripts/entrypoint.sh", "deploy/systemd/mihomo.service.in", "deploy/systemd/mihomo-manager.service.in"} {
		copyDeploymentFile(t, dir, name)
	}
	deployDir := filepath.Join(dir, "deploy")
	if err := os.Mkdir(filepath.Join(deployDir, "ui"), 0755); err != nil {
		t.Fatal(err)
	}
	writeDeploymentFixture(t, filepath.Join(deployDir, ".env"), "GITHUB_PROXY=\nGITHUB_API_PROXY=\n")
	artifact := filepath.Join(dir, "prebuilt-manager")
	writeDeploymentFixture(t, artifact, "#!/bin/bash\necho 'mihomo-manager go1.26.8 linux/amd64'\n")
	bin := filepath.Join(dir, "tools")
	runtimeBin := filepath.Join(dir, "installed-bin")
	units := filepath.Join(dir, "installed-systemd")
	events := filepath.Join(dir, "events")
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
[[ "$TEST_CASE" != unusable-core* ]] || exit 1
echo 'Mihomo v1.0.0 linux/amd64'
`)
	writeDeploymentFixture(t, filepath.Join(bin, "curl"), `#!/bin/bash
set -eu
doh=false
url="${@: -1}"
while [ "$1" != --output ]; do
    [ "$1" != --doh-url ] || doh=true
    shift
done
case "$TEST_CASE" in
    offline|unusable-core-offline) exit 6 ;;
    dns-failure) [ "$doh" = true ] || exit 6 ;;
    invalid-release|unusable-core) printf '{}' > "$2"; exit 0 ;;
    artifact-failure)
        [[ "$url" != *releases/download* ]] || exit 28
        printf '{"tag_name":"v1.0.1","assets":[{"name":"mihomo-linux-amd64-v1.0.1.deb"},{"name":"mihomo-linux-amd64-v1.0.1.rpm"},{"name":"mihomo-linux-amd64-v1.0.1.gz"}]}' > "$2"
        exit 0 ;;
esac
printf '{"tag_name":"v1.0.0"}' > "$2"
`)
	writeDeploymentFixture(t, filepath.Join(bin, "systemctl"), "#!/bin/bash\nprintf '%s\\n' \"$*\" >> \"$TEST_LOG\"\n")
	for _, name := range []string{"git", "go", "nft", "dpkg", "rpm"} {
		writeDeploymentFixture(t, filepath.Join(bin, name), "#!/bin/bash\necho unexpected >> \"$TEST_LOG\"\nexit 1\n")
	}
	cmd := exec.Command("bash", filepath.Join(deployDir, "install.sh"), artifact)
	cmd.Dir = dir // Installer must resolve its own package root.
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "TEST_RUNTIME_BIN="+runtimeBin, "TEST_SYSTEMD_DIR="+units, "TEST_LOG="+events, "TEST_CASE="+scenario)
	out, err := cmd.CombinedOutput()
	if strings.HasPrefix(scenario, "unusable-core") {
		if err == nil {
			t.Fatalf("unusable core accepted: %s", out)
		}
		if _, err := os.Stat(runtimeBin); !os.IsNotExist(err) {
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
	if (scenario == "offline" || scenario == "artifact-failure" || scenario == "invalid-release") && !strings.Contains(string(out), "保留") {
		t.Fatalf("missing truthful core fallback: %s", out)
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
		if !strings.Contains(string(data), `WorkingDirectory="`+deployDir+`"`) || strings.Contains(string(data), "@DEPLOY_DIR@") {
			t.Fatalf("incorrect service working directory: %s", data)
		}
		if unit == "mihomo" && !strings.Contains(string(data), `ExecStart="`+deployDir+`/scripts/entrypoint.sh"`) {
			t.Fatalf("incorrect service entrypoint: %s", data)
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
