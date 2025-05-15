# -*- coding: utf-8 -*-
import http.server
import socketserver
import os
import subprocess
import json
import re

SECRET_ENV_PATH = ".env"

def get_mihomo_secret():
    if not os.path.exists(SECRET_ENV_PATH):
        return None
    with open(SECRET_ENV_PATH, "r", encoding="utf-8") as f:
        for line in f:
            if line.strip().startswith("MIHOMO_SECRET="):
                return line.strip().split("=", 1)[1]
    return None

def get_port():
    if os.path.exists(SECRET_ENV_PATH):
        with open(SECRET_ENV_PATH, "r", encoding="utf-8") as f:
            for line in f:
                if line.strip().startswith("PORT="):
                    value = line.strip().split("=", 1)[1]
                    if value.isdigit():
                        return int(value)
    return 8000

PORT = get_port()

MIHOMO_SECRET = get_mihomo_secret()  # 启动时只读取一次

class CustomHandler(http.server.SimpleHTTPRequestHandler):
    def __init__(self, *args, **kwargs):
        self.process = None
        super().__init__(*args, **kwargs)

    def do_GET(self):
        if self.path == "/":
            self.path = "/ui.html"
            return super().do_GET()
        if self.path == "/reload":
            self._handle_reload()
        elif self.path == "/logs":
            self._handle_logs()
        elif self.path == "/get_settings":
            self._handle_get_settings()
        else:
            self._not_found()

    def do_POST(self):
        if self.path == "/check_secret":
            self._handle_check_secret()
        elif self.path == "/save_settings":
            self._handle_save_settings()
        else:
            self._not_found()

    def _handle_reload(self):
        self.send_response(200)
        self.send_header("Content-type", "text/plain; charset=utf-8")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("X-Content-Type-Options", "nosniff")
        self.end_headers()
        
        try:
            self.process = subprocess.Popen(
                ["./auto_task.sh"], 
                stdout=subprocess.PIPE, 
                stderr=subprocess.STDOUT,
                text=True,
                bufsize=1
            )
            
            try:
                for line in iter(self.process.stdout.readline, ''):
                    self.wfile.write(line.encode('utf-8'))
                    self.wfile.flush()
            except (BrokenPipeError, ConnectionResetError):
                # 客户端关闭连接，终止进程
                self._terminate_process()
                return
                
            self.process.stdout.close()
            self.process.wait()
            
        except Exception as e:
            self.wfile.write(f"执行失败: {e}".encode('utf-8'))
        finally:
            self._terminate_process()

    def _handle_logs(self):
        self.send_response(200)
        self.send_header("Content-type", "text/plain; charset=utf-8")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("X-Content-Type-Options", "nosniff")
        self.end_headers()
        
        try:
            self.process = subprocess.Popen(
                ["journalctl", "-n", "1000", "-fu", "mihomo"], 
                stdout=subprocess.PIPE, 
                stderr=subprocess.STDOUT,
                text=True,
                bufsize=1
            )
            
            try:
                for line in iter(self.process.stdout.readline, ''):
                    self.wfile.write(line.encode('utf-8'))
                    self.wfile.flush()
            except (BrokenPipeError, ConnectionResetError):
                # 客户端关闭连接，终止进程
                self._terminate_process()
                return
                
            self.process.stdout.close()
            self.process.wait()
            
        except Exception as e:
            self.wfile.write(f"执行失败: {e}".encode('utf-8'))
        finally:
            self._terminate_process()

    def _terminate_process(self):
        if self.process and self.process.poll() is None:
            try:
                self.process.terminate()
                self.process.wait(timeout=1)
            except subprocess.TimeoutExpired:
                self.process.kill()
            self.process = None

    def finish(self):
        self._terminate_process()
        super().finish()

    def _handle_get_settings(self):
        settings = {}
        
        if os.path.exists(SECRET_ENV_PATH):
            with open(SECRET_ENV_PATH, "r", encoding="utf-8") as f:
                for line in f:
                    line = line.strip()
                    if not line or "=" not in line:
                        continue
                    key, value = line.split("=", 1)
                    # 排除密钥
                    if key != "MIHOMO_SECRET":
                        settings[key] = value
        
        self._json_response({"success": True, "data": settings})

    def _handle_check_secret(self):
        try:
            length = int(self.headers.get('Content-Length', 0))
            data = json.loads(self.rfile.read(length).decode('utf-8'))
            input_secret = data.get("secret", "")
        except Exception:
            self._json_response({"success": False, "msg": "请求格式错误"}, 400)
            return
        passed = (not MIHOMO_SECRET or input_secret == MIHOMO_SECRET)
        self._json_response({"success": passed, "msg": "" if passed else "密钥错误"})

    def _handle_save_settings(self):
        try:
            data = json.loads(self.rfile.read(int(self.headers['Content-Length'])).decode('utf-8'))
            input_secret = data.get("secret", "")
            if MIHOMO_SECRET and input_secret != MIHOMO_SECRET:
                self._json_response({'success': False, 'msg': '密钥错误'}, 403)
                return
        except Exception:
            self._json_response({'success': False, 'msg': '请求格式错误'}, 400)
            return
        env = open(SECRET_ENV_PATH, 'r', encoding='utf-8').read() if os.path.exists(SECRET_ENV_PATH) else ''
        for k, v in data.items():
            if k == "secret":
                k = "MIHOMO_SECRET"
            old = [l for l in env.splitlines() if l.startswith(f'{k}=')]
            env = env.replace(old[0], f'{k}={v}') if old else env + ('' if env.endswith('\n') or env == '' else '\n') + f'{k}={v}\n'
        open(SECRET_ENV_PATH, 'w', encoding='utf-8').write(env)
        self._json_response({'success': True})

    def _json_response(self, data, code=200):
        self.send_response(code)
        self.send_header("Content-type", "application/json; charset=utf-8")
        self.end_headers()
        self.wfile.write(json.dumps(data).encode('utf-8'))

    def _not_found(self):
        self.send_error(404, "Not Found")

if __name__ == "__main__":
    # 设置允许地址重用
    socketserver.TCPServer.allow_reuse_address = True
    with socketserver.ThreadingTCPServer(("", PORT), CustomHandler) as httpd:
        print(f"服务已启动，端口：{PORT}")
        httpd.serve_forever()
