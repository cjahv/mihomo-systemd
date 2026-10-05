package manager

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestProviderFreshnessAndSourceIdentity(t *testing.T) {
	for _, scenario := range []string{"fresh", "expired", "future", "missing", "empty", "zero", "omitted", "changed-url", "changed-header", "changed-format", "changed-interval", "extended-interval", "changed-path", "proxy-provider"} {
		t.Run(scenario, func(t *testing.T) {
			r := providerFixture(t)
			var logs bytes.Buffer
			r.output = &logs
			stage := t.TempDir()
			old := providerOptions("https://example.invalid/rules", "rules/cache")
			options := providerOptions("https://example.invalid/rules", "rules/cache")
			modified := time.Now().Add(-time.Minute).Truncate(time.Second)
			switch scenario {
			case "expired", "zero", "omitted":
				modified = time.Now().Add(-2 * time.Hour).Truncate(time.Second)
			case "extended-interval":
				modified = time.Now().Add(-90 * time.Minute).Truncate(time.Second)
			case "future":
				modified = time.Now().Add(time.Hour).Truncate(time.Second)
			}
			switch scenario {
			case "zero":
				old["interval"], options["interval"] = 0, 0
			case "omitted":
				delete(old, "interval")
				delete(options, "interval")
			case "changed-url":
				options["url"] = "https://different.invalid/rules"
			case "changed-header":
				options["header"] = map[string]interface{}{"X-Source": []string{"new"}}
			case "changed-format":
				options["format"] = "text"
			case "changed-interval":
				options["interval"] = 60
			case "extended-interval":
				options["interval"] = 7200
			case "changed-path":
				options["path"] = "rules/new-path"
			}
			p := providerSpec{kind: "rule-providers", name: "remote"}
			if scenario == "proxy-provider" {
				p.kind = "proxy-providers"
			}
			for dir, declaration := range map[string]map[string]interface{}{r.dir: old, stage: options} {
				cfg := map[string]interface{}{p.kind: map[string]interface{}{p.name: declaration}, "rules": []string{"MATCH,DIRECT"}}
				if err := writeJSON(filepath.Join(dir, "config.yaml"), cfg); err != nil {
					t.Fatal(err)
				}
			}
			if scenario != "missing" {
				body := []byte("cached")
				if scenario == "empty" {
					body = nil
				}
				if err := os.MkdirAll(filepath.Join(r.dir, "rules"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := atomicWriteAt(filepath.Join(r.dir, "rules/cache"), body, 0600, modified); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.stageProviders(context.Background(), stage, true); err != nil {
				t.Fatal(err)
			}
			wantCached := scenario == "fresh" || scenario == "zero" || scenario == "omitted" || scenario == "changed-path" || scenario == "extended-interval" || scenario == "proxy-provider"
			if strings.Contains(logs.String(), "[下载]") == wantCached {
				t.Fatalf("wrong fetch decision (cached=%v): %s", wantCached, &logs)
			}
			if wantCached && (!strings.Contains(logs.String(), "未过期") || !strings.Contains(logs.String(), "https://example.invalid/rules")) {
				t.Fatalf("missing cache decision/URL: %s", &logs)
			}
			cache, err := readProviderCache(stage, p.managedPath())
			if err != nil {
				t.Fatal(err)
			}
			if wantCached {
				if string(cache.data) != "cached" || !cache.modified.Equal(modified) {
					t.Fatalf("cache copy changed content/time: %+v", cache)
				}
			} else if string(cache.data) != "payload:\n  - example.com\n" || time.Since(cache.modified) > time.Minute {
				t.Fatalf("did not prepare fresh download: %+v", cache)
			}
			if scenario != "missing" {
				stat, err := os.Stat(filepath.Join(r.dir, "rules/cache"))
				if err != nil || !stat.ModTime().Equal(modified) {
					t.Fatalf("modified live timestamp: %v %v", stat, err)
				}
			}
		})
	}
}

func TestProviderIntervalAndExpiryBoundary(t *testing.T) {
	for _, value := range []interface{}{-1, 1.5, "bad", math.Inf(1), math.NaN(), float64(math.MaxInt64), true} {
		if _, err := providerInterval(map[string]interface{}{"interval": value}); err == nil {
			t.Fatalf("accepted invalid interval %v", value)
		}
	}
	now := time.Now()
	cache := providerCache{data: []byte("rules"), modified: now.Add(-time.Hour)}
	if cache.fresh(now, time.Hour) || !cache.fresh(now.Add(-time.Nanosecond), time.Hour) {
		t.Fatal("expiry boundary is not exact")
	}
}

func TestExpiredProviderFallbackKeepsOriginalTimestamp(t *testing.T) {
	r := providerFixture(t)
	t.Setenv("TEST_DOWNLOAD_FAIL", "true")
	var logs bytes.Buffer
	r.output = &logs
	stage := t.TempDir()
	options := providerOptions("https://example.invalid/rules", "rules/cache")
	writeProviderConfig(t, r.dir, map[string]interface{}{"remote": options})
	writeProviderConfig(t, stage, map[string]interface{}{"remote": options})
	modified := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	if err := writeProviderSnapshot(r.dir, map[string][]byte{"rules/cache": []byte("cached")}, map[string]time.Time{"rules/cache": modified}); err != nil {
		t.Fatal(err)
	}
	if err := r.stageProviders(context.Background(), stage, true); err != nil {
		t.Fatal(err)
	}
	p := providerSpec{kind: "rule-providers", name: "remote"}
	cache, err := readProviderCache(stage, p.managedPath())
	if err != nil || !cache.modified.Equal(modified) || string(cache.data) != "cached" {
		t.Fatalf("fallback refreshed stale cache: %+v %v", cache, err)
	}
	if !strings.Contains(logs.String(), "沿用同来源过期缓存") {
		t.Fatalf("missing stale-cache message: %s", &logs)
	}
}

func realProviderCurl(t *testing.T) *linuxRuntime {
	t.Helper()
	r := providerFixture(t)
	if err := os.Remove(filepath.Join(r.dir, "bin/curl")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOWNLOAD_PROGRESS", "off")
	return r
}

func TestProviderDownloadsAreBoundedAndParallel(t *testing.T) {
	r := realProviderCurl(t)
	var logs bytes.Buffer
	r.output = &logs // The race detector checks synchronization of parallel output.
	var active, peak, requests atomic.Int32
	started := make(chan struct{}, 16)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)
		for previous := peak.Load(); n > previous && !peak.CompareAndSwap(previous, n); previous = peak.Load() {
		}
		requests.Add(1)
		started <- struct{}{}
		select {
		case <-release:
			fmt.Fprint(w, "payload:\n  - example.com\n")
		case <-req.Context().Done():
		}
	}))
	defer server.Close()
	stage := t.TempDir()
	options := make(map[string]interface{})
	for i := range 9 {
		options[fmt.Sprintf("rule-%02d", i)] = providerOptions(server.URL+fmt.Sprintf("/rule-%d", i), fmt.Sprintf("rules/%d", i))
	}
	writeProviderConfig(t, stage, options)
	original := readFile(t, filepath.Join(stage, "config.yaml"))
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.stageProviders(ctx, stage, true) }()
	for range providerDownloadWorkers {
		select {
		case <-started:
		case <-ctx.Done():
			close(release)
			<-done
			t.Fatal("downloads did not run concurrently")
		}
	}
	if got := readFile(t, filepath.Join(stage, "config.yaml")); got != original {
		t.Fatal("rewrote config before providers finished")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if peak.Load() != providerDownloadWorkers || requests.Load() != 9 {
		t.Fatalf("peak=%d requests=%d", peak.Load(), requests.Load())
	}
	resources, err := r.Resources(filepath.Join(stage, "config.yaml"))
	if err != nil || len(resources) != 9 {
		t.Fatalf("resources=%d err=%v", len(resources), err)
	}
}

func TestProviderCancellationKillsDownloadsAndDoesNotUseStaleCache(t *testing.T) {
	r := realProviderCurl(t)
	started := make(chan struct{}, 8)
	closed := make(chan struct{}, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		started <- struct{}{}
		<-req.Context().Done()
		closed <- struct{}{}
	}))
	defer server.Close()
	options := make(map[string]interface{})
	cache := make(map[string][]byte)
	times := make(map[string]time.Time)
	for i := range 7 {
		name := fmt.Sprintf("rules/%d", i)
		options[fmt.Sprint(i)] = providerOptions(server.URL+"/"+fmt.Sprint(i), name)
		cache[name], times[name] = []byte("cached"), time.Now().Add(-2*time.Hour)
	}
	writeProviderConfig(t, r.dir, options)
	if err := writeProviderSnapshot(r.dir, cache, times); err != nil {
		t.Fatal(err)
	}
	stage := t.TempDir()
	writeProviderConfig(t, stage, options)
	original := readFile(t, filepath.Join(stage, "config.yaml"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.stageProviders(ctx, stage, true) }()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for range providerDownloadWorkers {
		select {
		case <-started:
		case <-timer.C:
			cancel()
			<-done
			t.Fatal("requests did not start")
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation became cache success: %v", err)
		}
	case <-timer.C:
		t.Fatal("cancelled processes did not exit")
	}
	for range providerDownloadWorkers {
		select {
		case <-closed:
		case <-timer.C:
			t.Fatal("a curl descendant remained running")
		}
	}
	if readFile(t, filepath.Join(stage, "config.yaml")) != original {
		t.Fatal("cancelled preparation rewrote config")
	}
}

func TestProviderSnapshotPreservesTimeAndUnchangedRefresh(t *testing.T) {
	u, f := setupUpdater(t)
	f.resourceNames = []string{"rules/cache"}
	f.resourceCandidate = map[string][]byte{"rules/cache": []byte("cached")}
	f.candidate, f.newCIDR = []byte("old"), []byte("old-cidr")
	modified := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	if err := writeProviderSnapshot(u.dir, f.resourceCandidate, map[string]time.Time{"rules/cache": modified}); err != nil {
		t.Fatal(err)
	}
	before, err := u.snapshot(u.dir)
	if err != nil || !before.ResourceTimes["rules/cache"].Equal(modified) {
		t.Fatalf("snapshot lost timestamp: %+v %v", before.ResourceTimes, err)
	}
	materialized := t.TempDir()
	if err := u.materialize(materialized, before); err != nil {
		t.Fatal(err)
	}
	stat, err := os.Stat(filepath.Join(materialized, "rules/cache"))
	if err != nil || !stat.ModTime().Equal(modified) {
		t.Fatalf("materialization refreshed timestamp: %v %v", stat, err)
	}
	beforeInode, _ := os.Stat(filepath.Join(u.dir, "rules/cache"))
	if err := u.RunLocked(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	assertResult(t, u, "unchanged", "old")
	after, err := os.Stat(filepath.Join(u.dir, "rules/cache"))
	if err != nil || !after.ModTime().After(modified) || !os.SameFile(beforeInode, after) || f.restarts != 0 {
		t.Fatalf("same-content refresh replaced cache/restarted core or lost time: %v %v restarts=%d", after, err, f.restarts)
	}
	var confirmed configSnapshot
	if err := readJSON(u.path("confirmed.json"), &confirmed); err != nil || !confirmed.ResourceTimes["rules/cache"].Equal(after.ModTime()) {
		t.Fatalf("confirmation lost refresh timestamp: %+v %v", confirmed.ResourceTimes, err)
	}
	// Recovery records with unknown age must not turn old bytes into a fresh download.
	before.ResourceTimes = nil
	if err := u.apply(before); err != nil {
		t.Fatal(err)
	}
	stat, err = os.Stat(filepath.Join(u.dir, "rules/cache"))
	if err != nil || !stat.ModTime().Equal(time.Unix(0, 0)) {
		t.Fatalf("unknown timestamp became fresh: %v %v", stat, err)
	}
}

func TestProviderFailureCancelsSiblingRequests(t *testing.T) {
	r := realProviderCurl(t)
	started := make(chan struct{}, 8)
	closed := make(chan struct{}, 8)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		started <- struct{}{}
		if req.URL.Path == "/0" {
			select {
			case <-release:
				w.WriteHeader(http.StatusNotFound)
			case <-req.Context().Done():
			}
			return
		}
		<-req.Context().Done()
		closed <- struct{}{}
	}))
	defer server.Close()
	options := make(map[string]interface{})
	for i := range 7 {
		options[fmt.Sprint(i)] = providerOptions(server.URL+"/"+fmt.Sprint(i), "rules/"+fmt.Sprint(i))
	}
	stage := t.TempDir()
	writeProviderConfig(t, stage, options)
	original := readFile(t, filepath.Join(stage, "config.yaml"))
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.stageProviders(ctx, stage, true) }()
	for range providerDownloadWorkers {
		select {
		case <-started:
		case <-ctx.Done():
			close(release)
			<-done
			t.Fatal("sibling requests did not start")
		}
	}
	close(release)
	err := <-done
	if err == nil || !strings.Contains(err.Error(), "rule-providers[0]") || !strings.Contains(err.Error(), "curl 22") {
		t.Fatalf("lost original failure: %v", err)
	}
	for range providerDownloadWorkers - 1 {
		select {
		case <-closed:
		case <-ctx.Done():
			t.Fatal("sibling process remained running after failure")
		}
	}
	if readFile(t, filepath.Join(stage, "config.yaml")) != original {
		t.Fatal("failure published partial provider paths")
	}
}

func TestProviderPreparationEnforcesAggregateLimit(t *testing.T) {
	r := realProviderCurl(t)
	body := bytes.Repeat([]byte("x"), 8<<20)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write(body)
	}))
	defer server.Close()
	options := make(map[string]interface{})
	for i := range 9 {
		options[fmt.Sprint(i)] = providerOptions(server.URL+"/"+fmt.Sprint(i), "rules/"+fmt.Sprint(i))
	}
	stage := t.TempDir()
	writeProviderConfig(t, stage, options)
	original := readFile(t, filepath.Join(stage, "config.yaml"))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := r.stageProviders(ctx, stage, true)
	if err == nil || !strings.Contains(err.Error(), "64 MiB") {
		t.Fatalf("aggregate provider limit was not enforced: %v", err)
	}
	if readFile(t, filepath.Join(stage, "config.yaml")) != original {
		t.Fatal("oversized resources published partial configuration")
	}
}
