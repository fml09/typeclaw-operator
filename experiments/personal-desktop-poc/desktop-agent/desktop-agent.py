#!/usr/bin/env python3
"""Personal Desktop PoC guest-side desktop agent.

KubeVirt Ubuntu 24.04 XFCE guest 안에서 실행하는 단일 파일 HTTP agent입니다.
PoC Gateway(Go)가 computer-use 관찰/입력 요청을 이 agent로 proxy하며, 기존
agent-browser/noVNC 제어 경로를 대체합니다.

안전 설계:
- 모든 endpoint에 Bearer token 인증을 적용합니다. token은 시작 시
  DESKTOP_AGENT_TOKEN_FILE(기본 /etc/personal-desktop/agent-token)의 첫 줄에서
  읽고, 길이가 24 미만이면 stderr에 이유를 남기고 exit(1) 합니다. 비교는
  hmac.compare_digest로만 합니다.
- 도구(xdotool/scrot/wmctrl)는 shell 없이 argv list로만 실행합니다. 사용자
  text는 항상 `--` 뒤에 배치해서 option injection을 차단합니다.
- X11 도구는 reentrant하지 않으므로 GET /health를 제외한 모든 요청을 lock
  하나로 직렬화합니다.
- gateway가 1차 검증하지만 agent도 재검증합니다: 좌표/화면 범위, text 길이,
  key 형식, app 이름. 검증 실패는 400, 도구 실패는 500(JSON), 요청 body는
  1MiB 제한입니다.

표준 라이브러리만 사용합니다. guest에는 python3, xdotool, scrot, wmctrl만
설치하면 됩니다.
"""

import hmac
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

DEFAULT_TOKEN_FILE = "/etc/personal-desktop/agent-token"
DEFAULT_HOST = "0.0.0.0"
DEFAULT_PORT = 9876
DEFAULT_TYPE_DELAY_MS = 6

MAX_BODY_BYTES = 1_000_000
MAX_TEXT_CHARS = 4000
MAX_DELTA = 4000
MAX_KEY_CHARS = 80
SCROLL_STEP_PX = 120
MAX_SCROLL_STEPS = 40

KEY_SPEC_RE = re.compile(r"^[A-Za-z0-9+_-]+$")
APP_NAME_RE = re.compile(r"^[a-z0-9][a-z0-9._-]{0,63}$")

BUTTON_NUMBERS = {"left": "1", "middle": "2", "right": "3"}
BUILTIN_APPS = {
    "firefox": ["firefox"],
    "terminal": ["xfce4-terminal"],
    "files": ["thunar"],
}
MODIFIER_KEYS = {
    "control": "ctrl",
    "ctrl": "ctrl",
    "cmd": "super",
    "meta": "super",
    "win": "super",
    "super": "super",
    "option": "alt",
    "alt": "alt",
    "shift": "shift",
}
NAMED_KEYS = {
    "enter": "Return",
    "return": "Return",
    "esc": "Escape",
    "escape": "Escape",
    "tab": "Tab",
    "space": "space",
    "backspace": "BackSpace",
    "delete": "Delete",
    "del": "Delete",
    "insert": "Insert",
    "home": "Home",
    "end": "End",
    "pageup": "Prior",
    "pagedown": "Next",
    "up": "Up",
    "down": "Down",
    "left": "Left",
    "right": "Right",
}

ROUTES = {
    "/health": "GET",
    "/windows": "GET",
    "/screenshot": "POST",
    "/click": "POST",
    "/type": "POST",
    "/key": "POST",
    "/scroll": "POST",
    "/launch": "POST",
}


class AgentError(Exception):
    """Request-scoped failure carrying an HTTP status."""

    def __init__(self, status, message):
        super().__init__(message)
        self.status = status
        self.message = message


def _int_field(payload, name, minimum=None, maximum=None):
    value = payload.get(name)
    if isinstance(value, bool) or not isinstance(value, int):
        raise AgentError(400, f"{name} must be an integer")
    if minimum is not None and value < minimum:
        raise AgentError(400, f"{name} must be >= {minimum}")
    if maximum is not None and value > maximum:
        raise AgentError(400, f"{name} must be <= {maximum}")
    return value


class DesktopAgent:
    """Executes validated computer-use actions with the guest's X11 tools."""

    def __init__(self, token, xdotool="xdotool", scrot="scrot", wmctrl="wmctrl",
                 type_delay_ms=DEFAULT_TYPE_DELAY_MS):
        self.token = token
        self.xdotool = xdotool
        self.scrot = scrot
        self.wmctrl = wmctrl
        self.type_delay_ms = type_delay_ms
        self.lock = threading.Lock()

    # ------------------------------------------------------------------
    # tool helpers
    # ------------------------------------------------------------------

    def _run(self, argv):
        """Run a tool without a shell; spawn failure or non-zero exit is a 500."""
        try:
            proc = subprocess.run(argv, capture_output=True)
        except OSError as exc:
            raise AgentError(500, f"{os.path.basename(argv[0])} invocation failed: {exc}") from exc
        if proc.returncode != 0:
            stderr = proc.stderr.decode("utf-8", "replace").strip()
            message = f"{os.path.basename(argv[0])} exited with {proc.returncode}"
            if stderr:
                message += f": {stderr}"
            raise AgentError(500, message)
        return proc.stdout.decode("utf-8", "replace")

    def _geometry(self):
        out = self._run([self.xdotool, "getdisplaygeometry"]).strip()
        parts = out.split()
        if len(parts) != 2:
            raise AgentError(500, f"unexpected getdisplaygeometry output: {out!r}")
        try:
            return int(parts[0]), int(parts[1])
        except ValueError:
            raise AgentError(500, f"unexpected getdisplaygeometry output: {out!r}") from None

    def _require_on_screen(self, x, y):
        width, height = self._geometry()
        if not (0 <= x < width and 0 <= y < height):
            raise AgentError(400, f"coordinates ({x},{y}) exceeds screen {width}x{height}")

    # ------------------------------------------------------------------
    # endpoints
    # ------------------------------------------------------------------

    def health(self):
        width, height = self._geometry()
        return {"ok": True, "agent": "desktop-agent/1.0",
                "screen": {"width": width, "height": height}}

    def screenshot(self):
        width, height = self._geometry()
        # scrot 1.10 does not overwrite an existing file: it silently saves to
        # an auto-numbered name instead. Generate a unique path WITHOUT
        # creating it so scrot writes exactly where we read.
        path = os.path.join(tempfile.gettempdir(),
                            f"desktop-agent-{os.getpid()}-{time.time_ns()}.png")
        try:
            self._run([self.scrot, "-z", path])
            with open(path, "rb") as fh:
                return width, height, fh.read()
        finally:
            try:
                os.unlink(path)
            except FileNotFoundError:
                pass

    def click(self, payload):
        x = _int_field(payload, "x", minimum=0)
        y = _int_field(payload, "y", minimum=0)
        button = payload.get("button")
        if button not in BUTTON_NUMBERS:
            raise AgentError(400, "button must be one of left, middle, right")
        clicks = payload.get("clicks", 1)
        if isinstance(clicks, bool) or clicks not in (1, 2):
            raise AgentError(400, "clicks must be 1 or 2")
        self._require_on_screen(x, y)
        self._run([self.xdotool, "mousemove", "--sync", str(x), str(y)])
        self._run([self.xdotool, "click", "--repeat", str(clicks), "--delay", "100",
                   BUTTON_NUMBERS[button]])
        return {"applied": True, "x": x, "y": y, "button": button, "clicks": clicks}

    def type_text(self, payload):
        text = payload.get("text")
        if not isinstance(text, str):
            raise AgentError(400, "text must be a string")
        if len(text) > MAX_TEXT_CHARS:
            raise AgentError(400, f"text must be at most {MAX_TEXT_CHARS} characters")
        self._run([self.xdotool, "type", "--delay", str(self.type_delay_ms), "--", text])
        return {"applied": True, "characters": len(text)}

    def press_key(self, payload):
        spec = payload.get("key")
        if not isinstance(spec, str) or len(spec) > MAX_KEY_CHARS or not KEY_SPEC_RE.match(spec):
            raise AgentError(400, f"key must match [A-Za-z0-9+_-]+ and be at most "
                                  f"{MAX_KEY_CHARS} characters")
        tokens = spec.split("+")
        if any(token == "" for token in tokens):
            raise AgentError(400, "key contains an empty + separated token")
        modifiers = [MODIFIER_KEYS.get(token.lower(), token.lower()) for token in tokens[:-1]]
        last = tokens[-1]
        keysym = NAMED_KEYS.get(last.lower(), last)
        normalized = "+".join(modifiers + [keysym])
        self._run([self.xdotool, "key", "--", normalized])
        return {"applied": True, "key": normalized}

    def scroll(self, payload):
        x = _int_field(payload, "x", minimum=0)
        y = _int_field(payload, "y", minimum=0)
        delta_x = _int_field(payload, "deltaX", minimum=-MAX_DELTA, maximum=MAX_DELTA)
        delta_y = _int_field(payload, "deltaY", minimum=-MAX_DELTA, maximum=MAX_DELTA)
        self._require_on_screen(x, y)
        self._run([self.xdotool, "mousemove", "--sync", str(x), str(y)])
        wheels = []
        if delta_y != 0:
            wheels.append(("5" if delta_y > 0 else "4", delta_y))
        if delta_x != 0:
            wheels.append(("7" if delta_x > 0 else "6", delta_x))
        for button, delta in wheels:
            for _ in self._wheel_steps(delta):
                self._run([self.xdotool, "click", button])
        return {"applied": True}

    @staticmethod
    def _wheel_steps(delta):
        steps = max(1, min(MAX_SCROLL_STEPS, round(abs(delta) / SCROLL_STEP_PX)))
        return range(steps)

    def launch(self, payload):
        app = payload.get("app")
        if not isinstance(app, str):
            raise AgentError(400, "app must be a string")
        if app in BUILTIN_APPS:
            command = list(BUILTIN_APPS[app])
        else:
            if not APP_NAME_RE.match(app) or shutil.which(app) is None:
                raise AgentError(400, f"unknown app: {app!r}")
            command = [app]
        try:
            subprocess.Popen(command, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                             start_new_session=True)
        except OSError as exc:
            raise AgentError(500, f"launch failed: {exc}") from exc
        return {"applied": True, "command": command}

    def windows(self):
        out = self._run([self.wmctrl, "-l"])
        rows = []
        for line in out.splitlines():
            parts = line.split(None, 3)
            if len(parts) != 4:
                continue
            try:
                desktop = int(parts[1])
            except ValueError:
                continue
            rows.append({"id": parts[0], "desktop": desktop, "title": parts[3]})
        return {"windows": rows}

    def handle_action(self, path, payload):
        if path == "/click":
            return self.click(payload)
        if path == "/type":
            return self.type_text(payload)
        if path == "/key":
            return self.press_key(payload)
        if path == "/scroll":
            return self.scroll(payload)
        if path == "/launch":
            return self.launch(payload)
        raise AgentError(404, f"not found: {path}")


class AgentHTTPRequestHandler(BaseHTTPRequestHandler):
    server_version = "desktop-agent/1.0"

    def do_GET(self):
        self._dispatch("GET")

    def do_POST(self):
        self._dispatch("POST")

    def _dispatch(self, method):
        agent = self.server.agent
        path = self.path.split("?", 1)[0]
        try:
            expected_method = ROUTES.get(path)
            if expected_method is None:
                self._send_json(404, {"error": f"not found: {path}"})
                return
            if not self._authorized(agent.token):
                self._send_json(401, {"error": "unauthorized"})
                return
            if method != expected_method:
                self._send_json(405, {"error": f"{method} not allowed on {path}"})
                return
            if path == "/health":
                self._send_json(200, agent.health())
                return
            if path == "/screenshot":
                self._read_length()
                with agent.lock:
                    width, height, png = agent.screenshot()
                self._send_png(width, height, png)
                return
            if method == "POST":
                payload = self._read_json()
            else:
                payload = None
            with agent.lock:
                if path == "/windows":
                    result = agent.windows()
                else:
                    result = agent.handle_action(path, payload)
            self._send_json(200, result)
        except AgentError as exc:
            self._send_json(exc.status, {"error": exc.message})
        except Exception as exc:
            print(f"desktop-agent: {method} {path} failed: {exc!r}", file=sys.stderr, flush=True)
            self._send_json(500, {"error": f"internal error: {exc}"})

    def _authorized(self, token):
        header = self.headers.get("Authorization", "")
        if not header.startswith("Bearer "):
            return False
        provided = header[len("Bearer "):].strip()
        return hmac.compare_digest(provided.encode("utf-8"), token.encode("utf-8"))

    def _read_length(self):
        raw = self.headers.get("Content-Length")
        if raw is None:
            raise AgentError(413, "Content-Length header is required")
        try:
            length = int(raw)
        except ValueError:
            raise AgentError(413, f"invalid Content-Length: {raw!r}") from None
        if length < 0 or length > MAX_BODY_BYTES:
            raise AgentError(413, f"body too large: {length} > {MAX_BODY_BYTES} bytes")
        return length

    def _read_json(self):
        length = self._read_length()
        body = self.rfile.read(length) if length else b""
        try:
            payload = json.loads(body.decode("utf-8"))
        except (UnicodeDecodeError, ValueError):
            raise AgentError(400, "malformed JSON body") from None
        if not isinstance(payload, dict):
            raise AgentError(400, "JSON body must be an object")
        return payload

    def _send_json(self, status, payload):
        data = json.dumps(payload).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def _send_png(self, width, height, data):
        self.send_response(200)
        self.send_header("Content-Type", "image/png")
        self.send_header("X-Screen-Width", str(width))
        self.send_header("X-Screen-Height", str(height))
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)


class AgentHTTPServer(ThreadingHTTPServer):
    daemon_threads = True
    allow_reuse_address = True

    def __init__(self, address, agent):
        self.agent = agent
        super().__init__(address, AgentHTTPRequestHandler)


def load_token(path):
    try:
        with open(path, "r", encoding="utf-8") as fh:
            token = fh.readline().strip()
    except OSError as exc:
        raise SystemExit(f"desktop-agent: cannot read token file {path}: {exc}")
    if len(token) < 24:
        raise SystemExit(f"desktop-agent: token in {path} is shorter than 24 characters")
    return token


def main():
    token = load_token(os.environ.get("DESKTOP_AGENT_TOKEN_FILE", DEFAULT_TOKEN_FILE))
    agent = DesktopAgent(
        token=token,
        xdotool=os.environ.get("DESKTOP_AGENT_XDOTOOL", "xdotool"),
        scrot=os.environ.get("DESKTOP_AGENT_SCROT", "scrot"),
        wmctrl=os.environ.get("DESKTOP_AGENT_WMCTRL", "wmctrl"),
        type_delay_ms=int(os.environ.get("DESKTOP_AGENT_TYPE_DELAY_MS",
                                         str(DEFAULT_TYPE_DELAY_MS))),
    )
    host = os.environ.get("DESKTOP_AGENT_HOST", DEFAULT_HOST)
    port = int(os.environ.get("DESKTOP_AGENT_PORT", str(DEFAULT_PORT)))
    server = AgentHTTPServer((host, port), agent)
    print(f"desktop-agent: listening on {host}:{port}", file=sys.stderr, flush=True)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()


if __name__ == "__main__":
    main()
