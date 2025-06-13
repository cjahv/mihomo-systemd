package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
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
	process *exec.Cmd
	cancel  context.CancelFunc
	mu      sync.Mutex
}

func (h *CustomHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(200)
	
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	
	h.mu.Lock()
	h.cancel = cancel
	h.process = exec.CommandContext(ctx, "./auto_task.sh")
	h.mu.Unlock()
	
	stdout, err := h.process.StdoutPipe()
	if err != nil {
		fmt.Fprintf(w, "执行失败: %v", err)
		return
	}
	
	if err := h.process.Start(); err != nil {
		fmt.Fprintf(w, "执行失败: %v", err)
		return
	}
	
	// Go的优势：更好的流式处理和并发控制
	h.streamOutput(w, stdout, ctx)
	
	h.process.Wait()
	h.terminateProcess()
}

// 对应Python的_handle_logs()
func (h *CustomHandler) handleLogs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(200)
	
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	
	h.mu.Lock()
	h.cancel = cancel
	h.process = exec.CommandContext(ctx, "journalctl", "-n", "1000", "-fu", "mihomo")
	h.mu.Unlock()
	
	stdout, err := h.process.StdoutPipe()
	if err != nil {
		fmt.Fprintf(w, "执行失败: %v", err)
		return
	}
	
	if err := h.process.Start(); err != nil {
		fmt.Fprintf(w, "执行失败: %v", err)
		return
	}
	
	h.streamOutput(w, stdout, ctx)
	
	h.process.Wait()
	h.terminateProcess()
}

// Go的优势：更好的流式输出控制
func (h *CustomHandler) streamOutput(w http.ResponseWriter, reader io.Reader, ctx context.Context) {
	scanner := bufio.NewScanner(reader)
	flusher, _ := w.(http.Flusher)
	
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			// 客户端断开连接，对应Python的异常处理
			h.terminateProcess()
			return
		default:
			w.Write([]byte(scanner.Text() + "\n"))
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}

// 对应Python的_terminate_process()
func (h *CustomHandler) terminateProcess() {
	h.mu.Lock()
	defer h.mu.Unlock()
	
	if h.process != nil && h.process.Process != nil {
		h.process.Process.Kill()
		h.process = nil
	}
	if h.cancel != nil {
		h.cancel()
		h.cancel = nil
	}
}

// 对应Python的_handle_get_settings()
func (h *CustomHandler) handleGetSettings(w http.ResponseWriter, r *http.Request) {
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
	
	// 对应Python的复杂.env文件处理逻辑
	var envContent string
	if content, err := os.ReadFile(SECRET_ENV_PATH); err == nil {
		envContent = string(content)
	}
	
	for k, v := range data {
		if k == "secret" {
			k = "MIHOMO_SECRET"
		}
		
		valueStr := fmt.Sprintf("%v", v)
		keyPattern := k + "="
		
		lines := strings.Split(envContent, "\n")
		found := false
		
		// 查找并更新现有行
		for i, line := range lines {
			if strings.HasPrefix(strings.TrimSpace(line), keyPattern) {
				lines[i] = k + "=" + valueStr
				found = true
				break
			}
		}
		
		if !found {
			// 添加新行
			if envContent != "" && !strings.HasSuffix(envContent, "\n") {
				envContent += "\n"
			}
			envContent += k + "=" + valueStr + "\n"
		} else {
			envContent = strings.Join(lines, "\n")
		}
	}
	
	if err := os.WriteFile(SECRET_ENV_PATH, []byte(envContent), 0644); err != nil {
		h.jsonResponse(w, map[string]interface{}{
			"success": false,
			"msg":     "保存失败",
		}, 500)
		return
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

func main() {
	// 对应Python的socketserver.ThreadingTCPServer配置
	handler := &CustomHandler{}
	
	// Go的优势：更简洁的HTTP服务器设置
	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", PORT),
		Handler: handler,
	}
	
	fmt.Printf("服务已启动，端口：%d\n", PORT)
	log.Fatal(server.ListenAndServe())
} 