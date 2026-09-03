"""Hermetic tests for the Guest Desktop Agent.

Nothing here needs a desktop: the X11 backend runs against fake tool
executables, the Windows backend against an injected Win32 façade, and the
macOS backend against a fake Quartz module. The suite therefore passes on the
same machine that builds the operator image.
"""

import ctypes
import http.client
import json
import os
import struct
import sys
import tempfile
import threading
import time
import unittest
import zlib

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import desktop_agent
from desktop_agent import (AgentError, AgentHTTPServer, BackendUnavailable, DesktopAgent,
                           MacOSBackend, WindowsBackend, X11Backend)

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
        for app in ("firefox", "xfce4-terminal", "thunar", "mousepad"):
            self._write_tool(app, FAKE_APP)

        self._setenv("AGENT_ARGV_LOG", self.argv_log)
        self._setenv("FAKE_PNG_SOURCE", png_source)
        self._setenv("AGENT_WMCTRL_OUTPUT", windows_source)
        self._setenv("FAKE_GEOMETRY", "1280 800")
        self._prepend_path(self.bin_dir)

        self.backend = X11Backend(
            xdotool=self.xdotool,
            scrot=self.scrot,
            wmctrl=self.wmctrl,
            type_delay_ms=6,
        )
        self.agent = DesktopAgent(token=AGENT_TOKEN, backend=self.backend)

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


class X11ActionTests(FakeToolsTestCase):
    def test_health_reports_screen_backend_and_os(self):
        self.assertEqual(
            self.agent.health(),
            {"ok": True, "agent": "desktop-agent/2.0", "backend": "x11",
             "os": desktop_agent.os_name(),
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

    def test_click_rejects_a_float_click_count_before_touching_the_desktop(self):
        # `1.0 in (1, 2)` is True, so a JSON float used to pass validation and
        # reach range(clicks) in the Windows and macOS backends, where it raises
        # TypeError rather than a 400.
        for clicks in (1.0, 2.0, "1"):
            with self.assertRaises(AgentError) as ctx:
                self.agent.click({"x": 1, "y": 2, "button": "left", "clicks": clicks})
            self.assertEqual(ctx.exception.status, 400)
            self.assertEqual(ctx.exception.message, "clicks must be 1 or 2")
        self.assertFalse(os.path.exists(self.argv_log))

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
        for app, command in (("firefox", ["firefox"]), ("browser", ["firefox"]),
                             ("terminal", ["xfce4-terminal"]), ("files", ["thunar"]),
                             ("editor", ["mousepad"])):
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
        with self.assertRaises(AgentError) as ctx:
            self.agent.launch({"app": 7})
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

    def test_action_batch_rejects_unknown_types_and_non_objects(self):
        for actions in ([{"type": "drag", "x": 1, "y": 1}], ["not-an-object"]):
            with self.assertRaises(AgentError) as ctx:
                self.agent.action_batch({"actions": actions})
            self.assertEqual(ctx.exception.status, 400)
        with self.assertRaises(AgentError) as ctx:
            self.agent.action_batch({"actions": "click"})
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
        self.assertEqual(result["failedActionType"], "key")
        self.assertEqual(result["retrySafe"], False)
        self.assertIn("simulated matched xdotool failure", result["error"])

    def test_action_batch_reports_partial_when_the_backend_fails_outside_agent_error(self):
        # The Gateway reads a 5xx without an outcome as "nothing was applied",
        # so a backend fault that is not an AgentError must still say how far
        # the batch got - the text below has already reached the desktop.
        def explode(*_args):
            raise TypeError("'float' object cannot be interpreted as an integer")

        self.agent.backend.click = explode
        result = self.agent.action_batch({"actions": [
            {"type": "type", "text": "hello"},
            {"type": "click", "x": 1, "y": 1, "button": "left"},
        ]})
        self.assertEqual(result["applied"], False)
        self.assertEqual(result["outcome"], "Partial")
        self.assertEqual(result["actionCount"], 2)
        self.assertEqual(result["completedActions"], 1)
        self.assertEqual(result["failedActionIndex"], 1)
        self.assertEqual(result["failedActionType"], "click")
        self.assertEqual(result["retrySafe"], False)
        self.assertEqual(result["error"],
                         "internal error: 'float' object cannot be interpreted as an integer")
        self.assertIn("type --delay 6 -- hello", self.argv_log_text())

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

    def test_unexpected_geometry_output_is_500(self):
        self._setenv("FAKE_GEOMETRY", "not a geometry")
        with self.assertRaises(AgentError) as ctx:
            self.agent.health()
        self.assertEqual(ctx.exception.status, 500)


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

    def test_resolve_token_prefers_environment_variable(self):
        env = {"DESKTOP_AGENT_TOKEN": "  " + AGENT_TOKEN + "  ",
               "DESKTOP_AGENT_TOKEN_FILE": "/nonexistent/agent-token-path"}
        self.assertEqual(desktop_agent.resolve_token(env), AGENT_TOKEN)

    def test_resolve_token_rejects_short_environment_token(self):
        with self.assertRaises(SystemExit) as ctx:
            desktop_agent.resolve_token({"DESKTOP_AGENT_TOKEN": "short"})
        self.assertIn("24", str(ctx.exception))

    def test_resolve_token_falls_back_to_the_token_file(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "token")
            with open(path, "w", encoding="utf-8") as fh:
                fh.write(AGENT_TOKEN + "\n")
            self.assertEqual(desktop_agent.resolve_token({"DESKTOP_AGENT_TOKEN_FILE": path}),
                             AGENT_TOKEN)

    def test_default_token_file_is_platform_dependent(self):
        self.assertEqual(desktop_agent.default_token_file(platform="linux", env={}),
                         "/etc/personal-desktop/agent-token")
        self.assertEqual(desktop_agent.default_token_file(platform="darwin", env={}),
                         "/etc/personal-desktop/agent-token")
        self.assertEqual(
            desktop_agent.default_token_file(platform="win32",
                                             env={"ProgramData": r"C:\ProgramData"}),
            r"C:\ProgramData\PersonalDesktop\agent-token",
        )


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
            {"ok": True, "agent": "desktop-agent/2.0", "backend": "x11",
             "os": desktop_agent.os_name(),
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

    def test_launch_via_http(self):
        status, _, body = self._request("POST", "/launch", payload={"app": "browser"})
        self.assertEqual(status, 200)
        self.assertEqual(json.loads(body), {"applied": True, "command": ["firefox"]})

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


# ----------------------------------------------------------------------
# Windows backend
# ----------------------------------------------------------------------

SCREEN_DC = 0x1001
MEMORY_DC = 0x2002
CAPTURE_BITMAP = 0x3003
PREVIOUS_BITMAP = 0x4004


def _signed32(value):
    return value - 0x100000000 if value >= 0x80000000 else value


def _decode_input(item):
    """Read one INPUT structure the way the Win32 API would."""
    if item.type == desktop_agent.INPUT_MOUSE:
        return {"kind": "mouse", "dx": item.mi.dx, "dy": item.mi.dy,
                "data": _signed32(item.mi.mouseData), "flags": item.mi.dwFlags}
    return {"kind": "key", "vk": item.ki.wVk, "scan": item.ki.wScan,
            "flags": item.ki.dwFlags}


class FakeWin32:
    """Records every Win32 call WindowsBackend makes."""

    def __init__(self, width=1280, height=800, capture=b"", windows=(), foreground=0):
        self.width = width
        self.height = height
        self.capture = capture
        self.windows = list(windows)
        self.foreground = foreground
        self.dpi_calls = 0
        self.calls = []
        self.batches = []
        self.spawned = []

    # input ------------------------------------------------------------

    def set_dpi_awareness(self):
        self.dpi_calls += 1
        return "per-monitor-v2"

    def send_input(self, inputs):
        self.calls.append("send_input")
        self.batches.append([_decode_input(item) for item in inputs])
        return len(inputs)

    def screen_size(self):
        return self.width, self.height

    def vk_key_scan(self, character):
        # US layout: letters answer with their uppercase virtual key, and the
        # high byte carries the shift state the layout needs.
        if character.isalpha():
            return ord(character.upper()) | (0x0100 if character.isupper() else 0)
        if character.isdigit():
            return ord(character)
        if character == "!":
            return ord("1") | 0x0100
        return -1

    # GDI --------------------------------------------------------------

    def get_dc(self, window):
        self.calls.append("get_dc")
        return SCREEN_DC

    def release_dc(self, window, dc):
        self.calls.append(f"release_dc:{dc:#x}")

    def create_compatible_dc(self, dc):
        self.calls.append("create_compatible_dc")
        return MEMORY_DC

    def create_compatible_bitmap(self, dc, width, height):
        self.calls.append(f"create_compatible_bitmap:{width}x{height}")
        return CAPTURE_BITMAP

    def select_object(self, dc, obj):
        self.calls.append(f"select_object:{obj:#x}")
        return PREVIOUS_BITMAP

    def bit_blt(self, dest_dc, x, y, width, height, source_dc, source_x, source_y):
        self.calls.append(f"bit_blt:{width}x{height}")

    def get_di_bits(self, dc, bitmap, width, height):
        self.calls.append("get_di_bits")
        return self.capture

    def delete_object(self, obj):
        self.calls.append(f"delete_object:{obj:#x}")

    def delete_dc(self, dc):
        self.calls.append(f"delete_dc:{dc:#x}")

    # windows ----------------------------------------------------------

    def get_foreground_window(self):
        return self.foreground

    def enum_window_handles(self):
        return [hwnd for hwnd, _visible, _title in self.windows]

    def is_window_visible(self, hwnd):
        return next(visible for handle, visible, _ in self.windows if handle == hwnd)

    def get_window_text(self, hwnd):
        return next(title for handle, _, title in self.windows if handle == hwnd)


class WindowsBackendTests(unittest.TestCase):
    def setUp(self):
        self.api = FakeWin32()
        self.spawned = []
        self.backend = WindowsBackend(api=self.api, type_delay_ms=0,
                                      spawn=self.spawned.append, sleep=lambda _s: None)
        self.agent = DesktopAgent(token=AGENT_TOKEN, backend=self.backend)

    def test_dpi_awareness_is_declared_once_at_startup(self):
        self.assertEqual(self.api.dpi_calls, 1)
        self.assertEqual(self.backend.dpi_awareness, "per-monitor-v2")

    def test_health_reports_the_windows_backend(self):
        self.assertEqual(self.agent.health()["backend"], "windows")
        self.assertEqual(self.agent.health()["screen"], {"width": 1280, "height": 800})

    def test_click_normalizes_absolute_coordinates_over_the_primary_display(self):
        self.backend.click(0, 0, "left", 1)
        move = self.api.batches[-1][0]
        self.assertEqual((move["dx"], move["dy"]), (0, 0))
        self.assertEqual(move["flags"],
                         desktop_agent.MOUSEEVENTF_MOVE | desktop_agent.MOUSEEVENTF_ABSOLUTE)

        self.backend.click(1279, 799, "left", 1)
        move = self.api.batches[-1][0]
        self.assertEqual((move["dx"], move["dy"]), (65535, 65535))

        self.backend.click(640, 400, "left", 1)
        move = self.api.batches[-1][0]
        self.assertEqual(move["dx"], round(640 * 65535 / 1279))
        self.assertEqual(move["dy"], round(400 * 65535 / 799))

    def test_click_emits_one_down_up_pair_per_click(self):
        self.backend.click(10, 20, "right", 2)
        events = self.api.batches[-1]
        self.assertEqual([event["flags"] for event in events[1:]], [
            desktop_agent.MOUSEEVENTF_RIGHTDOWN, desktop_agent.MOUSEEVENTF_RIGHTUP,
            desktop_agent.MOUSEEVENTF_RIGHTDOWN, desktop_agent.MOUSEEVENTF_RIGHTUP,
        ])

        self.backend.click(10, 20, "middle", 1)
        self.assertEqual([event["flags"] for event in self.api.batches[-1][1:]], [
            desktop_agent.MOUSEEVENTF_MIDDLEDOWN, desktop_agent.MOUSEEVENTF_MIDDLEUP,
        ])

    def test_type_text_sends_one_unicode_pair_per_utf16_code_unit(self):
        self.backend.type_text("hi")
        self.assertEqual(self.api.batches, [
            [{"kind": "key", "vk": 0, "scan": ord("h"),
              "flags": desktop_agent.KEYEVENTF_UNICODE},
             {"kind": "key", "vk": 0, "scan": ord("h"),
              "flags": desktop_agent.KEYEVENTF_UNICODE | desktop_agent.KEYEVENTF_KEYUP}],
            [{"kind": "key", "vk": 0, "scan": ord("i"),
              "flags": desktop_agent.KEYEVENTF_UNICODE},
             {"kind": "key", "vk": 0, "scan": ord("i"),
              "flags": desktop_agent.KEYEVENTF_UNICODE | desktop_agent.KEYEVENTF_KEYUP}],
        ])

    def test_type_text_splits_astral_characters_into_surrogate_pairs(self):
        self.backend.type_text("\U0001F600")
        events = self.api.batches[-1]
        self.assertEqual([event["scan"] for event in events], [0xD83D, 0xD83D, 0xDE00, 0xDE00])
        self.assertEqual([event["flags"] for event in events], [
            desktop_agent.KEYEVENTF_UNICODE,
            desktop_agent.KEYEVENTF_UNICODE | desktop_agent.KEYEVENTF_KEYUP,
            desktop_agent.KEYEVENTF_UNICODE,
            desktop_agent.KEYEVENTF_UNICODE | desktop_agent.KEYEVENTF_KEYUP,
        ])

    def test_type_text_sends_control_characters_as_keystrokes(self):
        self.backend.type_text("a\r\n\tb")
        virtual_keys = [event["vk"] for batch in self.api.batches for event in batch
                        if event["vk"]]
        self.assertEqual(virtual_keys, [0x0D, 0x0D, 0x09, 0x09])

    def test_press_key_holds_modifiers_around_the_key(self):
        action = DesktopAgent._prepare_key({"key": "Control+a"})
        self.assertEqual(self.backend.press_key(action["modifiers"], action["key"]), "ctrl+a")
        self.assertEqual([(event["vk"], event["flags"]) for event in self.api.batches[-1]], [
            (0x11, 0),
            (0x41, 0),
            (0x41, desktop_agent.KEYEVENTF_KEYUP),
            (0x11, desktop_agent.KEYEVENTF_KEYUP),
        ])

    def test_press_key_marks_the_navigation_cluster_as_extended(self):
        action = DesktopAgent._prepare_key({"key": "cmd+pageup"})
        self.backend.press_key(action["modifiers"], action["key"])
        extended = desktop_agent.KEYEVENTF_EXTENDEDKEY
        self.assertEqual([(event["vk"], event["flags"]) for event in self.api.batches[-1]], [
            (0x5B, extended),
            (0x21, extended),
            (0x21, extended | desktop_agent.KEYEVENTF_KEYUP),
            (0x5B, extended | desktop_agent.KEYEVENTF_KEYUP),
        ])

    def test_press_key_adds_the_shift_the_layout_requires(self):
        self.backend.press_key([], "A")
        self.assertEqual([(event["vk"], event["flags"]) for event in self.api.batches[-1]], [
            (0x10, 0),
            (0x41, 0),
            (0x41, desktop_agent.KEYEVENTF_KEYUP),
            (0x10, desktop_agent.KEYEVENTF_KEYUP),
        ])

    def test_press_key_rejects_keys_and_modifiers_the_backend_cannot_map(self):
        with self.assertRaises(AgentError) as ctx:
            self.backend.press_key([], "hyper")
        self.assertEqual(ctx.exception.status, 500)
        with self.assertRaises(AgentError) as ctx:
            self.backend.press_key(["hyper"], "a")
        self.assertEqual(ctx.exception.status, 500)

    def test_press_function_keys(self):
        self.backend.press_key([], "F4")
        self.assertEqual(self.api.batches[-1][0]["vk"], 0x73)

    def test_scroll_sends_one_wheel_delta_per_step(self):
        self.agent.scroll({"x": 3, "y": 4, "deltaX": 0, "deltaY": -360})
        wheel = [batch[0] for batch in self.api.batches[1:]]
        self.assertEqual(len(wheel), 3)
        for event in wheel:
            self.assertEqual(event["flags"], desktop_agent.MOUSEEVENTF_WHEEL)
            self.assertEqual(event["data"], desktop_agent.WHEEL_DELTA)

    def test_scroll_down_and_horizontal_directions(self):
        self.agent.scroll({"x": 0, "y": 0, "deltaX": 500, "deltaY": 130})
        events = [batch[0] for batch in self.api.batches[1:]]
        self.assertEqual(events[0]["flags"], desktop_agent.MOUSEEVENTF_WHEEL)
        self.assertEqual(events[0]["data"], -desktop_agent.WHEEL_DELTA)
        self.assertEqual([event["flags"] for event in events[1:]],
                         [desktop_agent.MOUSEEVENTF_HWHEEL] * 4)
        self.assertEqual([event["data"] for event in events[1:]],
                         [desktop_agent.WHEEL_DELTA] * 4)

    def test_screenshot_encodes_gdi_pixels_and_releases_every_handle(self):
        self.api.width, self.api.height = 2, 2
        # GDI hands back BGRX scanlines; the encoder must publish them as RGB.
        self.api.capture = bytes([
            0x01, 0x02, 0x03, 0x00, 0x04, 0x05, 0x06, 0x00,
            0x07, 0x08, 0x09, 0x00, 0x0A, 0x0B, 0x0C, 0x00,
        ])
        width, height, png = self.backend.screenshot()
        self.assertEqual((width, height), (2, 2))
        self.assertEqual(_decode_png(png), (2, 2, bytes([
            0x03, 0x02, 0x01, 0x06, 0x05, 0x04,
            0x09, 0x08, 0x07, 0x0C, 0x0B, 0x0A,
        ])))
        self.assertEqual(self.api.calls, [
            "get_dc",
            "create_compatible_dc",
            "create_compatible_bitmap:2x2",
            f"select_object:{CAPTURE_BITMAP:#x}",
            "bit_blt:2x2",
            "get_di_bits",
            f"select_object:{PREVIOUS_BITMAP:#x}",
            f"delete_object:{CAPTURE_BITMAP:#x}",
            f"delete_dc:{MEMORY_DC:#x}",
            f"release_dc:{SCREEN_DC:#x}",
        ])

    def test_screenshot_releases_handles_when_the_capture_fails(self):
        self.api.width, self.api.height = 2, 2
        self.api.capture = b"too short"
        with self.assertRaises(AgentError):
            self.backend.screenshot()
        self.assertIn(f"delete_object:{CAPTURE_BITMAP:#x}", self.api.calls)
        self.assertIn(f"delete_dc:{MEMORY_DC:#x}", self.api.calls)
        self.assertIn(f"release_dc:{SCREEN_DC:#x}", self.api.calls)

    def test_list_windows_skips_hidden_and_untitled_windows(self):
        self.api.windows = [
            (0x00010001, True, "Notepad"),
            (0x00010002, False, "Hidden"),
            (0x00010003, True, ""),
            (0x00010004, True, "Microsoft Edge"),
        ]
        self.api.foreground = 0x00010004
        self.assertEqual(self.agent.windows(), {"windows": [
            {"id": "0x00010004", "desktop": 0, "title": "Microsoft Edge"},
            {"id": "0x00010001", "desktop": 0, "title": "Notepad"},
        ]})

    def test_launch_uses_windows_aliases_and_detached_spawn(self):
        self.assertEqual(self.agent.launch({"app": "files"}),
                         {"applied": True, "command": ["explorer.exe"]})
        self.assertEqual(self.agent.launch({"app": "editor"}),
                         {"applied": True, "command": ["notepad.exe"]})
        self.assertEqual(self.agent.launch({"app": "browser"})["command"],
                         ["cmd", "/c", "start", "", "msedge"])
        self.assertEqual(self.agent.launch({"app": "firefox"})["command"],
                         ["cmd", "/c", "start", "", "firefox"])
        self.assertEqual(self.spawned[0], ["explorer.exe"])
        with self.assertRaises(AgentError) as ctx:
            self.agent.launch({"app": "definitely-not-installed-tool-xyz"})
        self.assertEqual(ctx.exception.status, 400)


# ----------------------------------------------------------------------
# macOS backend
class Win32StructureLayoutTests(unittest.TestCase):
    """Pin the ctypes layout the real Win32API marshals.

    FakeWin32 replaces the whole façade, so no other test in this suite ever
    sees these structures. A wrong INPUT size makes SendInput deliver zero
    events and a positive biHeight mirrors every frame vertically; both would
    otherwise leave the suite green.
    """

    @unittest.skipUnless(ctypes.sizeof(ctypes.c_void_p) == 8,
                         "the Windows guest is amd64; ULONG_PTR is 4 bytes on a 32-bit host")
    def test_send_input_structures_match_the_win32_abi(self):
        self.assertEqual(ctypes.sizeof(desktop_agent.MOUSEINPUT), 32)
        self.assertEqual(ctypes.sizeof(desktop_agent.KEYBDINPUT), 24)
        self.assertEqual(ctypes.sizeof(desktop_agent.INPUT), 40)
        # The union follows the 4-byte type plus 4 bytes of alignment padding.
        self.assertEqual(desktop_agent.INPUT.u.offset, 8)

    def test_bitmap_info_header_matches_the_gdi_abi(self):
        self.assertEqual(ctypes.sizeof(desktop_agent.BITMAPINFOHEADER), 40)
        self.assertEqual(desktop_agent.BITMAPINFOHEADER.biHeight.offset, 8)

    def test_dib_header_requests_a_top_down_32_bit_frame(self):
        header = desktop_agent.dib_header(1280, 800).bmiHeader
        self.assertEqual(header.biSize, ctypes.sizeof(desktop_agent.BITMAPINFOHEADER))
        self.assertEqual(header.biWidth, 1280)
        # Negative: a positive height selects GDI's bottom-up order and every
        # screenshot would reach the model upside down.
        self.assertEqual(header.biHeight, -800)
        self.assertEqual(header.biPlanes, 1)
        self.assertEqual(header.biBitCount, 32)
        self.assertEqual(header.biCompression, desktop_agent.BI_RGB)


# ----------------------------------------------------------------------


class FakeEvent:
    def __init__(self, kind, **fields):
        self.kind = kind
        self.fields = dict(fields)


class FakeQuartz:
    """Stand-in for pyobjc's Quartz module."""

    kCGHIDEventTap = "hid"
    kCGEventMouseMoved = "mouse-moved"
    kCGEventLeftMouseDown = "left-down"
    kCGEventLeftMouseUp = "left-up"
    kCGEventRightMouseDown = "right-down"
    kCGEventRightMouseUp = "right-up"
    kCGEventOtherMouseDown = "other-down"
    kCGEventOtherMouseUp = "other-up"
    kCGMouseButtonLeft = 0
    kCGMouseButtonRight = 1
    kCGMouseButtonCenter = 2
    kCGMouseEventClickState = "click-state"
    kCGEventFlagMaskShift = 0x20000
    kCGEventFlagMaskControl = 0x40000
    kCGEventFlagMaskAlternate = 0x80000
    kCGEventFlagMaskCommand = 0x100000
    kCGScrollEventUnitPixel = 0
    kCGWindowListOptionOnScreenOnly = 0x1
    kCGWindowListExcludeDesktopElements = 0x10
    kCGNullWindowID = 0

    def __init__(self, width=1440, height=900, windows=(), pixel_width=None,
                 pixel_height=None):
        self.width = width
        self.height = height
        # A non-HiDPI display backs each point with one pixel; a Retina display
        # keeps the same point size over a larger backing store, and that store
        # is what screencapture writes.
        self.pixel_width = width if pixel_width is None else pixel_width
        self.pixel_height = height if pixel_height is None else pixel_height
        self.window_info = list(windows)
        self.posted = []
        self.list_options = None
        self.display_mode = "main-display-mode"

    def CGMainDisplayID(self):
        return 7

    def CGDisplayCopyDisplayMode(self, display):
        assert display == 7
        return self.display_mode

    def CGDisplayModeGetWidth(self, mode):
        assert mode == self.display_mode
        return self.width

    def CGDisplayModeGetHeight(self, mode):
        assert mode == self.display_mode
        return self.height

    def CGDisplayModeGetPixelWidth(self, mode):
        assert mode == self.display_mode
        return self.pixel_width

    def CGDisplayModeGetPixelHeight(self, mode):
        assert mode == self.display_mode
        return self.pixel_height

    def CGDisplayPixelsWide(self, display):
        assert display == 7
        return self.width

    def CGDisplayPixelsHigh(self, display):
        assert display == 7
        return self.height

    def CGEventCreateMouseEvent(self, source, event_type, position, button):
        return FakeEvent("mouse", type=event_type, position=position, button=button)

    def CGEventCreateKeyboardEvent(self, source, key_code, down):
        return FakeEvent("key", key_code=key_code, down=down, flags=0, text=None)

    def CGEventCreateScrollWheelEvent(self, source, unit, count, *deltas):
        return FakeEvent("scroll", unit=unit, count=count, deltas=tuple(deltas))

    def CGEventSetIntegerValueField(self, event, field, value):
        event.fields[field] = value

    def CGEventKeyboardSetUnicodeString(self, event, length, text):
        event.fields["text"] = text
        event.fields["length"] = length

    def CGEventSetFlags(self, event, flags):
        event.fields["flags"] = flags

    def CGEventPost(self, tap, event):
        self.posted.append((tap, event))

    def CGWindowListCopyWindowInfo(self, options, relative_to):
        self.list_options = options
        return self.window_info


class LegacyFakeQuartz(FakeQuartz):
    """A pyobjc build that predates the CGDisplayMode pixel accessors."""

    _HIDDEN = ("CGDisplayCopyDisplayMode", "CGDisplayModeGetPixelWidth",
               "CGDisplayModeGetPixelHeight")

    def __getattribute__(self, name):
        if name in LegacyFakeQuartz._HIDDEN:
            raise AttributeError(name)
        return super().__getattribute__(name)


class MacOSBackendTests(unittest.TestCase):
    def setUp(self):
        self.quartz = FakeQuartz()
        self.spawned = []
        self.tool_calls = []
        self.capture = FAKE_PNG
        self.backend = MacOSBackend(quartz=self.quartz, type_delay_ms=0,
                                    spawn=self.spawned.append, runner=self._run,
                                    sleep=lambda _s: None)
        self.agent = DesktopAgent(token=AGENT_TOKEN, backend=self.backend)

    def _run(self, argv):
        self.tool_calls.append(argv)
        with open(argv[-1], "wb") as fh:
            fh.write(self.capture)
        return ""

    def events(self, kind):
        return [event for _tap, event in self.quartz.posted if event.kind == kind]

    def test_health_reports_the_macos_backend(self):
        health = self.agent.health()
        self.assertEqual(health["backend"], "macos")
        self.assertEqual(health["screen"], {"width": 1440, "height": 900})

    def test_click_moves_then_presses_with_a_rising_click_state(self):
        self.backend.click(12, 34, "left", 2)
        events = self.events("mouse")
        self.assertEqual([event.fields["type"] for event in events],
                         ["mouse-moved", "left-down", "left-up", "left-down", "left-up"])
        self.assertEqual([event.fields["position"] for event in events], [(12, 34)] * 5)
        self.assertEqual([event.fields.get("click-state") for event in events[1:]],
                         [1, 1, 2, 2])

    def test_retina_geometry_matches_the_frame_and_keeps_every_pixel_reachable(self):
        # A 2x display in the 1440x900 HiDPI mode: Quartz reports 1440x900
        # points while screencapture writes the 2880x1800 backing store, and the
        # backing store is the frame the Gateway hands the model.
        self.quartz.pixel_width, self.quartz.pixel_height = 2880, 1800
        self.capture = _solid_png(2880, 1800)

        self.assertEqual(self.agent.health()["screen"], {"width": 2880, "height": 1800})
        width, height, png = self.backend.screenshot()
        self.assertEqual((width, height), (2880, 1800))
        self.assertEqual(_png_dimensions(png), (width, height))

        # The lower right quadrant of the frame was rejected as off screen while
        # geometry() reported points, and an accepted click landed at half its
        # intended position.
        self.assertEqual(self.agent.click({"x": 2000, "y": 1200, "button": "left"}),
                         {"applied": True, "x": 2000, "y": 1200,
                          "button": "left", "clicks": 1})
        self.assertEqual([event.fields["position"] for event in self.events("mouse")],
                         [(1000.0, 600.0)] * 3)

    def test_retina_scroll_moves_the_pointer_in_points(self):
        self.quartz.pixel_width, self.quartz.pixel_height = 2880, 1800
        self.agent.scroll({"x": 2400, "y": 1600, "deltaX": 0, "deltaY": 120})
        self.assertEqual(self.events("mouse")[0].fields["position"], (1200.0, 800.0))

    def test_geometry_falls_back_to_points_without_the_display_mode_api(self):
        backend = MacOSBackend(quartz=LegacyFakeQuartz(), type_delay_ms=0,
                               spawn=self.spawned.append, runner=self._run,
                               sleep=lambda _s: None)
        self.assertEqual(backend.geometry(), (1440, 900))
        backend.click(12, 34, "left", 1)
        self.assertEqual(backend.quartz.posted[0][1].fields["position"], (12.0, 34.0))

    def test_a_display_mode_without_a_backing_size_falls_back_to_points(self):
        self.quartz.pixel_width, self.quartz.pixel_height = 0, 0
        self.assertEqual(self.backend.geometry(), (1440, 900))

    def test_an_empty_display_geometry_is_a_500(self):
        self.quartz.width, self.quartz.height = 0, 0
        with self.assertRaises(AgentError) as ctx:
            self.backend.geometry()
        self.assertEqual(ctx.exception.status, 500)

    def test_click_maps_the_middle_button_to_the_other_button(self):
        self.backend.click(1, 2, "middle", 1)
        events = self.events("mouse")
        self.assertEqual([event.fields["type"] for event in events],
                         ["mouse-moved", "other-down", "other-up"])
        self.assertEqual(events[1].fields["button"], FakeQuartz.kCGMouseButtonCenter)

    def test_type_text_posts_a_unicode_string_per_character(self):
        self.backend.type_text("hé")
        events = self.events("key")
        self.assertEqual([event.fields["text"] for event in events], ["h", "h", "é", "é"])
        self.assertEqual([event.fields["down"] for event in events], [True, False, True, False])
        self.assertEqual([event.fields["key_code"] for event in events], [0, 0, 0, 0])

    def test_type_text_sends_control_characters_as_key_codes(self):
        self.backend.type_text("\r\n")
        events = self.events("key")
        self.assertEqual([event.fields["key_code"] for event in events], [0x24, 0x24])

    def test_press_key_maps_modifiers_to_event_flags(self):
        action = DesktopAgent._prepare_key({"key": "cmd+a"})
        self.assertEqual(self.backend.press_key(action["modifiers"], action["key"]), "super+a")
        events = self.events("key")
        self.assertEqual([event.fields["key_code"] for event in events], [0x00, 0x00])
        self.assertEqual([event.fields["flags"] for event in events],
                         [FakeQuartz.kCGEventFlagMaskCommand] * 2)

    def test_press_key_adds_shift_for_an_uppercase_character(self):
        self.backend.press_key(["ctrl"], "A")
        flags = self.events("key")[0].fields["flags"]
        self.assertEqual(flags,
                         FakeQuartz.kCGEventFlagMaskControl | FakeQuartz.kCGEventFlagMaskShift)

    def test_press_key_rejects_keys_and_modifiers_it_cannot_map(self):
        with self.assertRaises(AgentError) as ctx:
            self.backend.press_key([], "hyper")
        self.assertEqual(ctx.exception.status, 500)
        with self.assertRaises(AgentError) as ctx:
            self.backend.press_key(["hyper"], "a")
        self.assertEqual(ctx.exception.status, 500)

    def test_scroll_posts_pixel_deltas_per_step_vertical_first(self):
        self.agent.scroll({"x": 5, "y": 6, "deltaX": 130, "deltaY": -360})
        scrolls = self.events("scroll")
        self.assertEqual([event.fields["deltas"] for event in scrolls],
                         [(120, 0), (120, 0), (120, 0), (0, 120)])
        self.assertEqual(self.events("mouse")[0].fields["position"], (5, 6))

    def test_screenshot_uses_screencapture_and_removes_the_temp_file(self):
        width, height, data = self.backend.screenshot()
        self.assertEqual((width, height), (1440, 900))
        self.assertEqual(data, FAKE_PNG)
        argv = self.tool_calls[-1]
        self.assertEqual(argv[:4], ["screencapture", "-x", "-t", "png"])
        self.assertTrue(argv[-1].startswith(tempfile.gettempdir()))
        self.assertFalse(os.path.exists(argv[-1]))

    def test_list_windows_skips_overlays_and_falls_back_to_the_owner_name(self):
        self.quartz.window_info = [
            {"kCGWindowNumber": 42, "kCGWindowLayer": 0, "kCGWindowName": "Notes"},
            {"kCGWindowNumber": 43, "kCGWindowLayer": 25, "kCGWindowName": "Menubar"},
            {"kCGWindowNumber": 44, "kCGWindowLayer": 0, "kCGWindowName": "",
             "kCGWindowOwnerName": "Safari"},
            {"kCGWindowNumber": 45, "kCGWindowLayer": 0},
        ]
        self.assertEqual(self.agent.windows(), {"windows": [
            {"id": "0x0000002a", "desktop": 0, "title": "Notes"},
            {"id": "0x0000002c", "desktop": 0, "title": "Safari"},
        ]})
        self.assertEqual(self.quartz.list_options,
                         FakeQuartz.kCGWindowListOptionOnScreenOnly
                         | FakeQuartz.kCGWindowListExcludeDesktopElements)

    def test_launch_opens_applications_by_name(self):
        self.assertEqual(self.agent.launch({"app": "browser"}),
                         {"applied": True, "command": ["open", "-a", "Safari"]})
        self.assertEqual(self.agent.launch({"app": "firefox"})["command"],
                         ["open", "-a", "Firefox"])
        self.assertEqual(self.spawned[0], ["open", "-a", "Safari"])
        with self.assertRaises(AgentError) as ctx:
            self.agent.launch({"app": "definitely-not-installed-tool-xyz"})
        self.assertEqual(ctx.exception.status, 400)

    def test_missing_quartz_is_a_startup_failure_naming_the_dependency(self):
        original = desktop_agent._import_quartz

        def missing():
            raise ImportError("No module named 'Quartz'")

        desktop_agent._import_quartz = missing
        self.addCleanup(setattr, desktop_agent, "_import_quartz", original)
        with self.assertRaises(BackendUnavailable) as ctx:
            MacOSBackend()
        self.assertIn("pyobjc-framework-Quartz", str(ctx.exception))
        with self.assertRaises(SystemExit) as exit_ctx:
            desktop_agent.create_backend_or_exit("macos", {})
        self.assertEqual(exit_ctx.exception.code, 2)


# ----------------------------------------------------------------------
# PNG encoder
# ----------------------------------------------------------------------


def _png_dimensions(data):
    """Read the frame size out of a PNG IHDR without decompressing the image."""
    assert data[:8] == desktop_agent.PNG_SIGNATURE, "missing PNG signature"
    assert data[12:16] == b"IHDR", "IHDR must be the first chunk"
    return struct.unpack_from(">II", data, 16)


def _solid_png(width, height):
    """A black frame of the requested size, for tests that only assert its size."""
    return desktop_agent.encode_png(width, height, bytes(width * height * 3))


def _decode_png(data):
    """Parse a PNG the encoder produced back into (width, height, RGB bytes)."""
    assert data[:8] == desktop_agent.PNG_SIGNATURE, "missing PNG signature"
    offset = 8
    header = None
    compressed = b""
    seen_end = False
    while offset < len(data):
        (length,) = struct.unpack_from(">I", data, offset)
        kind = data[offset + 4:offset + 8]
        payload = data[offset + 8:offset + 8 + length]
        (crc,) = struct.unpack_from(">I", data, offset + 8 + length)
        assert crc == zlib.crc32(kind + payload) & 0xFFFFFFFF, f"bad CRC on {kind!r}"
        if kind == b"IHDR":
            header = struct.unpack(">IIBBBBB", payload)
        elif kind == b"IDAT":
            compressed += payload
        elif kind == b"IEND":
            seen_end = True
        offset += 12 + length
    assert header is not None and seen_end, "truncated PNG"
    width, height, depth, color_type, compression, filters, interlace = header
    assert (depth, color_type, compression, filters, interlace) == (8, 2, 0, 0, 0)
    raw = zlib.decompress(compressed)
    stride = width * 3
    pixels = bytearray()
    for row in range(height):
        start = row * (stride + 1)
        assert raw[start] == 0, "only filter type 0 is emitted"
        pixels += raw[start + 1:start + 1 + stride]
    return width, height, bytes(pixels)


class PngEncoderTests(unittest.TestCase):
    def test_round_trip_returns_the_original_pixels(self):
        pixels = bytes(range(3 * 4 * 3))
        self.assertEqual(_decode_png(desktop_agent.encode_png(4, 3, pixels)),
                         (4, 3, pixels))

    def test_single_pixel_image(self):
        self.assertEqual(_decode_png(desktop_agent.encode_png(1, 1, b"\xff\x00\x7f")),
                         (1, 1, b"\xff\x00\x7f"))

    def test_pixel_count_mismatch_is_refused(self):
        with self.assertRaises(ValueError):
            desktop_agent.encode_png(2, 2, b"\x00" * 11)

    def test_bgrx_to_rgb_swaps_channels_and_drops_padding(self):
        self.assertEqual(desktop_agent.bgrx_to_rgb(bytes([1, 2, 3, 255, 4, 5, 6, 255]), 2, 1),
                         bytes([3, 2, 1, 6, 5, 4]))
        with self.assertRaises(AgentError):
            desktop_agent.bgrx_to_rgb(b"\x00\x00\x00", 2, 1)


# ----------------------------------------------------------------------
# Backend selection and cross-backend key equivalence
# ----------------------------------------------------------------------


class BackendSelectionTests(unittest.TestCase):
    def test_auto_follows_the_platform(self):
        self.assertEqual(desktop_agent.resolve_backend_name("auto", "linux"), "x11")
        self.assertEqual(desktop_agent.resolve_backend_name("auto", "win32"), "windows")
        self.assertEqual(desktop_agent.resolve_backend_name("auto", "darwin"), "macos")
        self.assertEqual(desktop_agent.resolve_backend_name("", "linux"), "x11")
        self.assertEqual(desktop_agent.resolve_backend_name(None, "freebsd14"), "x11")

    def test_explicit_backend_overrides_the_platform(self):
        self.assertEqual(desktop_agent.resolve_backend_name("X11", "darwin"), "x11")
        self.assertEqual(desktop_agent.resolve_backend_name(" macos ", "linux"), "macos")

    def test_unknown_backend_is_a_startup_failure(self):
        with self.assertRaises(SystemExit) as ctx:
            desktop_agent.resolve_backend_name("wayland", "linux")
        self.assertIn("wayland", str(ctx.exception))

    def test_os_name_families(self):
        self.assertEqual(desktop_agent.os_name("linux"), "linux")
        self.assertEqual(desktop_agent.os_name("win32"), "windows")
        self.assertEqual(desktop_agent.os_name("darwin"), "darwin")

    def test_build_backend_passes_the_tool_overrides_through(self):
        backend = desktop_agent.build_backend("x11", {
            "DESKTOP_AGENT_XDOTOOL": "/opt/xdotool",
            "DESKTOP_AGENT_SCROT": "/opt/scrot",
            "DESKTOP_AGENT_WMCTRL": "/opt/wmctrl",
            "DESKTOP_AGENT_TYPE_DELAY_MS": "11",
        })
        self.assertEqual((backend.xdotool, backend.scrot, backend.wmctrl,
                          backend.type_delay_ms),
                         ("/opt/xdotool", "/opt/scrot", "/opt/wmctrl", 11))

    @unittest.skipIf(hasattr(__import__("ctypes"), "WinDLL"), "runs off Windows only")
    def test_windows_backend_refuses_to_start_without_the_win32_api(self):
        with self.assertRaises(SystemExit) as ctx:
            desktop_agent.create_backend_or_exit("windows", {})
        self.assertEqual(ctx.exception.code, 2)


class CrossBackendKeyTests(unittest.TestCase):
    """One model-facing key spec must reach the same key on every backend."""

    def setUp(self):
        self.win32 = FakeWin32()
        self.windows = WindowsBackend(api=self.win32, type_delay_ms=0,
                                      spawn=lambda argv: None, sleep=lambda _s: None)
        self.quartz = FakeQuartz()
        self.macos = MacOSBackend(quartz=self.quartz, type_delay_ms=0,
                                  spawn=lambda argv: None, runner=lambda argv: "",
                                  sleep=lambda _s: None)

    def windows_key(self, action):
        self.win32.batches.clear()
        self.windows.press_key(action["modifiers"], action["key"])
        events = self.win32.batches[-1]
        # The pressed key sits between the modifier presses and their releases.
        return [event["vk"] for event in events[:len(events) // 2]]

    def macos_key(self, action):
        self.quartz.posted.clear()
        self.macos.press_key(action["modifiers"], action["key"])
        event = self.quartz.posted[0][1]
        return event.fields["key_code"], event.fields["flags"]

    def test_aliases_resolve_to_the_same_key_on_every_backend(self):
        cases = [
            # spec, x11 spec, windows [modifier vks..., key vk], macos (code, flags)
            ("Control+a", "ctrl+a", [0x11, 0x41], (0x00, FakeQuartz.kCGEventFlagMaskControl)),
            ("ctrl+a", "ctrl+a", [0x11, 0x41], (0x00, FakeQuartz.kCGEventFlagMaskControl)),
            ("cmd+a", "super+a", [0x5B, 0x41], (0x00, FakeQuartz.kCGEventFlagMaskCommand)),
            ("win+a", "super+a", [0x5B, 0x41], (0x00, FakeQuartz.kCGEventFlagMaskCommand)),
            ("meta+a", "super+a", [0x5B, 0x41], (0x00, FakeQuartz.kCGEventFlagMaskCommand)),
            ("option+a", "alt+a", [0x12, 0x41],
             (0x00, FakeQuartz.kCGEventFlagMaskAlternate)),
            ("shift+tab", "shift+Tab", [0x10, 0x09], (0x30, FakeQuartz.kCGEventFlagMaskShift)),
            ("Return", "Return", [0x0D], (0x24, 0)),
            ("enter", "Return", [0x0D], (0x24, 0)),
            ("escape", "Escape", [0x1B], (0x35, 0)),
            ("esc", "Escape", [0x1B], (0x35, 0)),
            ("space", "space", [0x20], (0x31, 0)),
            ("backspace", "BackSpace", [0x08], (0x33, 0)),
            ("del", "Delete", [0x2E], (0x75, 0)),
            ("delete", "Delete", [0x2E], (0x75, 0)),
            ("insert", "Insert", [0x2D], (0x72, 0)),
            ("home", "Home", [0x24], (0x73, 0)),
            ("end", "End", [0x23], (0x77, 0)),
            ("pageup", "Prior", [0x21], (0x74, 0)),
            ("pagedown", "Next", [0x22], (0x79, 0)),
            ("up", "Up", [0x26], (0x7E, 0)),
            ("down", "Down", [0x28], (0x7D, 0)),
            ("left", "Left", [0x25], (0x7B, 0)),
            ("right", "Right", [0x27], (0x7C, 0)),
            ("F1", "F1", [0x70], (0x7A, 0)),
            ("F12", "F12", [0x7B], (0x6F, 0)),
        ]
        for spec, x11_spec, windows_keys, macos_key in cases:
            with self.subTest(spec=spec):
                action = DesktopAgent._prepare_key({"key": spec})
                rendered = "+".join(
                    action["modifiers"]
                    + [desktop_agent.X11_NAMED_KEYS.get(action["key"], action["key"])])
                self.assertEqual(rendered, x11_spec)
                self.assertEqual(self.windows_key(action), windows_keys)
                self.assertEqual(self.macos_key(action), macos_key)


if __name__ == "__main__":
    unittest.main()
