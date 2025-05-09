import unittest
from unittest.mock import patch, mock_open, MagicMock
import json
import io
from main import CustomHandler, SECRET_ENV_PATH

class TestCustomHandler(unittest.TestCase):
    def setUp(self):
        pass

    @patch("builtins.open", new_callable=mock_open, read_data="MIHOMO_SECRET=123\n")
    @patch("os.path.exists", return_value=True)
    def test_handle_save_settings_update(self, mock_exists, mock_file):
        data = {"MIHOMO_SECRET": "abc"}
        json_data = json.dumps(data).encode("utf-8")
        # 用__new__跳过父类__init__
        handler = CustomHandler.__new__(CustomHandler)
        handler.rfile = io.BytesIO(json_data)
        handler.headers = {"Content-Length": str(len(json_data))}
        handler.wfile = io.BytesIO()
        handler._json_response = MagicMock()
        handler._handle_save_settings()
        mock_file().write.assert_called()
        handler._json_response.assert_called_with({'success': True})

    @patch("builtins.open", new_callable=mock_open, read_data="")
    @patch("os.path.exists", return_value=False)
    def test_handle_save_settings_create(self, mock_exists, mock_file):
        data = {"MIHOMO_SECRET": "newsecret"}
        json_data = json.dumps(data).encode("utf-8")
        handler = CustomHandler.__new__(CustomHandler)
        handler.rfile = io.BytesIO(json_data)
        handler.headers = {"Content-Length": str(len(json_data))}
        handler.wfile = io.BytesIO()
        handler._json_response = MagicMock()
        handler._handle_save_settings()
        mock_file().write.assert_called()
        handler._json_response.assert_called_with({'success': True})

if __name__ == "__main__":
    unittest.main() 