package deployment

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Privileged paths, SSH, compilation and services are confined to the fixture.
func TestPublishDeployment(t *testing.T) {
	for _, scenario := range []string{"build-only", "existing-env", "terminal", "new-env", "probe-failure", "unsupported", "build-failure", "env-check-failure", "upload-failure", "checksum-failure", "prepare-failure", "install-failure", "update-failure", "active-update-failure", "manager-failure", "process-mismatch", "interrupted", "recovery-failure", "force", "force-interrupted", "force-prepare-failure", "force-install-failure", "force-update-failure", "force-recovery-failure", "force-pending-restore-failure", "force-invalid-pending-failure", "force-lock-failure"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			remote := filepath.Join(dir, "remote dir's")
			systemBin := filepath.Join(dir, "installed-bin")
			units := filepath.Join(dir, "units")
			for _, p := range []string{bin, remote, systemBin, units} {
				if err := os.Mkdir(p, 0755); err != nil {
					t.Fatal(err)
				}
			}
			for _, name := range []string{"scripts/publish.sh", "deploy/lib/env.sh", "deploy/lib/target.sh", "deploy/lib/http.sh", "deploy/lib/release.sh", "deploy/release.sh", "deploy/systemd/mihomo.service.in", "deploy/systemd/mihomo-manager.service.in"} {
				copyDeploymentFile(t, dir, name)
			}
			releasePath := filepath.Join(dir, "deploy", "release.sh")
			release, _ := os.ReadFile(releasePath)
			body := strings.ReplaceAll(string(release), "/usr/local/bin", systemBin)
			body = strings.ReplaceAll(body, "/etc/systemd/system", units)
			proc := filepath.Join(dir, "proc")
			body = strings.ReplaceAll(body, "/proc/", proc+"/")
			writeDeploymentFixture(t, releasePath, body)
			writeDeploymentFixture(t, filepath.Join(dir, ".env"), "REMOTE_USER=root\nREMOTE_HOST=example.invalid\nREMOTE_DIR="+remote+"\nMIHOMO_SECRET=local-fixture\n")
			if scenario != "new-env" {
				writeDeploymentFixture(t, filepath.Join(remote, ".env"), "remote-fixture\n")
			}
			writeDeploymentFixture(t, filepath.Join(remote, "config.yaml"), "old-config")
			writeDeploymentFixture(t, filepath.Join(remote, "scripts", "entrypoint.sh"), "old-runtime")
			writeDeploymentFixture(t, filepath.Join(dir, "deploy", "scripts", "entrypoint.sh"), "new-runtime")
			writeDeploymentFixture(t, filepath.Join(dir, "deploy", "scripts", "update.sh"), "new-update")
			writeDeploymentFixture(t, filepath.Join(dir, "deploy", "install.sh"), `#!/bin/bash
set -eu
expected_force=false
[[ "$TEST_CASE" != force* ]] || expected_force=true
[ "$MIHOMO_FORCE_CORE_INSTALL" = "$expected_force" ] || { echo 'core force mode lost' >&2; exit 1; }
case "$1" in
 --prepare)
  echo prepare-install >> "$TEST_LOG"
  mkdir .release
  printf '#!/bin/bash\necho core\n' > .release/mihomo
  chmod +x .release/mihomo
  if [ "$TEST_CASE" = terminal ]; then
    echo "remote-terminal=$MIHOMO_DOWNLOAD_TERMINAL" >> "$TEST_LOG"
    source ./lib/http.sh
    DOWNLOAD_DOH_SERVERS= http_download .release/fixture 10 1000 https://fixture.invalid
  fi ;;
 --apply)
  echo install >> "$TEST_LOG"
  cp "$2" "$TEST_INSTALLED/mihomo-manager"
  cp "$MIHOMO_PREPARED_DIR/mihomo" "$TEST_INSTALLED/mihomo"
  for unit in mihomo mihomo-manager; do printf new-unit > "$TEST_UNITS/$unit.service"; done
  [[ "$TEST_CASE" != *install-failure ]] || exit 1 ;;
esac
`)
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
			writeDeploymentFixture(t, filepath.Join(bin, "mise"), "#!/bin/bash\nshift 2\nexec \"$@\"\n")
			writeDeploymentFixture(t, filepath.Join(bin, "go"), `#!/bin/bash
set -eu
printf 'build %s %s %s %s %s\n' "$GOOS" "$GOARCH" "$CGO_ENABLED" "$GOTOOLCHAIN" "$GOAMD64" >> "$TEST_LOG"
[ "$TEST_CASE" != build-failure ] || exit 1
while [ "$1" != -o ]; do shift; done
cat > "$2" <<'BINARY'
#!/bin/bash
set -eu
if [ "$1" = --version ]; then echo 'mihomo-manager go1.26.8 linux/amd64'; exit 0; fi
case "$2" in
 --prepare-only)
  echo preflight >> "$TEST_LOG"
  [[ "$TEST_CASE" != *prepare-failure ]] || exit 1
  echo '{}' > "$3/before.json"
  cp "$TEST_REMOTE/config.yaml" "$3/before.config"
  echo '{}' > "$3/candidate.json" ;;
 --prepared)
  echo update >> "$TEST_LOG"
  echo new-config > "$TEST_REMOTE/config.yaml"
  [[ "$TEST_CASE" != *update-failure && "$TEST_CASE" != force-recovery-failure && "$TEST_CASE" != force-pending-restore-failure ]] || exit 1
  touch "$TEST_REMOTE/core.active" ;;
 --check-snapshot) echo check-source >> "$TEST_LOG" ;;
 --source-dir)
  if [ "$4" = --verify-snapshot ]; then echo verify-restored >> "$TEST_LOG"; exit 0; fi
  echo restore-data >> "$TEST_LOG"
  [ "$TEST_CASE" != force-recovery-failure ] || exit 1
  cp "${5%.json}.config" "$TEST_REMOTE/config.yaml" ;;
 *) exit 1 ;;
esac
BINARY
chmod +x "$2"
`)
			writeDeploymentFixture(t, filepath.Join(bin, "systemctl"), `#!/bin/bash
set -eu
case "$1" in
 is-active) test -f "$TEST_REMOTE/${@: -1}.active" || { [[ "${@: -1}" = mihomo.service ]] && test -f "$TEST_REMOTE/core.active"; } ;;
 is-enabled) exit 1 ;;
 show) if [ "$2" = mihomo.service ]; then echo 123; else echo 124; fi ;;
 restart)
  echo "restart $2" >> "$TEST_LOG"
  if [ "$TEST_CASE" = manager-failure ] && [ "$2" = mihomo-manager.service ]; then exit 1; fi
  if [ -f "$TEST_UNITS/$2" ] && [ "$(cat "$TEST_UNITS/$2")" = broken-unit ]; then exit 1; fi
  touch "$TEST_REMOTE/$2.active" ;;
 stop) echo "stop $2" >> "$TEST_LOG"; rm -f "$TEST_REMOTE/$2.active" "$TEST_REMOTE/core.active" ;;
 *) exit 0 ;;
esac
`)
			writeDeploymentFixture(t, filepath.Join(bin, "stat"), `#!/bin/bash
if [ "$TEST_CASE" = process-mismatch ] && [[ "${@: -1}" = */proc/* ]]; then echo different; else echo matching; fi
`)
			writeDeploymentFixture(t, filepath.Join(bin, "sudo"), "#!/bin/bash\nexec \"$@\"\n")
			writeDeploymentFixture(t, filepath.Join(bin, "curl"), `#!/bin/bash
output=""; bar=false
while [ "$#" -gt 0 ]; do
 case "$1" in
  --progress-bar) bar=true ;;
  --output) output="$2"; shift ;;
 esac
 shift
done
if [ -n "$output" ]; then
 printf complete > "$output"
 [ "$bar" != true ] || printf '\r######## 100.0%%\n' >&2
fi
exit 0
`)
			writeDeploymentFixture(t, filepath.Join(bin, "sync"), `#!/bin/bash
if [ "$TEST_CASE" = force-pending-restore-failure ] && [ -f "$2" ] && [ "$(cat "$2")" = "$TEST_REMOTE/.deploy.interrupted" ]; then exit 1; fi
exit 0
`)
			// macOS has no flock utility; keep the orchestration fixture independent of it.
			writeDeploymentFixture(t, filepath.Join(bin, "flock"), "#!/bin/bash\n[ \"$TEST_CASE\" != force-lock-failure ] || exit 3\nexit 0\n")
			logPath := filepath.Join(dir, "events")
			if scenario == "active-update-failure" {
				for i, service := range []string{"mihomo", "mihomo-manager"} {
					writeDeploymentFixture(t, filepath.Join(remote, service+".service.active"), "")
					writeDeploymentFixture(t, filepath.Join(systemBin, service), "new-on-disk")
					held := filepath.Join(dir, "held", service)
					writeDeploymentFixture(t, held, "old-running-"+service)
					pid := "123"
					if i == 1 {
						pid = "124"
					}
					if err := os.MkdirAll(filepath.Join(proc, pid), 0755); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(held, filepath.Join(proc, pid, "exe")); err != nil {
						t.Fatal(err)
					}
				}
			}
			oldStage := filepath.Join(remote, ".deploy.interrupted")
			pending := filepath.Join(remote, ".update-state", "deployment.pending")
			if scenario == "interrupted" || scenario == "recovery-failure" || strings.HasPrefix(scenario, "force-") {
				for _, service := range []string{"mihomo", "mihomo-manager"} {
					writeDeploymentFixture(t, filepath.Join(oldStage, "backup", service+".active"), "inactive\n")
					writeDeploymentFixture(t, filepath.Join(oldStage, "backup", service+".enabled"), "disabled\n")
				}
				for _, file := range []string{".env", "scripts/entrypoint.sh"} {
					b, _ := os.ReadFile(filepath.Join(remote, file))
					writeDeploymentFixture(t, filepath.Join(oldStage, "backup", "deployment", file), string(b))
					writeDeploymentFixture(t, filepath.Join(oldStage, "backup", "deployment", file+".present"), "")
				}
				writeDeploymentFixture(t, filepath.Join(oldStage, "prepared", "before.json"), "{}")
				writeDeploymentFixture(t, filepath.Join(oldStage, "mihomo-manager"), `#!/bin/bash
echo interrupted-restore >> "$TEST_LOG"
[ "$TEST_CASE" != recovery-failure ] || exit 1
printf old-config > "$TEST_REMOTE/config.yaml"
`)
				writeDeploymentFixture(t, filepath.Join(remote, "config.yaml"), "interrupted-config")
				writeDeploymentFixture(t, pending, oldStage+"\n")
				if strings.HasPrefix(scenario, "force-") {
					for _, service := range []string{"mihomo", "mihomo-manager"} {
						writeDeploymentFixture(t, filepath.Join(units, service+".service"), "broken-unit")
					}
				}
			}
			if scenario == "force-invalid-pending-failure" {
				writeDeploymentFixture(t, pending, filepath.Join(dir, "outside-deployment")+"\n")
			}
			originalPending, _ := os.ReadFile(pending)
			originalRecovery, _ := os.ReadFile(filepath.Join(oldStage, "mihomo-manager"))
			args := []string{"./scripts/publish.sh"}
			if scenario == "build-only" {
				args = append(args, "--build-only")
			}
			if strings.HasPrefix(scenario, "force") {
				args = append(args, "--force")
			}
			cmd := exec.Command("bash", args...)
			if scenario == "terminal" {
				script, err := exec.LookPath("script")
				if err != nil {
					t.Skip("script utility is required for pseudo-terminal acceptance")
				}
				if runtime.GOOS == "darwin" {
					cmd = exec.Command(script, "-q", "/dev/null", "bash", "./scripts/publish.sh")
				} else {
					cmd = exec.Command(script, "-q", "-e", "-c", "bash ./scripts/publish.sh", "/dev/null")
				}
			}
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "TEST_LOG="+logPath, "TEST_CASE="+scenario, "TEST_REMOTE="+remote, "TEST_INSTALLED="+systemBin, "TEST_UNITS="+units)
			out, err := cmd.CombinedOutput()
			failed := strings.HasSuffix(scenario, "failure") || scenario == "unsupported" || scenario == "process-mismatch"
			if (err != nil) != failed {
				t.Fatalf("err=%v output=%s", err, out)
			}
			data, _ := os.ReadFile(logPath)
			log := string(data)
			if scenario == "terminal" && (!strings.Contains(log, "remote-terminal=1") || !strings.Contains(string(out), "100.0%")) {
				t.Fatalf("did not display remote progress through terminal publish: %s %q", log, out)
			}
			if scenario == "interrupted" && (!strings.Contains(log, "interrupted-restore") || strings.Index(log, "interrupted-restore") > strings.Index(log, "prepare-install")) {
				t.Fatal("new publication preceded interrupted deployment recovery")
			}
			if scenario == "recovery-failure" {
				stages, _ := filepath.Glob(filepath.Join(remote, ".deploy.*"))
				if len(stages) != 1 || !strings.HasSuffix(stages[0], ".deploy.interrupted") || strings.Contains(log, "prepare-install") {
					t.Fatalf("did not retain only the recovery package: %v %s %s", stages, log, out)
				}
				if _, err := os.Stat(filepath.Join(remote, ".update-state", "deployment.pending")); err != nil {
					t.Fatal("lost pending recovery marker")
				}
				return
			}
			if strings.HasPrefix(scenario, "force-") {
				if strings.Contains(log, "interrupted-restore") {
					t.Fatalf("forced installation attempted old recovery: %s", log)
				}
				retained, err := os.ReadFile(filepath.Join(oldStage, "mihomo-manager"))
				if err != nil || string(retained) != string(originalRecovery) {
					t.Fatalf("old recovery package changed: %v %s", err, retained)
				}
				stages, _ := filepath.Glob(filepath.Join(remote, ".deploy.*"))
				currentPending, pendingErr := os.ReadFile(pending)
				if scenario == "force-recovery-failure" || scenario == "force-pending-restore-failure" {
					if len(stages) != 2 || pendingErr != nil || string(currentPending) == string(originalPending) {
						t.Fatalf("lost failed forced-install recovery: %v %s %v %s", stages, currentPending, pendingErr, out)
					}
					newStage := strings.TrimSpace(string(currentPending))
					parent, err := os.ReadFile(filepath.Join(newStage, "previous-deployment.pending"))
					if err != nil || string(parent) != string(originalPending) {
						t.Fatalf("lost previous recovery reference: %v %s", err, parent)
					}
					if _, err := os.Stat(filepath.Join(newStage, "backup")); err != nil {
						t.Fatalf("pending forced recovery has no backup: %v", err)
					}
					if scenario == "force-pending-restore-failure" && !strings.Contains(string(out), "原未完成发布记录恢复失败") {
						t.Fatalf("falsely reported recovery marker restored: %s", out)
					}
				} else {
					if len(stages) != 1 || stages[0] != oldStage {
						t.Fatalf("unexpected retained packages: %v %s", stages, out)
					}
					if failed && (pendingErr != nil || string(currentPending) != string(originalPending)) {
						t.Fatalf("failed forced install lost old recovery gate: %v %s", pendingErr, currentPending)
					}
					if !failed && !os.IsNotExist(pendingErr) {
						t.Fatalf("successful forced install retained recovery gate: %v %s", pendingErr, currentPending)
					}
				}
				installed := strings.Contains(log, "\ninstall\n")
				if scenario == "force-prepare-failure" || scenario == "force-invalid-pending-failure" || scenario == "force-lock-failure" {
					if installed {
						t.Fatalf("force bypassed preflight/ownership/lock: %s", log)
					}
				} else if !installed || !strings.Contains(log, "preflight\n") || strings.Index(log, "preflight\n") > strings.Index(log, "\ninstall\n") {
					t.Fatalf("forced installation omitted preflight or install: %s", log)
				}
				if scenario == "force-install-failure" || scenario == "force-update-failure" {
					cfg, _ := os.ReadFile(filepath.Join(remote, "config.yaml"))
					if string(cfg) != "interrupted-config" || !strings.Contains(log, "restore-data") {
						t.Fatalf("forced install did not restore its current-state baseline: %s %s", cfg, log)
					}
				}
				if !failed {
					for _, service := range []string{"mihomo", "mihomo-manager"} {
						unit, _ := os.ReadFile(filepath.Join(units, service+".service"))
						if string(unit) != "new-unit" {
							t.Fatalf("forced install did not replace broken service unit: %s", unit)
						}
					}
					if !strings.Contains(log, "update\n") || !strings.Contains(log, "restart mihomo-manager.service") {
						t.Fatalf("forced install omitted activation: %s", log)
					}
				}
				env, _ := os.ReadFile(filepath.Join(remote, ".env"))
				if string(env) != "remote-fixture\n" {
					t.Fatal("forced install overwrote existing environment")
				}
				return
			}
			if scenario == "prepare-failure" && strings.Contains(log, "\ninstall\n") {
				t.Fatal("installed after failed preflight")
			}
			if strings.Contains(log, "\ninstall\n") && strings.Index(log, "preflight\n") > strings.Index(log, "\ninstall\n") {
				t.Fatal("installation preceded preflight")
			}
			changed := scenario == "install-failure" || scenario == "update-failure" || scenario == "active-update-failure" || scenario == "manager-failure" || scenario == "process-mismatch"
			if changed {
				if !strings.Contains(log, "restore-data") {
					t.Fatalf("missing compensation: %s %s", log, out)
				}
				cfg, _ := os.ReadFile(filepath.Join(remote, "config.yaml"))
				if string(cfg) != "old-config" {
					t.Fatalf("config not restored: %s", cfg)
				}
				old, _ := os.ReadFile(filepath.Join(remote, "scripts", "entrypoint.sh"))
				if string(old) != "old-runtime" {
					t.Fatal("runtime not restored")
				}
				if scenario == "active-update-failure" {
					for _, service := range []string{"mihomo", "mihomo-manager"} {
						b, err := os.ReadFile(filepath.Join(systemBin, service))
						if err != nil || string(b) != "old-running-"+service {
							t.Fatalf("did not restore actually running binary: %s %v", b, err)
						}
					}
					if !strings.Contains(log, "verify-restored") {
						t.Fatal("did not verify restored core")
					}
				} else if _, err := os.Stat(filepath.Join(systemBin, "mihomo-manager")); !os.IsNotExist(err) {
					t.Fatal("new manager left installed after compensation")
				}
			}
			if !failed && scenario != "build-only" {
				if !strings.Contains(log, "update\n") || !strings.Contains(log, "restart mihomo-manager.service") {
					t.Fatalf("publication incomplete: %s", log)
				}
			}
			env, _ := os.ReadFile(filepath.Join(remote, ".env"))
			if scenario != "new-env" && string(env) != "remote-fixture\n" {
				t.Fatal("overwrote existing environment")
			}
			if scenario == "new-env" && !strings.Contains(string(env), "local-fixture") {
				t.Fatal("missing initial environment")
			}
			stages, _ := filepath.Glob(filepath.Join(remote, ".deploy.*"))
			if len(stages) != 0 {
				t.Fatalf("staging retained after normal completion/compensation: %v %s", stages, out)
			}
			if strings.Contains(string(out), "LIBARCHIVE.xattr") {
				t.Fatal("uploaded macOS metadata")
			}
		})
	}
}

func TestPublishRejectsConflictingModesBeforeRemoteAccess(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"scripts/publish.sh", "deploy/lib/env.sh", "deploy/lib/target.sh"} {
		copyDeploymentFile(t, dir, name)
	}
	for _, args := range [][]string{{"--force", "--build-only"}, {"--build-only", "--force"}} {
		cmd := exec.Command("bash", append([]string{"./scripts/publish.sh"}, args...)...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "参数过多") {
			t.Fatalf("conflicting modes were not rejected before reading configuration: %v %s", err, out)
		}
	}
}
