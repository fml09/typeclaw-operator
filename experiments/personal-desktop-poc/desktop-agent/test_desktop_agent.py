"""Tests for desktop-agent.py using fake tool executables (no X11 required)."""

import http.client
import importlib.util
import json
import os
import sys
import tempfile
import threading
import time
import unittest

_AGENT_PATH = os.path.join(os.path.dirname(os.path.abspath(__file__)), "desktop-agent.py")
_AGENT_SPEC = importlib.util.spec_from_file_location("desktop_agent", _AGENT_PATH)
desktop_agent = importlib.util.module_from_spec(_AGENT_SPEC)
sys.modules["desktop_agent"] = desktop_agent
_AGENT_SPEC.loader.exec_module(desktop_agent)

from desktop_agent import AgentError, AgentHTTPServer, DesktopAgent

AGENT_TOKEN = "unit-test-token-0123456789abcdef"
FAKE_PNG = b"\x89PNG\r\n\x1a\nfake-png-bytes"

FAKE_XDOTOOL = r"""#!/bin/sh
printf '%s\n' "$*" >> "$AGENT_ARGV_LOG"
if [ "$1" = "getdisplaygeometry" ]; then
  echo "${FAKE_GEOMETRY:-1280 800}"
  exit 0
fi
case "$*" in
  *"$FAKE_XDOTOOL_FAIL_MATCH"*)
    if [ -n "$FAKE_XDOTOOL_FAIL_MATCH" ]; then
      echo "simulated matched xdotool failure" >&2
      exit 4
    fi
    ;;
esac
if [ -n "$FAKE_XDOTOOL_FAIL" ]; then
  echo "simulated xdotool failure" >&2
  exit 3
fi
exit 0
"""

FAKE_SCROT = r"""#!/bin/sh
printf '%s\n' "$*" >> "$AGENT_ARGV_LOG"
cat "$FAKE_PNG_SOURCE" > "$2"
exit 0
"""

FAKE_WMCTRL = r"""#!/bin/sh
printf '%s\n' "$*" >> "$AGENT_ARGV_LOG"
cat "$AGENT_WMCTRL_OUTPUT"
exit 0
"""

FAKE_APP = r"""#!/bin/sh
printf '%s launched\n' "$0" >> "$AGENT_ARGV_LOG"
exit 0
"""

WMCTRL_OUTPUT = (
    "0x03c00002  0 myhost Firefox\n"
    "0x03c00003  1 myhost GNOME Terminal\n"
    "0x03c00004  2 myhost Text Editor\n"
    "malformed-line\n"
    "0x03c00005  notanint myhost Broken\n"
)


class FakeToolsTestCase(unittest.TestCase):
    """Installs fake xdotool/scrot/wmctrl scripts and wires env overrides."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.bin_dir = os.path.join(self.tmp.name, "bin")
        os.mkdir(self.bin_dir)
        self.argv_log = os.path.join(self.tmp.name, "argv.log")

        png_source = os.path.join(self.tmp.name, "fake.png")
        with open(png_source, "wb") as fh:
            fh.write(FAKE_PNG)
        windows_source = os.path.join(self.tmp.name, "windows.txt")
        with open(windows_source, "w", encoding="utf-8") as fh:
            fh.write(WMCTRL_OUTPUT)

        self.xdotool = self._write_tool("xdotool", FAKE_XDOTOOL)
        self.scrot = self._write_tool("scrot", FAKE_SCROT)
        self.wmctrl = self._write_tool("wmctrl", FAKE_WMCTRL)
        for app in ("firefox", "xfce4-terminal", "thunar"):
            self._write_tool(app, FAKE_APP)

        self._setenv("AGENT_ARGV_LOG", self.argv_log)
        self._setenv("FAKE_PNG_SOURCE", png_source)
        self._setenv("AGENT_WMCTRL_OUTPUT", windows_source)
        self._setenv("FAKE_GEOMETRY", "1280 800")
        self._prepend_path(self.bin_dir)

        self.agent = DesktopAgent(
            token=AGENT_TOKEN,
            xdotool=self.xdotool,
            scrot=self.scrot,
            wmctrl=self.wmctrl,
            type_delay_ms=6,
        )

    def _write_tool(self, name, body):
        path = os.path.join(self.bin_dir, name)
        with open(path, "w", encoding="utf-8") as fh:
            fh.write(body)
        os.chmod(path, 0o755)
        return path

    def _setenv(self, name, value):
        original = os.environ.get(name)
        os.environ[name] = value

        def restore():
            if original is None:
                os.environ.pop(name, None)
            else:
                os.environ[name] = original
        self.addCleanup(restore)

    def _prepend_path(self, directory):
        original = os.environ.get("PATH")
        os.environ["PATH"] = directory + os.pathsep + (original or "")

        def restore():
            if original is None:
                os.environ.pop("PATH", None)
            else:
                os.environ["PATH"] = original
        self.addCleanup(restore)

    def wait_for_log(self, substring, timeout=5.0):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if os.path.exists(self.argv_log) and substring in self.argv_log_text():
                return self.argv_log_text()
            time.sleep(0.05)
        self.fail(f"{substring!r} did not appear in argv log within {timeout}s")

    def argv_log_text(self):
        with open(self.argv_log, "r", encoding="utf-8") as fh:
            return fh.read()


class DesktopAgentActionTests(FakeToolsTestCase):
    def test_health_reports_screen(self):
        self.assertEqual(
            self.agent.health(),
            {"ok": True, "agent": "desktop-agent/1.0",
             "screen": {"width": 1280, "height": 800}},
        )

    def test_click_maps_button_and_echoes_request(self):
        result = self.agent.click({"x": 10, "y": 20, "button": "left", "clicks": 2})
        self.assertEqual(result, {"applied": True, "x": 10, "y": 20, "button": "left",
                                  "clicks": 2})
        log = self.argv_log_text()
        self.assertIn("mousemove --sync 10 20", log)
        self.assertIn("click --repeat 2 --delay 100 1", log)

        self.agent.click({"x": 5, "y": 6, "button": "right"})
        self.assertIn("click --repeat 1 --delay 100 3", self.argv_log_text())

    def test_click_rejects_bad_fields(self):
        for payload in (
            {"x": -1, "y": 0, "button": "left"},
            {"x": 0, "y": 0, "button": "side"},
            {"x": 0, "y": 0, "button": "left", "clicks": 3},
            {"y": 0, "button": "left"},
            {"x": 0, "y": True, "button": "left"},
        ):
            with self.assertRaises(AgentError) as ctx:
                self.agent.click(payload)
            self.assertEqual(ctx.exception.status, 400)

    def test_click_rejects_coordinates_beyond_screen(self):
        for x, y in ((1280, 0), (0, 800), (5000, 5000)):
            with self.assertRaises(AgentError) as ctx:
                self.agent.click({"x": x, "y": y, "button": "left"})
            self.assertEqual(ctx.exception.status, 400)
            self.assertIn("exceeds screen 1280x800", ctx.exception.message)

    def test_type_sends_text_verbatim_after_double_dash(self):
        text = "-hello\nworld"
        result = self.agent.type_text({"text": text})
        self.assertEqual(result, {"applied": True, "characters": 12})
        self.assertIn("type --delay 6 -- -hello\nworld", self.argv_log_text())

    def test_type_rejects_long_and_non_string_text(self):
        with self.assertRaises(AgentError) as ctx:
            self.agent.type_text({"text": "x" * 4001})
        self.assertEqual(ctx.exception.status, 400)
        with self.assertRaises(AgentError) as ctx:
            self.agent.type_text({"text": 123})
        self.assertEqual(ctx.exception.status, 400)

    def test_key_normalization(self):
        cases = {
            "Control+a": "ctrl+a",
            "cmd+pageup": "super+Prior",
            "shift+option+left": "shift+alt+Left",
            "win+Return": "super+Return",
            "esc": "Escape",
            "F4": "F4",
            "space": "space",
        }
        for spec, expected in cases.items():
            result = self.agent.press_key({"key": spec})
            self.assertEqual(result, {"applied": True, "key": expected})
            self.assertIn(f"key -- {expected}", self.argv_log_text())

    def test_key_rejects_invalid_specs(self):
        for spec in ("Control+a;rm -rf /", "a" * 81, "ctrl++a", "", None, 42):
            with self.assertRaises(AgentError) as ctx:
                self.agent.press_key({"key": spec})
            self.assertEqual(ctx.exception.status, 400)

    def test_scroll_vertical_wheel_direction_and_steps(self):
        self.assertEqual(self.agent.scroll({"x": 3, "y": 4, "deltaX": 0, "deltaY": -360}),
                         {"applied": True})
        log = self.argv_log_text()
        self.assertIn("mousemove --sync 3 4", log)
        self.assertEqual(log.count("click 4"), 3)
        self.assertNotIn("click 5", log)

    def test_scroll_minimum_one_step(self):
        self.agent.scroll({"x": 0, "y": 0, "deltaX": 0, "deltaY": 1})
        self.assertEqual(self.argv_log_text().count("click 5"), 1)

    def test_scroll_horizontal_wheel(self):
        self.agent.scroll({"x": 0, "y": 0, "deltaX": 500, "deltaY": 0})
        log = self.argv_log_text()
        self.assertEqual(log.count("click 7"), 4)
        self.assertNotIn("click 6", log)

        self.agent.scroll({"x": 0, "y": 0, "deltaX": -1000, "deltaY": 0})
        self.assertEqual(self.argv_log_text().count("click 6"), 8)

    def test_scroll_vertical_then_horizontal(self):
        self.agent.scroll({"x": 0, "y": 0, "deltaX": 130, "deltaY": 130})
        log = self.argv_log_text()
        self.assertEqual(log.count("click 5"), 1)
        self.assertEqual(log.count("click 7"), 1)
        self.assertLess(log.index("click 5"), log.index("click 7"))

    def test_scroll_rejects_out_of_bounds_before_any_motion(self):
        with self.assertRaises(AgentError) as ctx:
            self.agent.scroll({"x": 9999, "y": 0, "deltaX": 0, "deltaY": 100})
        self.assertEqual(ctx.exception.status, 400)
        self.assertNotIn("mousemove", self.argv_log_text())

    def test_scroll_rejects_bad_deltas(self):
        for payload in (
            {"x": 0, "y": 0, "deltaX": 4001, "deltaY": 0},
            {"x": 0, "y": 0, "deltaX": 0, "deltaY": "fast"},
            {"x": 0, "y": 0, "deltaY": 10},
        ):
            with self.assertRaises(AgentError) as ctx:
                self.agent.scroll(payload)
            self.assertEqual(ctx.exception.status, 400)

    def test_launch_builtin_apps(self):
        for app, command in (("firefox", ["firefox"]), ("terminal", ["xfce4-terminal"]),
                             ("files", ["thunar"])):
            self.assertEqual(self.agent.launch({"app": app}),
                             {"applied": True, "command": command})
        self.wait_for_log("xfce4-terminal launched")

    def test_launch_plain_app_found_on_path(self):
        self._write_tool("my-tool", FAKE_APP)
        self.assertEqual(self.agent.launch({"app": "my-tool"}),
                         {"applied": True, "command": ["my-tool"]})

    def test_launch_unknown_app_rejected_when_which_misses(self):
        with self.assertRaises(AgentError) as ctx:
            self.agent.launch({"app": "definitely-not-installed-tool-xyz"})
        self.assertEqual(ctx.exception.status, 400)
        with self.assertRaises(AgentError) as ctx:
            self.agent.launch({"app": "Bad App"})
        self.assertEqual(ctx.exception.status, 400)

    def test_windows_parses_wmctrl_output(self):
        self.assertEqual(
            self.agent.windows(),
            {"windows": [
                {"id": "0x03c00002", "desktop": 0, "title": "Firefox"},
                {"id": "0x03c00003", "desktop": 1, "title": "GNOME Terminal"},
                {"id": "0x03c00004", "desktop": 2, "title": "Text Editor"},
            ]},
        )
        self.assertIn("-l", self.argv_log_text())

    def test_action_batch_validates_then_executes_in_order(self):
        actions = [
            {"type": "click", "x": 10, "y": 20, "button": "left", "clicks": 1},
            {"type": "type", "text": "hello"},
            {"type": "key", "key": "Enter"},
        ]
        self.assertEqual(
            self.agent.action_batch({"actions": actions}),
            {"applied": True, "outcome": "Applied",
             "actionCount": 3, "completedActions": 3},
        )
        log = self.argv_log_text()
        self.assertLess(log.index("mousemove --sync 10 20"), log.index("type --delay 6 -- hello"))
        self.assertLess(log.index("type --delay 6 -- hello"), log.index("key -- Return"))

    def test_action_batch_rejects_every_action_before_input(self):
        with self.assertRaises(AgentError) as ctx:
            self.agent.action_batch({"actions": [
                {"type": "click", "x": 10, "y": 20, "button": "left"},
                {"type": "key", "key": "Control+a;rm"},
            ]})
        self.assertEqual(ctx.exception.status, 400)
        log = self.argv_log_text()
        self.assertNotIn("mousemove", log)
        self.assertNotIn("key --", log)

    def test_action_batch_bounds_length(self):
        with self.assertRaises(AgentError) as ctx:
            self.agent.action_batch({"actions": []})
        self.assertEqual(ctx.exception.status, 400)
        with self.assertRaises(AgentError) as ctx:
            self.agent.action_batch({
                "actions": [{"type": "key", "key": "Tab"}]
                * (desktop_agent.MAX_ACTIONS_PER_BATCH + 1),
            })
        self.assertEqual(ctx.exception.status, 400)

        with self.assertRaises(AgentError) as ctx:
            self.agent.action_batch({"actions": [
                {"type": "type", "text": "x" * 3000},
                {"type": "type", "text": "y" * 2000},
            ]})
        self.assertEqual(ctx.exception.status, 400)

    def test_action_batch_reports_partial_failure_without_retry_permission(self):
        self._setenv("FAKE_XDOTOOL_FAIL_MATCH", "key -- Return")
        result = self.agent.action_batch({"actions": [
            {"type": "type", "text": "hello"},
            {"type": "key", "key": "Enter"},
        ]})
        self.assertEqual(result["applied"], False)
        self.assertEqual(result["outcome"], "Partial")
        self.assertEqual(result["actionCount"], 2)
        self.assertEqual(result["completedActions"], 1)
        self.assertEqual(result["failedActionIndex"], 1)
        self.assertEqual(result["retrySafe"], False)
        self.assertIn("simulated matched xdotool failure", result["error"])

    def test_screenshot_returns_png_and_cleans_temp_file(self):
        width, height, data = self.agent.screenshot()
        self.assertEqual((width, height), (1280, 800))
        self.assertEqual(data, FAKE_PNG)
        log = self.wait_for_log("-z ")
        scrot_lines = [line for line in log.splitlines() if line.startswith("-z ")]
        self.assertEqual(len(scrot_lines), 1)
        tmp_path = scrot_lines[0].split()[-1]
        self.assertTrue(tmp_path.startswith(tempfile.gettempdir()))
        self.assertFalse(os.path.exists(tmp_path))

    def test_tool_failure_maps_to_500_with_stderr(self):
        self._setenv("FAKE_XDOTOOL_FAIL", "1")
        with self.assertRaises(AgentError) as ctx:
            self.agent.click({"x": 1, "y": 2, "button": "left"})
        self.assertEqual(ctx.exception.status, 500)
        self.assertIn("simulated xdotool failure", ctx.exception.message)


class TokenLoadingTests(unittest.TestCase):
    def test_load_token_reads_first_line_stripped(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "token")
            with open(path, "w", encoding="utf-8") as fh:
                fh.write("  abcdefghijklmnopqrstuvwxyz012345  \nsecond\n")
            self.assertEqual(desktop_agent.load_token(path),
                             "abcdefghijklmnopqrstuvwxyz012345")

    def test_load_token_rejects_short_token(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "token")
            with open(path, "w", encoding="utf-8") as fh:
                fh.write("short\n")
            with self.assertRaises(SystemExit) as ctx:
                desktop_agent.load_token(path)
            self.assertIn("24", str(ctx.exception))

    def test_load_token_missing_file_exits(self):
        with self.assertRaises(SystemExit):
            desktop_agent.load_token("/nonexistent/agent-token-path")


class DesktopAgentHTTPTests(FakeToolsTestCase):
    def setUp(self):
        super().setUp()
        self.server = AgentHTTPServer(("127.0.0.1", 0), self.agent)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.addCleanup(self.thread.join)
        self.addCleanup(self.server.server_close)
        self.addCleanup(self.server.shutdown)
        self.port = self.server.server_address[1]

    @staticmethod
    def _headers(token):
        if token is None:
            return {}
        return {"Authorization": "Bearer " + token}

    def _request(self, method, path, token=AGENT_TOKEN, payload=None, raw_body=None,
                 content_length="auto"):
        conn = http.client.HTTPConnection("127.0.0.1", self.port, timeout=10)
        self.addCleanup(conn.close)
        if raw_body is None and payload is not None:
            raw_body = json.dumps(payload).encode("utf-8")
        if content_length == "auto":
            conn.request(method, path, body=raw_body or b"", headers=self._headers(token))
        else:
            conn.putrequest(method, path, skip_accept_encoding=True)
            for name, value in self._headers(token).items():
                conn.putheader(name, value)
            if content_length is not None:
                conn.putheader("Content-Length", str(content_length))
            conn.endheaders()
            if raw_body:
                conn.send(raw_body)
        resp = conn.getresponse()
        body = resp.read()
        return resp.status, dict(resp.getheaders()), body

    def test_health_requires_bearer_auth(self):
        status, _, body = self._request("GET", "/health", token=None)
        self.assertEqual(status, 401)
        self.assertIn("error", json.loads(body))
        status, _, _ = self._request("GET", "/health", token="x" * 32)
        self.assertEqual(status, 401)

    def test_health_ok(self):
        status, _, body = self._request("GET", "/health")
        self.assertEqual(status, 200)
        self.assertEqual(
            json.loads(body),
            {"ok": True, "agent": "desktop-agent/1.0",
             "screen": {"width": 1280, "height": 800}},
        )

    def test_actions_roundtrip_and_auth_rejection(self):
        payload = {"actions": [
            {"type": "click", "x": 15, "y": 25, "button": "middle", "clicks": 1},
            {"type": "type", "text": "hello"},
        ]}
        status, _, body = self._request("POST", "/actions", payload=payload)
        self.assertEqual(status, 200)
        self.assertEqual(
            json.loads(body),
            {"applied": True, "outcome": "Applied",
             "actionCount": 2, "completedActions": 2},
        )
        status, _, body = self._request(
            "POST", "/actions", token="not-the-token-not-the-token", payload=payload,
        )
        self.assertEqual(status, 401)
        self.assertIn("error", json.loads(body))

    def test_screenshot_png_with_screen_headers(self):
        status, headers, body = self._request("POST", "/screenshot", payload={})
        self.assertEqual(status, 200)
        self.assertEqual(headers["X-Screen-Width"], "1280")
        self.assertEqual(headers["X-Screen-Height"], "800")
        self.assertEqual(headers["Content-Type"], "image/png")
        self.assertEqual(body, FAKE_PNG)

    def test_windows_via_http(self):
        status, _, body = self._request("GET", "/windows")
        self.assertEqual(status, 200)
        self.assertEqual(len(json.loads(body)["windows"]), 3)

    def test_unknown_path_404(self):
        status, _, body = self._request("GET", "/nope")
        self.assertEqual(status, 404)
        self.assertIn("error", json.loads(body))
        status, _, _ = self._request("POST", "/nope", payload={})
        self.assertEqual(status, 404)

    def test_wrong_method_405(self):
        status, _, body = self._request("GET", "/actions")
        self.assertEqual(status, 405)
        self.assertIn("error", json.loads(body))
        status, _, _ = self._request("POST", "/windows", payload={})
        self.assertEqual(status, 405)

    def test_malformed_json_400(self):
        status, _, body = self._request("POST", "/actions", raw_body=b"{not json")
        self.assertEqual(status, 400)
        self.assertIn("error", json.loads(body))

    def test_non_object_json_400(self):
        status, _, body = self._request("POST", "/actions", raw_body=b"[1,2]")
        self.assertEqual(status, 400)
        self.assertIn("error", json.loads(body))

    def test_missing_content_length_413(self):
        status, _, body = self._request("POST", "/actions", raw_body=b"{}", content_length=None)
        self.assertEqual(status, 413)
        self.assertIn("error", json.loads(body))

    def test_oversized_body_413(self):
        status, _, body = self._request("POST", "/actions", raw_body=b"x",
                                        content_length=desktop_agent.MAX_BODY_BYTES + 1)
        self.assertEqual(status, 413)
        self.assertIn("error", json.loads(body))

    def test_validation_error_via_http(self):
        status, _, body = self._request(
            "POST",
            "/actions",
            payload={"actions": [{"type": "click", "x": -5, "y": 0, "button": "left"}]},
        )
        self.assertEqual(status, 400)
        self.assertIn("error", json.loads(body))


if __name__ == "__main__":
    unittest.main()
