package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

const SECRET_ENV_PATH = ".env"

var (
	PORT          int
	MIHOMO_SECRET string
)

// 启动时读取配置，对应Python的全局变量
func init() {
	PORT = getPort()
	MIHOMO_SECRET = getMihomoSecret()
}

// 对应Python的get_mihomo_secret()
func getMihomoSecret() string {
	if _, err := os.Stat(SECRET_ENV_PATH); os.IsNotExist(err) {
		return ""
	}

	file, err := os.Open(SECRET_ENV_PATH)
	if err != nil {
		return ""
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "MIHOMO_SECRET=") {
			return strings.SplitN(line, "=", 2)[1]
		}
	}
	return ""
}

// 对应Python的get_port()
func getPort() int {
	if _, err := os.Stat(SECRET_ENV_PATH); os.IsNotExist(err) {
		return 8000
	}

	file, err := os.Open(SECRET_ENV_PATH)
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

// CustomHandler 对应Python的CustomHandler类
type CustomHandler struct {
}

func (h *CustomHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if MIHOMO_SECRET == "" && !isLoopbackRequest(r) {
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

func (h *CustomHandler) handleGET(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/":
		// 对应Python的 self.path = "/ui.html"
		http.ServeFile(w, r, "ui.html")
	case "/reload":
		h.handleReload(w, r)
	case "/logs":
		h.handleLogs(w, r)
	case "/get_settings":
		h.handleGetSettings(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (h *CustomHandler) handlePOST(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/check_secret":
		h.handleCheckSecret(w, r)
	case "/save_settings":
		h.handleSaveSettings(w, r)
	default:
		http.NotFound(w, r)
	}
}

// 对应Python的_handle_reload()
func (h *CustomHandler) handleReload(w http.ResponseWriter, r *http.Request) {
	if !h.isAuthorized(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(200)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	cmd := exec.CommandContext(ctx, "./auto_task.sh")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintf(w, "执行失败: %v", err)
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		fmt.Fprintf(w, "执行失败: %v", err)
		return
	}

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(w, "执行失败: %v", err)
		return
	}

	// Go的优势：更好的流式处理和并发控制
	h.streamOutput(w, ctx, stdout, stderr)

	if err := cmd.Wait(); err != nil && ctx.Err() == nil {
		fmt.Fprintf(w, "执行失败: %v\n", err)
	}
}

// 对应Python的_handle_logs()
func (h *CustomHandler) handleLogs(w http.ResponseWriter, r *http.Request) {
	if !h.isAuthorized(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(200)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	cmd := exec.CommandContext(ctx, "journalctl", "-n", "1000", "-fu", "mihomo")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintf(w, "执行失败: %v", err)
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		fmt.Fprintf(w, "执行失败: %v", err)
		return
	}

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(w, "执行失败: %v", err)
		return
	}

	h.streamOutput(w, ctx, stdout, stderr)

	if err := cmd.Wait(); err != nil && ctx.Err() == nil {
		fmt.Fprintf(w, "执行失败: %v\n", err)
	}
}

// Go的优势：更好的流式输出控制
func (h *CustomHandler) streamOutput(w http.ResponseWriter, ctx context.Context, readers ...io.Reader) {
	flusher, _ := w.(http.Flusher)
	lineCh := make(chan string, 128)
	var wg sync.WaitGroup

	for _, reader := range readers {
		if reader == nil {
			continue
		}
		wg.Add(1)
		go func(r io.Reader) {
			defer wg.Done()
			scanner := bufio.NewScanner(r)
			scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
			for scanner.Scan() {
				select {
				case <-ctx.Done():
					return
				case lineCh <- scanner.Text() + "\n":
				}
			}
			if err := scanner.Err(); err != nil && ctx.Err() == nil {
				select {
				case <-ctx.Done():
				case lineCh <- fmt.Sprintf("读取输出失败: %v\n", err):
				}
			}
		}(reader)
	}

	go func() {
		wg.Wait()
		close(lineCh)
	}()

	for {
		select {
		case <-ctx.Done():
			// 客户端断开连接，对应Python的异常处理
			return
		case line, ok := <-lineCh:
			if !ok {
				return
			}
			_, _ = w.Write([]byte(line))
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}

// 对应Python的_handle_get_settings()
func (h *CustomHandler) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	if !h.isAuthorized(r) {
		h.jsonResponse(w, map[string]interface{}{
			"success": false,
			"msg":     "未授权",
		}, http.StatusUnauthorized)
		return
	}

	settings := make(map[string]string)

	if _, err := os.Stat(SECRET_ENV_PATH); !os.IsNotExist(err) {
		file, err := os.Open(SECRET_ENV_PATH)
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
func (h *CustomHandler) handleCheckSecret(w http.ResponseWriter, r *http.Request) {
	var data map[string]interface{}

	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		h.jsonResponse(w, map[string]interface{}{
			"success": false,
			"msg":     "请求格式错误",
		}, 400)
		return
	}

	inputSecret, _ := data["secret"].(string)
	// 对应Python的逻辑：not MIHOMO_SECRET or input_secret == MIHOMO_SECRET
	passed := (MIHOMO_SECRET == "" || inputSecret == MIHOMO_SECRET)

	response := map[string]interface{}{
		"success": passed,
	}
	if !passed {
		response["msg"] = "密钥错误"
	}

	h.jsonResponse(w, response, 200)
}

// 对应Python的_handle_save_settings()
func (h *CustomHandler) handleSaveSettings(w http.ResponseWriter, r *http.Request) {
	var data map[string]interface{}

	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		h.jsonResponse(w, map[string]interface{}{
			"success": false,
			"msg":     "请求格式错误",
		}, 400)
		return
	}

	inputSecret, _ := data["secret"].(string)
	if MIHOMO_SECRET != "" && inputSecret != MIHOMO_SECRET {
		h.jsonResponse(w, map[string]interface{}{
			"success": false,
			"msg":     "密钥错误",
		}, 403)
		return
	}

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
	if content, err := os.ReadFile(SECRET_ENV_PATH); err == nil {
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

	if err := os.WriteFile(SECRET_ENV_PATH, []byte(envContent), 0600); err != nil {
		h.jsonResponse(w, map[string]interface{}{
			"success": false,
			"msg":     "保存失败",
		}, 500)
		return
	}

	if newSecret, ok := updates["MIHOMO_SECRET"]; ok {
		MIHOMO_SECRET = newSecret
	}

	h.jsonResponse(w, map[string]interface{}{
		"success": true,
	}, 200)
}

// 对应Python的_json_response()
func (h *CustomHandler) jsonResponse(w http.ResponseWriter, data interface{}, code int) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(data)
}

func (h *CustomHandler) isAuthorized(r *http.Request) bool {
	if MIHOMO_SECRET == "" {
		return isLoopbackRequest(r)
	}
	return requestSecret(r) == MIHOMO_SECRET
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

func main() {
	// 对应Python的socketserver.ThreadingTCPServer配置
	handler := &CustomHandler{}

	// Go的优势：更简洁的HTTP服务器设置
	server := &http.Server{
		Addr:              fmt.Sprintf(":%d", PORT),
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	if MIHOMO_SECRET == "" {
		log.Printf("未设置 MIHOMO_SECRET，已限制仅允许本机访问")
	}
	fmt.Printf("服务已启动，端口：%d\n", PORT)
	log.Fatal(server.ListenAndServe())
}
