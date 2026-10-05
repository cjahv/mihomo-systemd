package manager

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestManagerPageIsEmbeddedAndIndependentOfWorkingDirectory(t *testing.T) {
	t.Chdir(t.TempDir())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	response := httptest.NewRecorder()
	(&managerHandler{}).ServeHTTP(response, req)
	if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), managerPage) {
		t.Fatalf("embedded page unavailable without source files: status=%d", response.Code)
	}
	if response.Header().Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("unexpected page content type: %s", response.Header().Get("Content-Type"))
	}
}

func TestRunRejectsUnknownCommandWithoutCreatingRuntimeState(t *testing.T) {
	t.Chdir(t.TempDir())
	if code := Run([]string{"unknown"}); code != 1 {
		t.Fatalf("unexpected exit code: %d", code)
	}
	if _, err := os.Stat(".update-state"); !os.IsNotExist(err) {
		t.Fatal("invalid command created runtime state")
	}
}
