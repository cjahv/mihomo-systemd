package manager

import (
	"bufio"
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const environmentPath = ".env"

//go:embed web/index.html
var managerPage []byte

var (
	managerSecret string
	secretMu      sync.RWMutex
)

// 对应Python的get_mihomo_secret()
func getMihomoSecret() string {
	env, err := readEnvironment(environmentPath)
	if err != nil {
		return ""
	}
	return env["MIHOMO_SECRET"]
}

func currentSecret() string {
	secretMu.RLock()
	defer secretMu.RUnlock()
	return managerSecret
}

func setSecret(secret string) {
	secretMu.Lock()
	defer secretMu.Unlock()
	managerSecret = secret
}

// 对应Python的get_port()
func getPort() int {
	if _, err := os.Stat(environmentPath); os.IsNotExist(err) {
		return 8000
	}

	file, err := os.Open(environmentPath)
	if err != nil {
		return 8000
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "PORT=") {
			portStr := strings.SplitN(line, "=", 2)[1]
			if port, err := strconv.Atoi(portStr); err == nil {
				return port
			}
		}
	}
	return 8000
}

// managerHandler 对应Python的CustomHandler类
type managerHandler struct {
	updateDir      string
	updaterFactory func(string, io.Writer) *configUpdater
}

func (h *managerHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if currentSecret() == "" && !isLoopbackRequest(r) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	switch r.Method {
	case "GET":
		h.handleGET(w, r)
	case "POST":
		h.handlePOST(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (h *managerHandler) handleGET(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/":
		http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(managerPage))
	case "/reload":
		h.handleReload(w, r)
	case "/update_status":
		h.handleUpdateStatus(w, r)
	case "/logs":
		h.handleLogs(w, r)
	case "/get_settings":
		h.handleGetSettings(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (h *managerHandler) handlePOST(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/reload":
		h.handleReload(w, r)
	case "/check_secret":
		h.handleCheckSecret(w, r)
	case "/save_settings":
		h.handleSaveSettings(w, r)
	default:
		http.NotFound(w, r)
	}
}

// 对应Python的_handle_get_settings()
func (h *managerHandler) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	if !h.isAuthorized(r) {
		h.jsonResponse(w, map[string]interface{}{
			"success": false,
			"msg":     "未授权",
		}, http.StatusUnauthorized)
		return
	}

	settings := make(map[string]string)

	if _, err := os.Stat(environmentPath); !os.IsNotExist(err) {
		file, err := os.Open(environmentPath)
		if err == nil {
			defer file.Close()
			scanner := bufio.NewScanner(file)
			for scanner.Scan() {
				line := strings.TrimSpace(scanner.Text())
				if line == "" || !strings.Contains(line, "=") {
					continue
				}
				parts := strings.SplitN(line, "=", 2)
				key, value := parts[0], parts[1]
				// 排除密钥，对应Python逻辑
				if key != "MIHOMO_SECRET" {
					settings[key] = value
				}
			}
		}
	}

	response := map[string]interface{}{
		"success": true,
		"data":    settings,
	}
	h.jsonResponse(w, response, 200)
}

// 对应Python的_handle_check_secret()
func (h *managerHandler) handleCheckSecret(w http.ResponseWriter, r *http.Request) {
	var data map[string]interface{}

	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		h.jsonResponse(w, map[string]interface{}{
			"success": false,
			"msg":     "请求格式错误",
		}, 400)
		return
	}

	inputSecret, _ := data["secret"].(string)
	// 对应Python的逻辑：not managerSecret or input_secret == managerSecret
	passed := (currentSecret() == "" || inputSecret == currentSecret())

	response := map[string]interface{}{
		"success": passed,
	}
	if !passed {
		response["msg"] = "密钥错误"
	}

	h.jsonResponse(w, response, 200)
}

// 对应Python的_handle_save_settings()
func (h *managerHandler) handleSaveSettings(w http.ResponseWriter, r *http.Request) {
	var data map[string]interface{}

	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		h.jsonResponse(w, map[string]interface{}{
			"success": false,
			"msg":     "请求格式错误",
		}, 400)
		return
	}

	inputSecret, _ := data["secret"].(string)
	if currentSecret() != "" && inputSecret != currentSecret() {
		h.jsonResponse(w, map[string]interface{}{
			"success": false,
			"msg":     "密钥错误",
		}, 403)
		return
	}

	lock, err := acquireUpdateLock(h.updateDirectory())
	if err != nil {
		code := http.StatusInternalServerError
		if err == errUpdateBusy {
			code = http.StatusConflict
		}
		h.jsonResponse(w, map[string]interface{}{"success": false, "msg": err.Error()}, code)
		return
	}
	defer lock.Close()

	allowedKeys := []string{
		"SKIP_CNIP",
		"QUIC",
		"LOCAL_LOOPBACK_PROXY",
		"CONFIG_URL",
		"GITHUB_PROXY",
		"GITHUB_API_PROXY",
		"MIHOMO_SECRET",
	}
	boolKeys := map[string]struct{}{
		"SKIP_CNIP":            {},
		"QUIC":                 {},
		"LOCAL_LOOPBACK_PROXY": {},
	}

	updates := make(map[string]string)
	for _, key := range allowedKeys {
		raw, ok := data[key]
		if !ok {
			continue
		}
		valueStr := strings.TrimSpace(fmt.Sprintf("%v", raw))
		if strings.ContainsAny(valueStr, "\n\r") {
			h.jsonResponse(w, map[string]interface{}{
				"success": false,
				"msg":     "配置值包含非法换行",
			}, 400)
			return
		}
		if _, isBool := boolKeys[key]; isBool {
			if valueStr != "true" && valueStr != "false" {
				h.jsonResponse(w, map[string]interface{}{
					"success": false,
					"msg":     "布尔配置值必须为 true 或 false",
				}, 400)
				return
			}
		}
		if key == "MIHOMO_SECRET" && valueStr == "" {
			// 空值不更新密钥，避免误清空
			continue
		}
		updates[key] = valueStr
	}

	if len(updates) == 0 {
		h.jsonResponse(w, map[string]interface{}{
			"success": true,
		}, 200)
		return
	}

	// 对应Python的复杂.env文件处理逻辑
	var envContent string
	if content, err := os.ReadFile(environmentPath); err == nil {
		envContent = string(content)
	}

	lines := strings.Split(envContent, "\n")
	if len(lines) == 1 && lines[0] == "" {
		lines = []string{}
	}

	for _, key := range allowedKeys {
		valueStr, ok := updates[key]
		if !ok {
			continue
		}
		keyPattern := key + "="
		found := false
		for i, line := range lines {
			if strings.HasPrefix(strings.TrimSpace(line), keyPattern) {
				lines[i] = keyPattern + valueStr
				found = true
				break
			}
		}
		if !found {
			lines = append(lines, keyPattern+valueStr)
		}
	}

	envContent = strings.Join(lines, "\n")
	if !strings.HasSuffix(envContent, "\n") {
		envContent += "\n"
	}

	if err := atomicWrite(environmentPath, []byte(envContent), 0600); err != nil {
		h.jsonResponse(w, map[string]interface{}{
			"success": false,
			"msg":     "保存失败",
		}, 500)
		return
	}

	if newSecret, ok := updates["MIHOMO_SECRET"]; ok {
		setSecret(newSecret)
	}

	h.jsonResponse(w, map[string]interface{}{
		"success": true,
	}, 200)
}

// 对应Python的_json_response()
func (h *managerHandler) jsonResponse(w http.ResponseWriter, data interface{}, code int) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(data)
}

func (h *managerHandler) isAuthorized(r *http.Request) bool {
	if currentSecret() == "" {
		return isLoopbackRequest(r)
	}
	return requestSecret(r) == currentSecret()
}

func isLoopbackRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

func requestSecret(r *http.Request) string {
	if secret := r.Header.Get("X-Mihomo-Secret"); secret != "" {
		return secret
	}
	return ""
}

// Run 执行管理器命令；调用方只负责将返回值作为进程退出码。
func Run(args []string) int {
	if len(args) == 1 && args[0] == "--version" {
		fmt.Printf("mihomo-manager %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
		return 0
	}
	if len(args) > 0 {
		if args[0] != "update" {
			fmt.Fprintln(os.Stderr, "用法: mihomo-manager [--version|update [--retry|--recover-only]]")
			return 1
		}
		return runUpdateCommand(args[1:])
	}
	dir, err := os.Getwd()
	if err != nil {
		log.Print(err)
		return 1
	}
	serverPort := getPort()
	setSecret(getMihomoSecret())
	recoverOnStartup(dir)
	server := &http.Server{
		Addr:              fmt.Sprintf(":%d", serverPort),
		Handler:           &managerHandler{},
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	if currentSecret() == "" {
		log.Printf("未设置 MIHOMO_SECRET，已限制仅允许本机访问")
	}
	fmt.Printf("服务已启动，端口：%d\n", serverPort)
	if err := server.ListenAndServe(); err != nil {
		log.Print(err)
		return 1
	}
	return 0
}
