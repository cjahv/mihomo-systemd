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
	if flags.Parse(args) != nil || flags.NArg() != 0 {
		return 1
	}
	dir, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	lock, err := acquireUpdateLock(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		if errors.Is(err, errUpdateBusy) {
			return 3
		}
		return 1
	}
	defer lock.Close()
	u := newConfigUpdater(dir, os.Stdout)
	if err = u.RecoverLocked(); err != nil {
		fmt.Fprintln(os.Stderr, err)
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	if err = u.RunLocked(ctx, *retry); err != nil {
		fmt.Fprintln(os.Stderr, err)
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
