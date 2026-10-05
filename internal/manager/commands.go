package manager

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

func runUpdateCommand(args []string) int {
	flags := flag.NewFlagSet("update", flag.ContinueOnError)
	retry := flags.Bool("retry", false, "忽略同一失败候选的自动更新冷却")
	recoverOnly := flags.Bool("recover-only", false, "仅恢复未完成的更新")
	prepareDir := flags.String("prepare-only", "", "准备部署候选，不切换活动配置")
	prepared := flags.String("prepared", "", "应用已经预检的候选快照")
	restore := flags.String("restore-snapshot", "", "恢复部署前文件快照，由发布流程负责服务恢复")
	check := flags.String("check-snapshot", "", "安装前检查活动文件仍与恢复快照一致")
	verify := flags.String("verify-snapshot", "", "恢复策略选择并验收已恢复服务及原有可达性")
	sourceDir := flags.String("source-dir", "", "生产配置目录")
	packageRoot := flags.String("runtime-dir", "", "本次发布的脚本目录")
	envFile := flags.String("env-file", "", "预检环境文件")
	kernel := flags.String("kernel", "", "预检核心绝对路径")
	lockFD := flags.Int("lock-fd", -1, "继承发布流程持有的更新锁")
	forceRestart := flags.Bool("force-restart", false, "发布时强制加载已安装核心")
	if flags.Parse(args) != nil || flags.NArg() != 0 {
		return 1
	}
	dir, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if *sourceDir != "" {
		dir = *sourceDir
	}
	for _, path := range []string{dir, *prepareDir, *prepared, *restore, *check, *verify, *packageRoot, *envFile, *kernel} {
		if path != "" && !filepath.IsAbs(path) {
			fmt.Fprintln(os.Stderr, "发布参数必须为绝对路径")
			return 1
		}
	}
	modes := 0
	for _, enabled := range []bool{*recoverOnly, *prepareDir != "", *prepared != "", *restore != "", *check != "", *verify != ""} {
		if enabled {
			modes++
		}
	}
	if modes > 1 {
		fmt.Fprintln(os.Stderr, "更新模式不能组合")
		return 1
	}
	var lock *updateLock
	if *lockFD >= 0 {
		lock, err = inheritedUpdateLock(dir, *lockFD)
	} else {
		lock, err = acquireUpdateLock(dir)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		if errors.Is(err, errUpdateBusy) {
			return 3
		}
		return 1
	}
	defer lock.Close()
	u := newConfigUpdater(dir, os.Stdout)
	u.envFile, u.forceRestart = *envFile, *forceRestart
	runtime := &linuxRuntime{dir: dir, packageRoot: *packageRoot, envFile: *envFile, kernel: *kernel, output: os.Stdout}
	u.runtime = runtime
	if *restore != "" {
		var before configSnapshot
		if err = readJSON(*restore, &before); err == nil {
			err = u.apply(before)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	if *check != "" || *verify != "" {
		path := *check
		if *verify != "" {
			path = *verify
		}
		var before configSnapshot
		err = readJSON(path, &before)
		if err == nil && *check != "" {
			err = u.checkDeploymentSource(before)
		} else if err == nil {
			runtime.pid, err = runtime.mainPID(ctx)
			if err == nil {
				readyCtx, readyCancel := context.WithTimeout(ctx, u.readiness)
				err = runtime.Ready(readyCtx)
				readyCancel()
			}
			if err == nil {
				probeCtx, probeCancel := context.WithTimeout(ctx, u.window)
				err = runtime.RestoreSelections(probeCtx, before.Selections)
				if err == nil && before.Verified {
					err = runtime.Probe(probeCtx)
				}
				probeCancel()
			}
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		return 0
	}
	if *prepareDir != "" {
		if err = u.prepareDeployment(ctx, *prepareDir); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
	if *prepared != "" {
		u.prepared = &configSnapshot{}
		if err = readJSON(*prepared, u.prepared); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	if err = u.RecoverLocked(); err != nil {
		if u.status.Message != err.Error() {
			fmt.Fprintln(os.Stderr, err)
		}
		if *recoverOnly {
			return 2
		}
		if _, e := os.Stat(u.path("transaction.json")); !errors.Is(e, os.ErrNotExist) {
			return 2
		}
	}
	if *recoverOnly {
		return 0
	}
	if err = u.begin(updateID()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err = u.RunLocked(ctx, *retry); err != nil {
		if u.status.Message != err.Error() {
			fmt.Fprintln(os.Stderr, err)
		}
		if u.status.Result == "rolled_back" || u.status.Result == "recovery_failed" {
			return 2
		}
		return 1
	}
	return 0
}

func recoverOnStartup(dir string) {
	lock, err := acquireUpdateLock(dir)
	if errors.Is(err, errUpdateBusy) {
		return
	} // A cron worker is still the transaction owner.
	if err != nil {
		log.Printf("检查未完成更新失败: %v", err)
		return
	}
	defer lock.Close()
	if err = newConfigUpdater(dir, os.Stdout).RecoverLocked(); err != nil {
		log.Printf("上次更新恢复结果: %v", err)
	}
}

func (h *managerHandler) updateDirectory() string {
	if h.updateDir != "" {
		return h.updateDir
	}
	dir, _ := os.Getwd()
	return dir
}
func (h *managerHandler) updater(dir string) *configUpdater {
	if h.updaterFactory != nil {
		return h.updaterFactory(dir, io.Discard)
	}
	return newConfigUpdater(dir, os.Stdout)
}

func (h *managerHandler) handleUpdateStatus(w http.ResponseWriter, r *http.Request) {
	if !h.isAuthorized(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s, err := readStatus(h.updateDirectory())
	if errors.Is(err, os.ErrNotExist) {
		h.jsonResponse(w, updateStatus{Stage: "idle", Message: "暂无更新任务"}, 200)
		return
	}
	if err != nil {
		http.Error(w, "读取更新状态失败", 500)
		return
	}
	h.jsonResponse(w, s, 200)
}

func (h *managerHandler) handleReload(w http.ResponseWriter, r *http.Request) {
	if !h.isAuthorized(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	dir := h.updateDirectory()
	lock, err := acquireUpdateLock(dir)
	if err != nil {
		code := 500
		if errors.Is(err, errUpdateBusy) {
			code = 409
		}
		http.Error(w, err.Error(), code)
		return
	}
	u := h.updater(dir)
	if err = u.RecoverLocked(); err != nil {
		if _, e := os.Stat(filepath.Join(dir, ".update-state", "transaction.json")); !errors.Is(e, os.ErrNotExist) {
			lock.Close()
			http.Error(w, "上次更新尚未恢复，请检查服务日志", 500)
			return
		}
	}
	id := updateID()
	if err = u.begin(id); err != nil {
		lock.Close()
		http.Error(w, "无法保存更新状态", 500)
		return
	}
	go func() {
		defer lock.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		if err := u.RunLocked(ctx, true); err != nil {
			log.Printf("更新任务 %s: %v", id, err)
		}
	}()
	w.Header().Set("Cache-Control", "no-store")
	h.jsonResponse(w, map[string]string{"id": id}, http.StatusAccepted)
}
