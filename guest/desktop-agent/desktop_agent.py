#!/usr/bin/env python3
"""Personal Desktop Guest Desktop Agent.

A single-file HTTP service that runs inside the interactive desktop session of
a Personal Desktop virtual machine. The Desktop Gateway proxies typed
computer-use actions to it. The protocol is OS-agnostic: the portable core
parses, validates and orders actions, and every OS-touching operation lives
behind a backend (X11, Windows, macOS).

Safety design (ported from the proof of concept; do not weaken):
- Every endpoint requires `Authorization: Bearer <token>`. The token is read at
  startup from DESKTOP_AGENT_TOKEN_FILE (or DESKTOP_AGENT_TOKEN); anything
  shorter than 24 characters aborts the process, and comparison is
  hmac.compare_digest only.
- External tools are executed as argv lists without a shell, and caller text is
  always placed after `--` so it can never be read as an option.
- Desktop input is not reentrant, so every request except GET /health is
  serialized on a single lock.
- The Gateway validates first, but the agent revalidates everything: screen
  bounds, text length, key shape, app names. Validation failures are 400, tool
  failures are 500, and the request body is capped at 1 MiB.
- A batch is validated in full before the first action is applied, so a batch
  that is rejected never leaves the desktop half-typed.

Standard library only on Linux and Windows. The macOS backend imports Quartz
lazily; if it is missing the process refuses to start instead of degrading to a
backend that silently drops input.
"""

import ctypes
import hmac
import json
import ntpath
import os
import re
import shutil
import struct
import subprocess
import sys
import tempfile
import threading
import time
import zlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

AGENT_VERSION = "desktop-agent/2.0"

POSIX_TOKEN_FILE = "/etc/personal-desktop/agent-token"
WINDOWS_TOKEN_FILE = r"PersonalDesktop\agent-token"
DEFAULT_BIND = "0.0.0.0"
DEFAULT_PORT = 9876
DEFAULT_TYPE_DELAY_MS = 6
MIN_TOKEN_CHARS = 24

MAX_BODY_BYTES = 1_000_000
MAX_TEXT_CHARS = 4000
MAX_ACTIONS_PER_BATCH = 16
MAX_DELTA = 4000
MAX_KEY_CHARS = 80
SCROLL_STEP_PX = 120
MAX_SCROLL_STEPS = 40

KEY_SPEC_RE = re.compile(r"^[A-Za-z0-9+_-]+$")
APP_NAME_RE = re.compile(r"^[a-z0-9][a-z0-9._-]{0,63}$")
FUNCTION_KEY_RE = re.compile(r"^f([1-9]|1[0-9]|2[0-4])$")

BACKEND_NAMES = ("x11", "windows", "macos")
BUTTONS = ("left", "middle", "right")

# One model-facing key vocabulary for every backend: the core normalizes an
# alias to a canonical name and each backend translates that name into its own
# key identifier. Unknown tokens are passed through verbatim (case preserved)
# so single characters and F-keys still work.
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
KEY_ALIASES = {
    "enter": "enter",
    "return": "enter",
    "esc": "esc",
    "escape": "esc",
    "tab": "tab",
    "space": "space",
    "backspace": "backspace",
    "delete": "delete",
    "del": "delete",
    "insert": "insert",
    "home": "home",
    "end": "end",
    "pageup": "pageup",
    "pagedown": "pagedown",
    "up": "up",
    "down": "down",
    "left": "left",
    "right": "right",
}

ROUTES = {
    "/health": "GET",
    "/windows": "GET",
    "/screenshot": "POST",
    "/actions": "POST",
    "/launch": "POST",
}


class AgentError(Exception):
    """Request-scoped failure carrying an HTTP status."""

    def __init__(self, status, message):
        super().__init__(message)
        self.status = status
        self.message = message


class BackendUnavailable(Exception):
    """The selected backend cannot run here (missing dependency or platform)."""


def _int_field(payload, name, minimum=None, maximum=None):
    value = payload.get(name)
    if isinstance(value, bool) or not isinstance(value, int):
        raise AgentError(400, f"{name} must be an integer")
    if minimum is not None and value < minimum:
        raise AgentError(400, f"{name} must be >= {minimum}")
    if maximum is not None and value > maximum:
        raise AgentError(400, f"{name} must be <= {maximum}")
    return value


def _run_tool(argv):
    """Run a guest tool without a shell; spawn failure or non-zero exit is a 500."""
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


def _unique_temp_png():
    """Return a screenshot path that does not exist yet.

    scrot 1.10 and screencapture refuse to overwrite an existing file and
    silently save to an auto-numbered name instead, so the path is generated
    without creating it and the tool writes exactly where we read.
    """
    return os.path.join(tempfile.gettempdir(),
                        f"desktop-agent-{os.getpid()}-{time.time_ns()}.png")


def _resolve_path_executable(app):
    """Validate a caller-supplied executable name and resolve it on PATH."""
    if not APP_NAME_RE.match(app) or shutil.which(app) is None:
        raise AgentError(400, f"unknown app: {app!r}")
    return [app]


def spawn_detached_posix(argv):
    """Start a desktop application that outlives this request."""
    try:
        subprocess.Popen(argv, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                         start_new_session=True)
    except OSError as exc:
        raise AgentError(500, f"launch failed: {exc}") from exc


# Windows process creation flags; subprocess exposes them only on Windows.
DETACHED_PROCESS = 0x00000008
CREATE_NEW_PROCESS_GROUP = 0x00000200


def spawn_detached_windows(argv):
    """Start a desktop application detached from the agent's console.

    Without DETACHED_PROCESS the launched program inherits the scheduled task's
    console and dies with it; without a new process group Ctrl-C delivered to
    the agent would also terminate the desktop application.
    """
    try:
        subprocess.Popen(argv, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                         creationflags=DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP)
    except OSError as exc:
        raise AgentError(500, f"launch failed: {exc}") from exc


# ----------------------------------------------------------------------
# PNG encoding (Windows GDI capture has no encoder of its own)
# ----------------------------------------------------------------------

PNG_SIGNATURE = b"\x89PNG\r\n\x1a\n"


def _png_chunk(kind, data):
    return (struct.pack(">I", len(data)) + kind + data
            + struct.pack(">I", zlib.crc32(kind + data) & 0xFFFFFFFF))


def encode_png(width, height, pixels):
    """Encode top-down 8-bit RGB pixels as a PNG using only zlib and struct.

    Every scanline uses filter type 0: the guest CPU is shared with the
    interactive session, so a cheap encode beats a smaller frame.
    """
    stride = width * 3
    if len(pixels) != stride * height:
        raise ValueError(f"expected {stride * height} pixel bytes, got {len(pixels)}")
    raw = bytearray()
    for row in range(height):
        raw.append(0)
        raw += pixels[row * stride:(row + 1) * stride]
    out = bytearray(PNG_SIGNATURE)
    out += _png_chunk(b"IHDR", struct.pack(">IIBBBBB", width, height, 8, 2, 0, 0, 0))
    out += _png_chunk(b"IDAT", zlib.compress(bytes(raw), 6))
    out += _png_chunk(b"IEND", b"")
    return bytes(out)


def bgrx_to_rgb(raw, width, height):
    """Convert a 32-bit bottom-up-free BGRX GDI buffer to packed RGB."""
    expected = width * height * 4
    if len(raw) != expected:
        raise AgentError(500, f"expected {expected} captured bytes, got {len(raw)}")
    source = memoryview(raw)
    rgb = bytearray(width * height * 3)
    rgb[0::3] = source[2::4]
    rgb[1::3] = source[1::4]
    rgb[2::3] = source[0::4]
    return bytes(rgb)


# ----------------------------------------------------------------------
# Backends
# ----------------------------------------------------------------------


class DesktopBackend:
    """Operations a desktop session must provide for typed computer use."""

    name = "unknown"

    def geometry(self):
        raise NotImplementedError

    def click(self, x, y, button, clicks):
        raise NotImplementedError

    def type_text(self, text):
        raise NotImplementedError

    def press_key(self, modifiers, key):
        raise NotImplementedError

    def scroll(self, x, y, wheels):
        raise NotImplementedError

    def screenshot(self):
        raise NotImplementedError

    def list_windows(self):
        raise NotImplementedError

    def launch(self, app):
        raise NotImplementedError


X11_NAMED_KEYS = {
    "enter": "Return",
    "esc": "Escape",
    "tab": "Tab",
    "space": "space",
    "backspace": "BackSpace",
    "delete": "Delete",
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
X11_BUTTON_NUMBERS = {"left": "1", "middle": "2", "right": "3"}
X11_WHEEL_BUTTONS = {"up": "4", "down": "5", "left": "6", "right": "7"}
X11_APPS = {
    "browser": ["firefox"],
    "firefox": ["firefox"],
    "terminal": ["xfce4-terminal"],
    "files": ["thunar"],
    "editor": ["mousepad"],
}


class X11Backend(DesktopBackend):
    """Linux X11 session driven by xdotool, scrot and wmctrl."""

    name = "x11"

    def __init__(self, xdotool="xdotool", scrot="scrot", wmctrl="wmctrl",
                 type_delay_ms=DEFAULT_TYPE_DELAY_MS, spawn=spawn_detached_posix):
        self.xdotool = xdotool
        self.scrot = scrot
        self.wmctrl = wmctrl
        self.type_delay_ms = type_delay_ms
        self.spawn = spawn

    def _run(self, argv):
        return _run_tool(argv)

    def geometry(self):
        out = self._run([self.xdotool, "getdisplaygeometry"]).strip()
        parts = out.split()
        if len(parts) != 2:
            raise AgentError(500, f"unexpected getdisplaygeometry output: {out!r}")
        try:
            return int(parts[0]), int(parts[1])
        except ValueError:
            raise AgentError(500, f"unexpected getdisplaygeometry output: {out!r}") from None

    def click(self, x, y, button, clicks):
        self._run([self.xdotool, "mousemove", "--sync", str(x), str(y)])
        self._run([self.xdotool, "click", "--repeat", str(clicks), "--delay", "100",
                   X11_BUTTON_NUMBERS[button]])

    def type_text(self, text):
        self._run([self.xdotool, "type", "--delay", str(self.type_delay_ms), "--", text])

    def press_key(self, modifiers, key):
        spec = "+".join(list(modifiers) + [X11_NAMED_KEYS.get(key, key)])
        self._run([self.xdotool, "key", "--", spec])
        return spec

    def scroll(self, x, y, wheels):
        self._run([self.xdotool, "mousemove", "--sync", str(x), str(y)])
        for direction, steps in wheels:
            button = X11_WHEEL_BUTTONS[direction]
            for _ in range(steps):
                self._run([self.xdotool, "click", button])

    def screenshot(self):
        width, height = self.geometry()
        path = _unique_temp_png()
        try:
            self._run([self.scrot, "-z", path])
            with open(path, "rb") as fh:
                return width, height, fh.read()
        finally:
            try:
                os.unlink(path)
            except FileNotFoundError:
                pass

    def list_windows(self):
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
        return rows

    def launch(self, app):
        command = list(X11_APPS[app]) if app in X11_APPS else _resolve_path_executable(app)
        self.spawn(command)
        return command


# ----------------------------------------------------------------------
# Windows backend
# ----------------------------------------------------------------------

INPUT_MOUSE = 0
INPUT_KEYBOARD = 1

MOUSEEVENTF_MOVE = 0x0001
MOUSEEVENTF_LEFTDOWN = 0x0002
MOUSEEVENTF_LEFTUP = 0x0004
MOUSEEVENTF_RIGHTDOWN = 0x0008
MOUSEEVENTF_RIGHTUP = 0x0010
MOUSEEVENTF_MIDDLEDOWN = 0x0020
MOUSEEVENTF_MIDDLEUP = 0x0040
MOUSEEVENTF_WHEEL = 0x0800
MOUSEEVENTF_HWHEEL = 0x1000
MOUSEEVENTF_ABSOLUTE = 0x8000
WHEEL_DELTA = 120

KEYEVENTF_EXTENDEDKEY = 0x0001
KEYEVENTF_KEYUP = 0x0002
KEYEVENTF_UNICODE = 0x0004

ABSOLUTE_RANGE = 65535
SRCCOPY = 0x00CC0020
DIB_RGB_COLORS = 0
BI_RGB = 0
SM_CXSCREEN = 0
SM_CYSCREEN = 1
# SetProcessDpiAwarenessContext argument: per-monitor aware v2.
DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 = -4

WINDOWS_MODIFIER_VK = {"ctrl": 0x11, "alt": 0x12, "shift": 0x10, "super": 0x5B}
WINDOWS_NAMED_KEYS = {
    "enter": 0x0D,
    "esc": 0x1B,
    "tab": 0x09,
    "space": 0x20,
    "backspace": 0x08,
    "delete": 0x2E,
    "insert": 0x2D,
    "home": 0x24,
    "end": 0x23,
    "pageup": 0x21,
    "pagedown": 0x22,
    "up": 0x26,
    "down": 0x28,
    "left": 0x25,
    "right": 0x27,
}
# The navigation cluster and the Windows keys share virtual-key codes with the
# numeric keypad; without KEYEVENTF_EXTENDEDKEY applications see the keypad
# variant and NumLock decides what the user gets.
WINDOWS_EXTENDED_VK = frozenset({0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27, 0x28,
                                 0x2D, 0x2E, 0x5B, 0x5C})
WINDOWS_BUTTON_FLAGS = {
    "left": (MOUSEEVENTF_LEFTDOWN, MOUSEEVENTF_LEFTUP),
    "middle": (MOUSEEVENTF_MIDDLEDOWN, MOUSEEVENTF_MIDDLEUP),
    "right": (MOUSEEVENTF_RIGHTDOWN, MOUSEEVENTF_RIGHTUP),
}
WINDOWS_WHEEL = {
    "up": (MOUSEEVENTF_WHEEL, WHEEL_DELTA),
    "down": (MOUSEEVENTF_WHEEL, -WHEEL_DELTA),
    "right": (MOUSEEVENTF_HWHEEL, WHEEL_DELTA),
    "left": (MOUSEEVENTF_HWHEEL, -WHEEL_DELTA),
}
# `start` is used where the target is resolved through App Paths rather than
# PATH; explorer.exe and notepad.exe live in System32 and need no shell.
WINDOWS_APPS = {
    "browser": ["cmd", "/c", "start", "", "msedge"],
    "firefox": ["cmd", "/c", "start", "", "firefox"],
    "terminal": ["cmd", "/c", "start", "", "cmd"],
    "files": ["explorer.exe"],
    "editor": ["notepad.exe"],
}

# DWORD/WORD/LONG are fixed width on Windows, while ctypes' c_ulong follows the
# host, so the SendInput structures are declared with explicit widths to keep
# one layout on every platform this file is tested on.
DWORD = ctypes.c_uint32
WORD = ctypes.c_uint16
LONG = ctypes.c_int32
ULONG_PTR = ctypes.c_uint64 if ctypes.sizeof(ctypes.c_void_p) == 8 else ctypes.c_uint32


class MOUSEINPUT(ctypes.Structure):
    _fields_ = [("dx", LONG), ("dy", LONG), ("mouseData", DWORD),
                ("dwFlags", DWORD), ("time", DWORD), ("dwExtraInfo", ULONG_PTR)]


class KEYBDINPUT(ctypes.Structure):
    _fields_ = [("wVk", WORD), ("wScan", WORD), ("dwFlags", DWORD),
                ("time", DWORD), ("dwExtraInfo", ULONG_PTR)]


class HARDWAREINPUT(ctypes.Structure):
    _fields_ = [("uMsg", DWORD), ("wParamL", WORD), ("wParamH", WORD)]


class _INPUTUNION(ctypes.Union):
    _fields_ = [("mi", MOUSEINPUT), ("ki", KEYBDINPUT), ("hi", HARDWAREINPUT)]


class INPUT(ctypes.Structure):
    _anonymous_ = ("u",)
    _fields_ = [("type", DWORD), ("u", _INPUTUNION)]


class BITMAPINFOHEADER(ctypes.Structure):
    _fields_ = [("biSize", DWORD), ("biWidth", LONG), ("biHeight", LONG),
                ("biPlanes", WORD), ("biBitCount", WORD), ("biCompression", DWORD),
                ("biSizeImage", DWORD), ("biXPelsPerMeter", LONG),
                ("biYPelsPerMeter", LONG), ("biClrUsed", DWORD), ("biClrImportant", DWORD)]


class BITMAPINFO(ctypes.Structure):
    _fields_ = [("bmiHeader", BITMAPINFOHEADER), ("bmiColors", DWORD * 3)]


def dib_header(width, height):
    """Describe the 32-bit top-down DIB GetDIBits must copy into.

    This lives outside Win32API because the façade is where the tests inject a
    fake: a mistake here (a positive height, which makes GDI hand back a
    vertically mirrored frame, or a bit count other than 32, which breaks the
    BGRX unpacking) would otherwise be invisible to every test.
    """
    info = BITMAPINFO()
    info.bmiHeader.biSize = ctypes.sizeof(BITMAPINFOHEADER)
    info.bmiHeader.biWidth = width
    # A negative height requests a top-down buffer; GDI's default bottom-up
    # order would hand the PNG encoder a vertically mirrored frame.
    info.bmiHeader.biHeight = -height
    info.bmiHeader.biPlanes = 1
    info.bmiHeader.biBitCount = 32
    info.bmiHeader.biCompression = BI_RGB
    return info


class Win32API:
    """Thin façade over user32/gdi32 so the backend can be tested off Windows."""

    def __init__(self):
        if not hasattr(ctypes, "WinDLL"):
            raise BackendUnavailable(
                "the windows backend requires the Win32 API (ctypes.WinDLL), "
                "which exists only on Windows")
        self._user32 = ctypes.WinDLL("user32", use_last_error=True)
        self._gdi32 = ctypes.WinDLL("gdi32", use_last_error=True)
        self._user32.SendInput.argtypes = [ctypes.c_uint32, ctypes.c_void_p, ctypes.c_int32]
        self._user32.SendInput.restype = ctypes.c_uint32
        self._user32.GetSystemMetrics.argtypes = [ctypes.c_int32]
        self._user32.GetSystemMetrics.restype = ctypes.c_int32
        self._user32.VkKeyScanW.argtypes = [ctypes.c_wchar]
        self._user32.VkKeyScanW.restype = ctypes.c_short
        self._user32.GetDC.argtypes = [ctypes.c_void_p]
        self._user32.GetDC.restype = ctypes.c_void_p
        self._user32.ReleaseDC.argtypes = [ctypes.c_void_p, ctypes.c_void_p]
        self._user32.GetForegroundWindow.restype = ctypes.c_void_p
        self._user32.IsWindowVisible.argtypes = [ctypes.c_void_p]
        self._user32.GetWindowTextLengthW.argtypes = [ctypes.c_void_p]
        self._user32.GetWindowTextW.argtypes = [ctypes.c_void_p, ctypes.c_wchar_p, ctypes.c_int32]
        self._gdi32.CreateCompatibleDC.argtypes = [ctypes.c_void_p]
        self._gdi32.CreateCompatibleDC.restype = ctypes.c_void_p
        self._gdi32.CreateCompatibleBitmap.argtypes = [ctypes.c_void_p, ctypes.c_int32,
                                                       ctypes.c_int32]
        self._gdi32.CreateCompatibleBitmap.restype = ctypes.c_void_p
        self._gdi32.SelectObject.argtypes = [ctypes.c_void_p, ctypes.c_void_p]
        self._gdi32.SelectObject.restype = ctypes.c_void_p
        self._gdi32.BitBlt.argtypes = [ctypes.c_void_p, ctypes.c_int32, ctypes.c_int32,
                                       ctypes.c_int32, ctypes.c_int32, ctypes.c_void_p,
                                       ctypes.c_int32, ctypes.c_int32, DWORD]
        self._gdi32.GetDIBits.argtypes = [ctypes.c_void_p, ctypes.c_void_p, ctypes.c_uint32,
                                          ctypes.c_uint32, ctypes.c_void_p,
                                          ctypes.POINTER(BITMAPINFO), ctypes.c_uint32]
        self._gdi32.DeleteObject.argtypes = [ctypes.c_void_p]
        self._gdi32.DeleteDC.argtypes = [ctypes.c_void_p]

    def set_dpi_awareness(self):
        """Make GetSystemMetrics and SendInput agree with the framebuffer.

        A DPI-unaware process is handed virtualized coordinates, so a click at
        the framebuffer pixel the model saw would land somewhere else on a
        scaled display.
        """
        setter = getattr(self._user32, "SetProcessDpiAwarenessContext", None)
        if setter is not None:
            setter.argtypes = [ctypes.c_void_p]
            if setter(ctypes.c_void_p(DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2)):
                return "per-monitor-v2"
        legacy = getattr(self._user32, "SetProcessDPIAware", None)
        if legacy is not None and legacy():
            return "system"
        return "none"

    def send_input(self, inputs):
        sent = self._user32.SendInput(len(inputs), ctypes.byref(inputs),
                                      ctypes.sizeof(INPUT))
        if sent != len(inputs):
            raise AgentError(500, f"SendInput delivered {sent} of {len(inputs)} events "
                                  f"(error {ctypes.get_last_error()})")
        return sent

    def screen_size(self):
        return (int(self._user32.GetSystemMetrics(SM_CXSCREEN)),
                int(self._user32.GetSystemMetrics(SM_CYSCREEN)))

    def vk_key_scan(self, character):
        return int(self._user32.VkKeyScanW(character))

    def get_dc(self, window):
        return self._user32.GetDC(window)

    def release_dc(self, window, dc):
        self._user32.ReleaseDC(window, dc)

    def create_compatible_dc(self, dc):
        return self._gdi32.CreateCompatibleDC(dc)

    def create_compatible_bitmap(self, dc, width, height):
        return self._gdi32.CreateCompatibleBitmap(dc, width, height)

    def select_object(self, dc, obj):
        return self._gdi32.SelectObject(dc, obj)

    def bit_blt(self, dest_dc, x, y, width, height, source_dc, source_x, source_y):
        if not self._gdi32.BitBlt(dest_dc, x, y, width, height, source_dc,
                                  source_x, source_y, SRCCOPY):
            raise AgentError(500, f"BitBlt failed (error {ctypes.get_last_error()})")

    def get_di_bits(self, dc, bitmap, width, height):
        info = dib_header(width, height)
        buffer = ctypes.create_string_buffer(width * height * 4)
        copied = self._gdi32.GetDIBits(dc, bitmap, 0, height, buffer,
                                       ctypes.byref(info), DIB_RGB_COLORS)
        if copied != height:
            raise AgentError(500, f"GetDIBits copied {copied} of {height} scanlines")
        return buffer.raw

    def delete_object(self, obj):
        self._gdi32.DeleteObject(obj)

    def delete_dc(self, dc):
        self._gdi32.DeleteDC(dc)

    def get_foreground_window(self):
        return self._user32.GetForegroundWindow()

    def is_window_visible(self, hwnd):
        return bool(self._user32.IsWindowVisible(hwnd))

    def get_window_text(self, hwnd):
        length = int(self._user32.GetWindowTextLengthW(hwnd))
        if length <= 0:
            return ""
        buffer = ctypes.create_unicode_buffer(length + 1)
        self._user32.GetWindowTextW(hwnd, buffer, length + 1)
        return buffer.value

    def enum_window_handles(self):
        handles = []
        callback_type = ctypes.WINFUNCTYPE(ctypes.c_int32, ctypes.c_void_p, ctypes.c_void_p)

        def collect(hwnd, _param):
            handles.append(int(hwnd))
            return 1

        self._user32.EnumWindows(callback_type(collect), None)
        return handles


class WindowsBackend(DesktopBackend):
    """Windows session driven by SendInput, GDI and the window list APIs."""

    name = "windows"

    def __init__(self, api=None, type_delay_ms=DEFAULT_TYPE_DELAY_MS,
                 spawn=spawn_detached_windows, sleep=time.sleep):
        self.api = Win32API() if api is None else api
        self.type_delay_ms = type_delay_ms
        self.spawn = spawn
        self.sleep = sleep
        # Declared once at startup: the awareness of a process cannot be raised
        # after the first window or metric query in some Windows builds.
        self.dpi_awareness = self.api.set_dpi_awareness()

    # -- input helpers -------------------------------------------------

    @staticmethod
    def _mouse_input(flags, dx=0, dy=0, mouse_data=0):
        event = INPUT()
        event.type = INPUT_MOUSE
        event.mi = MOUSEINPUT(dx=dx, dy=dy, mouseData=mouse_data & 0xFFFFFFFF,
                              dwFlags=flags, time=0, dwExtraInfo=0)
        return event

    @staticmethod
    def _key_input(vk, scan, flags):
        event = INPUT()
        event.type = INPUT_KEYBOARD
        event.ki = KEYBDINPUT(wVk=vk, wScan=scan, dwFlags=flags, time=0, dwExtraInfo=0)
        return event

    def _send(self, events):
        array = (INPUT * len(events))(*events)
        self.api.send_input(array)

    def _absolute(self, x, y):
        """Map a framebuffer pixel onto the 0..65535 absolute input range.

        The range is inclusive, so dividing by the pixel count instead of the
        last pixel index would make the rightmost column unreachable.
        """
        width, height = self.geometry()
        dx = 0 if width <= 1 else round(x * ABSOLUTE_RANGE / (width - 1))
        dy = 0 if height <= 1 else round(y * ABSOLUTE_RANGE / (height - 1))
        return max(0, min(ABSOLUTE_RANGE, dx)), max(0, min(ABSOLUTE_RANGE, dy))

    def _vk_flags(self, vk):
        return KEYEVENTF_EXTENDEDKEY if vk in WINDOWS_EXTENDED_VK else 0

    def _resolve_key(self, key):
        """Return (virtual-key, implied modifier virtual-keys) for one key token."""
        lowered = key.lower()
        if lowered in WINDOWS_NAMED_KEYS:
            return WINDOWS_NAMED_KEYS[lowered], []
        function_key = FUNCTION_KEY_RE.match(lowered)
        if function_key:
            return 0x70 + int(function_key.group(1)) - 1, []
        if len(key) == 1:
            scan = self.api.vk_key_scan(key)
            if scan < 0:
                raise AgentError(500, f"the keyboard layout cannot produce {key!r}")
            implied = []
            shift_state = (scan >> 8) & 0xFF
            if shift_state & 0x01:
                implied.append(WINDOWS_MODIFIER_VK["shift"])
            if shift_state & 0x02:
                implied.append(WINDOWS_MODIFIER_VK["ctrl"])
            if shift_state & 0x04:
                implied.append(WINDOWS_MODIFIER_VK["alt"])
            return scan & 0xFF, implied
        raise AgentError(500, f"the windows backend does not support key {key!r}")

    # -- backend interface ---------------------------------------------

    def geometry(self):
        width, height = self.api.screen_size()
        if width <= 0 or height <= 0:
            raise AgentError(500, f"unexpected primary display size {width}x{height}")
        return width, height

    def click(self, x, y, button, clicks):
        down, up = WINDOWS_BUTTON_FLAGS[button]
        dx, dy = self._absolute(x, y)
        events = [self._mouse_input(MOUSEEVENTF_MOVE | MOUSEEVENTF_ABSOLUTE, dx, dy)]
        for _ in range(clicks):
            events.append(self._mouse_input(down))
            events.append(self._mouse_input(up))
        self._send(events)

    def type_text(self, text):
        for character in text:
            # KEYEVENTF_UNICODE injects a character, not a keystroke: control
            # characters would be swallowed, so they go through their keys.
            if character == "\r":
                continue
            if character == "\n":
                self.press_key([], "enter")
            elif character == "\t":
                self.press_key([], "tab")
            else:
                units = character.encode("utf-16-le")
                events = []
                for offset in range(0, len(units), 2):
                    unit = struct.unpack_from("<H", units, offset)[0]
                    events.append(self._key_input(0, unit, KEYEVENTF_UNICODE))
                    events.append(self._key_input(0, unit, KEYEVENTF_UNICODE | KEYEVENTF_KEYUP))
                self._send(events)
            if self.type_delay_ms > 0:
                self.sleep(self.type_delay_ms / 1000.0)

    def press_key(self, modifiers, key):
        try:
            held = [WINDOWS_MODIFIER_VK[modifier] for modifier in modifiers]
        except KeyError as exc:
            raise AgentError(500, f"the windows backend does not support modifier "
                                  f"{exc.args[0]!r}") from None
        vk, implied = self._resolve_key(key)
        held += [modifier for modifier in implied if modifier not in held]
        events = [self._key_input(modifier, 0, self._vk_flags(modifier)) for modifier in held]
        events.append(self._key_input(vk, 0, self._vk_flags(vk)))
        events.append(self._key_input(vk, 0, self._vk_flags(vk) | KEYEVENTF_KEYUP))
        events += [self._key_input(modifier, 0, self._vk_flags(modifier) | KEYEVENTF_KEYUP)
                   for modifier in reversed(held)]
        self._send(events)
        return "+".join(list(modifiers) + [key])

    def scroll(self, x, y, wheels):
        dx, dy = self._absolute(x, y)
        self._send([self._mouse_input(MOUSEEVENTF_MOVE | MOUSEEVENTF_ABSOLUTE, dx, dy)])
        for direction, steps in wheels:
            flag, delta = WINDOWS_WHEEL[direction]
            for _ in range(steps):
                self._send([self._mouse_input(flag, mouse_data=delta)])

    def screenshot(self):
        width, height = self.geometry()
        screen_dc = self.api.get_dc(None)
        if not screen_dc:
            raise AgentError(500, "GetDC returned no device context for the screen")
        memory_dc = None
        bitmap = None
        previous = None
        try:
            memory_dc = self.api.create_compatible_dc(screen_dc)
            bitmap = self.api.create_compatible_bitmap(screen_dc, width, height)
            if not memory_dc or not bitmap:
                raise AgentError(500, "GDI refused to allocate a capture bitmap")
            previous = self.api.select_object(memory_dc, bitmap)
            self.api.bit_blt(memory_dc, 0, 0, width, height, screen_dc, 0, 0)
            raw = self.api.get_di_bits(memory_dc, bitmap, width, height)
        finally:
            # GDI handles are a per-session quota; leaking them on a failed
            # capture would eventually break the whole desktop session.
            if previous is not None and memory_dc:
                self.api.select_object(memory_dc, previous)
            if bitmap:
                self.api.delete_object(bitmap)
            if memory_dc:
                self.api.delete_dc(memory_dc)
            self.api.release_dc(None, screen_dc)
        return width, height, encode_png(width, height, bgrx_to_rgb(raw, width, height))

    def list_windows(self):
        foreground = self.api.get_foreground_window()
        rows = []
        for hwnd in self.api.enum_window_handles():
            if not self.api.is_window_visible(hwnd):
                continue
            title = self.api.get_window_text(hwnd)
            if not title:
                continue
            rows.append({"id": f"0x{hwnd:08x}", "desktop": 0, "title": title})
        # Windows has no desktop index to sort on, so the active window leads
        # the list to give the model the same first entry a human would see.
        rows.sort(key=lambda row: row["id"] != f"0x{int(foreground or 0):08x}")
        return rows

    def launch(self, app):
        command = list(WINDOWS_APPS[app]) if app in WINDOWS_APPS else _resolve_path_executable(app)
        self.spawn(command)
        return command


# ----------------------------------------------------------------------
# macOS backend
# ----------------------------------------------------------------------

MACOS_MODIFIER_FLAGS = {
    "ctrl": "kCGEventFlagMaskControl",
    "alt": "kCGEventFlagMaskAlternate",
    "shift": "kCGEventFlagMaskShift",
    "super": "kCGEventFlagMaskCommand",
}
MACOS_BUTTONS = {
    "left": ("kCGEventLeftMouseDown", "kCGEventLeftMouseUp", "kCGMouseButtonLeft"),
    "middle": ("kCGEventOtherMouseDown", "kCGEventOtherMouseUp", "kCGMouseButtonCenter"),
    "right": ("kCGEventRightMouseDown", "kCGEventRightMouseUp", "kCGMouseButtonRight"),
}
MACOS_NAMED_KEYS = {
    "enter": 0x24,
    "esc": 0x35,
    "tab": 0x30,
    "space": 0x31,
    "backspace": 0x33,
    "delete": 0x75,
    # Apple keyboards have no Insert key; 0x72 is the key in that position.
    "insert": 0x72,
    "home": 0x73,
    "end": 0x77,
    "pageup": 0x74,
    "pagedown": 0x79,
    "up": 0x7E,
    "down": 0x7D,
    "left": 0x7B,
    "right": 0x7C,
}
MACOS_FUNCTION_KEYS = {
    1: 0x7A, 2: 0x78, 3: 0x63, 4: 0x76, 5: 0x60, 6: 0x61,
    7: 0x62, 8: 0x64, 9: 0x65, 10: 0x6D, 11: 0x67, 12: 0x6F,
    13: 0x69, 14: 0x6B, 15: 0x71, 16: 0x6A, 17: 0x40, 18: 0x4F,
    19: 0x50, 20: 0x5A,
}
# US layout virtual key codes: a shortcut needs a real key code, and unicode
# injection cannot express one.
MACOS_KEY_CODES = {
    "a": 0x00, "b": 0x0B, "c": 0x08, "d": 0x02, "e": 0x0E, "f": 0x03, "g": 0x05,
    "h": 0x04, "i": 0x22, "j": 0x26, "k": 0x28, "l": 0x25, "m": 0x2E, "n": 0x2D,
    "o": 0x1F, "p": 0x23, "q": 0x0C, "r": 0x0F, "s": 0x01, "t": 0x11, "u": 0x20,
    "v": 0x09, "w": 0x0D, "x": 0x07, "y": 0x10, "z": 0x06,
    "0": 0x1D, "1": 0x12, "2": 0x13, "3": 0x14, "4": 0x15, "5": 0x17,
    "6": 0x16, "7": 0x1A, "8": 0x1C, "9": 0x19,
    "-": 0x1B, "=": 0x18, "[": 0x21, "]": 0x1E, "\\": 0x2A, ";": 0x29,
    "'": 0x27, ",": 0x2B, ".": 0x2F, "/": 0x2C, "`": 0x32,
}
MACOS_WHEEL_AXIS = {"up": (1, 1), "down": (1, -1), "right": (2, 1), "left": (2, -1)}
MACOS_APPS = {
    "browser": ["open", "-a", "Safari"],
    "firefox": ["open", "-a", "Firefox"],
    "terminal": ["open", "-a", "Terminal"],
    "files": ["open", "-a", "Finder"],
    "editor": ["open", "-a", "TextEdit"],
}


def _import_quartz():
    import Quartz  # noqa: PLC0415 - imported lazily so other backends need no pyobjc

    return Quartz


def _display_mode_size(quartz, getter_name, mode, fallback):
    """Read one dimension of a CGDisplayMode, tolerating an older pyobjc.

    CGDisplayModeGetPixelWidth/Height arrived in macOS 10.8 and are absent from
    some pyobjc builds, and a mode that answers 0 knows no backing size. Both
    cases fall back to the point size rather than scaling every coordinate by a
    bogus factor.
    """
    getter = getattr(quartz, getter_name, None)
    if getter is None:
        return fallback
    value = int(getter(mode) or 0)
    return value if value > 0 else fallback


class MacOSBackend(DesktopBackend):
    """macOS session driven by Quartz event taps and screencapture."""

    name = "macos"

    def __init__(self, quartz=None, type_delay_ms=DEFAULT_TYPE_DELAY_MS,
                 screencapture="screencapture", spawn=spawn_detached_posix,
                 runner=_run_tool, sleep=time.sleep):
        if quartz is None:
            try:
                quartz = _import_quartz()
            except ImportError as exc:
                raise BackendUnavailable(
                    "the macos backend requires pyobjc-framework-Quartz "
                    f"(import Quartz failed: {exc})") from exc
        self.quartz = quartz
        self.type_delay_ms = type_delay_ms
        self.screencapture = screencapture
        self.spawn = spawn
        self.runner = runner
        self.sleep = sleep

    def _post(self, event):
        self.quartz.CGEventPost(self.quartz.kCGHIDEventTap, event)

    def _flags(self, modifiers):
        flags = 0
        for modifier in modifiers:
            name = MACOS_MODIFIER_FLAGS.get(modifier)
            if name is None:
                raise AgentError(500, f"the macos backend does not support modifier "
                                      f"{modifier!r}")
            flags |= getattr(self.quartz, name)
        return flags

    def _key_code(self, key):
        lowered = key.lower()
        if lowered in MACOS_NAMED_KEYS:
            return MACOS_NAMED_KEYS[lowered], False
        function_key = FUNCTION_KEY_RE.match(lowered)
        if function_key:
            number = int(function_key.group(1))
            if number in MACOS_FUNCTION_KEYS:
                return MACOS_FUNCTION_KEYS[number], False
        if len(key) == 1 and lowered in MACOS_KEY_CODES:
            return MACOS_KEY_CODES[lowered], key != lowered
        raise AgentError(500, f"the macos backend does not support key {key!r}")

    def _display_metrics(self):
        """Return (pixel_width, pixel_height, scale_x, scale_y) of the main display.

        screencapture writes the backing store, so the frame the model reasons
        about is in pixels, while CGEventCreateMouseEvent consumes points.
        CGDisplayPixelsWide/High report points despite their name (they are the
        display mode's width, not its pixel width), so publishing them as the
        screen size would reject every coordinate in the right and bottom halves
        of a Retina desktop and would land each accepted click at a fraction of
        its intended position. The pixel size is therefore authoritative and
        coordinates are divided back into points on the way out.
        """
        quartz = self.quartz
        display = quartz.CGMainDisplayID()
        point_width = int(quartz.CGDisplayPixelsWide(display))
        point_height = int(quartz.CGDisplayPixelsHigh(display))
        if point_width <= 0 or point_height <= 0:
            raise AgentError(500, f"the main display reports an empty geometry "
                                  f"({point_width}x{point_height})")
        pixel_width, pixel_height = point_width, point_height
        copy_mode = getattr(quartz, "CGDisplayCopyDisplayMode", None)
        mode = copy_mode(display) if copy_mode is not None else None
        if mode is not None:
            pixel_width = _display_mode_size(quartz, "CGDisplayModeGetPixelWidth",
                                             mode, point_width)
            pixel_height = _display_mode_size(quartz, "CGDisplayModeGetPixelHeight",
                                             mode, point_height)
        return (pixel_width, pixel_height,
                pixel_width / point_width, pixel_height / point_height)

    def geometry(self):
        pixel_width, pixel_height, _scale_x, _scale_y = self._display_metrics()
        return pixel_width, pixel_height

    def _point(self, x, y):
        """Map a frame pixel coordinate onto the point coordinate CGEvent takes."""
        _width, _height, scale_x, scale_y = self._display_metrics()
        return (x / scale_x, y / scale_y)

    def click(self, x, y, button, clicks):
        down_name, up_name, button_name = MACOS_BUTTONS[button]
        quartz = self.quartz
        button_id = getattr(quartz, button_name)
        position = self._point(x, y)
        self._post(quartz.CGEventCreateMouseEvent(None, quartz.kCGEventMouseMoved,
                                                  position, button_id))
        for index in range(clicks):
            for name in (down_name, up_name):
                event = quartz.CGEventCreateMouseEvent(None, getattr(quartz, name),
                                                       position, button_id)
                # Without an increasing click state the second press is a second
                # single click and applications never see a double click.
                quartz.CGEventSetIntegerValueField(event, quartz.kCGMouseEventClickState,
                                                   index + 1)
                self._post(event)

    def type_text(self, text):
        quartz = self.quartz
        for character in text:
            if character == "\r":
                continue
            if character == "\n":
                self.press_key([], "enter")
            elif character == "\t":
                self.press_key([], "tab")
            else:
                for down in (True, False):
                    event = quartz.CGEventCreateKeyboardEvent(None, 0, down)
                    quartz.CGEventKeyboardSetUnicodeString(event, len(character), character)
                    self._post(event)
            if self.type_delay_ms > 0:
                self.sleep(self.type_delay_ms / 1000.0)

    def press_key(self, modifiers, key):
        quartz = self.quartz
        key_code, needs_shift = self._key_code(key)
        flags = self._flags(modifiers)
        if needs_shift:
            flags |= quartz.kCGEventFlagMaskShift
        for down in (True, False):
            event = quartz.CGEventCreateKeyboardEvent(None, key_code, down)
            quartz.CGEventSetFlags(event, flags)
            self._post(event)
        return "+".join(list(modifiers) + [key])

    def scroll(self, x, y, wheels):
        quartz = self.quartz
        self._post(quartz.CGEventCreateMouseEvent(None, quartz.kCGEventMouseMoved,
                                                  self._point(x, y),
                                                  quartz.kCGMouseButtonLeft))
        for direction, steps in wheels:
            axis, sign = MACOS_WHEEL_AXIS[direction]
            for _ in range(steps):
                vertical = sign * SCROLL_STEP_PX if axis == 1 else 0
                horizontal = sign * SCROLL_STEP_PX if axis == 2 else 0
                self._post(quartz.CGEventCreateScrollWheelEvent(
                    None, quartz.kCGScrollEventUnitPixel, 2, vertical, horizontal))

    def screenshot(self):
        width, height = self.geometry()
        path = _unique_temp_png()
        try:
            self.runner([self.screencapture, "-x", "-t", "png", path])
            with open(path, "rb") as fh:
                return width, height, fh.read()
        except OSError as exc:
            raise AgentError(500, f"screencapture produced no frame: {exc}") from exc
        finally:
            try:
                os.unlink(path)
            except FileNotFoundError:
                pass

    def list_windows(self):
        quartz = self.quartz
        options = (quartz.kCGWindowListOptionOnScreenOnly
                   | quartz.kCGWindowListExcludeDesktopElements)
        rows = []
        for entry in quartz.CGWindowListCopyWindowInfo(options, quartz.kCGNullWindowID) or []:
            # Layer 0 is the normal application layer; the menu bar, the Dock
            # and status items sit above it and are not windows a model acts on.
            if int(entry.get("kCGWindowLayer", 0)) != 0:
                continue
            # kCGWindowName is empty unless the agent holds the screen
            # recording permission, so the owning application names the window.
            title = entry.get("kCGWindowName") or entry.get("kCGWindowOwnerName") or ""
            if not title:
                continue
            rows.append({"id": f"0x{int(entry.get('kCGWindowNumber', 0)):08x}",
                         "desktop": 0, "title": str(title)})
        return rows

    def launch(self, app):
        command = list(MACOS_APPS[app]) if app in MACOS_APPS else _resolve_path_executable(app)
        self.spawn(command)
        return command


# ----------------------------------------------------------------------
# Backend selection
# ----------------------------------------------------------------------


def os_name(platform=None):
    """Report the guest operating system family for /health."""
    platform = sys.platform if platform is None else platform
    if platform.startswith("win"):
        return "windows"
    if platform == "darwin":
        return "darwin"
    return "linux"


def resolve_backend_name(requested, platform=None):
    platform = sys.platform if platform is None else platform
    requested = (requested or "auto").strip().lower()
    if requested == "auto":
        if platform.startswith("win"):
            return "windows"
        if platform == "darwin":
            return "macos"
        return "x11"
    if requested not in BACKEND_NAMES:
        raise SystemExit(f"desktop-agent: DESKTOP_AGENT_BACKEND must be auto or one of "
                         f"{', '.join(BACKEND_NAMES)}, got {requested!r}")
    return requested


def build_backend(name, env=None):
    env = os.environ if env is None else env
    type_delay_ms = int(env.get("DESKTOP_AGENT_TYPE_DELAY_MS", str(DEFAULT_TYPE_DELAY_MS)))
    if name == "x11":
        return X11Backend(
            xdotool=env.get("DESKTOP_AGENT_XDOTOOL", "xdotool"),
            scrot=env.get("DESKTOP_AGENT_SCROT", "scrot"),
            wmctrl=env.get("DESKTOP_AGENT_WMCTRL", "wmctrl"),
            type_delay_ms=type_delay_ms,
        )
    if name == "windows":
        return WindowsBackend(type_delay_ms=type_delay_ms)
    if name == "macos":
        return MacOSBackend(type_delay_ms=type_delay_ms)
    raise BackendUnavailable(f"unknown backend {name!r}")


def create_backend_or_exit(name, env=None):
    """Build the backend or stop the process naming the missing dependency.

    Exiting is the only honest option: an agent that answers /health while it
    cannot deliver input would make the Gateway report a healthy desktop.
    """
    try:
        return build_backend(name, env)
    except BackendUnavailable as exc:
        print(f"desktop-agent: {exc}", file=sys.stderr, flush=True)
        raise SystemExit(2) from exc


# ----------------------------------------------------------------------
# Portable core
# ----------------------------------------------------------------------


class DesktopAgent:
    """Validates computer-use actions and applies them through a backend."""

    def __init__(self, token, backend):
        self.token = token
        self.backend = backend
        self.lock = threading.Lock()

    def _require_on_screen(self, x, y, geometry=None):
        width, height = geometry if geometry is not None else self.backend.geometry()
        if not (0 <= x < width and 0 <= y < height):
            raise AgentError(400, f"coordinates ({x},{y}) exceeds screen {width}x{height}")

    # -- endpoints -----------------------------------------------------

    def health(self):
        width, height = self.backend.geometry()
        return {"ok": True, "agent": AGENT_VERSION, "backend": self.backend.name,
                "os": os_name(), "screen": {"width": width, "height": height}}

    def screenshot(self):
        return self.backend.screenshot()

    def windows(self):
        return {"windows": self.backend.list_windows()}

    # -- action validation ---------------------------------------------

    def _prepare_click(self, payload, geometry=None):
        x = _int_field(payload, "x", minimum=0)
        y = _int_field(payload, "y", minimum=0)
        button = payload.get("button")
        if button not in BUTTONS:
            raise AgentError(400, "button must be one of left, middle, right")
        clicks = payload.get("clicks", 1)
        # `1.0 in (1, 2)` is True, so a JSON float would reach range(clicks) in
        # the Windows and macOS backends and raise TypeError there instead of
        # being rejected here. The same integer rule as x and y is applied.
        if isinstance(clicks, bool) or not isinstance(clicks, int) or clicks not in (1, 2):
            raise AgentError(400, "clicks must be 1 or 2")
        self._require_on_screen(x, y, geometry)
        return {"type": "click", "x": x, "y": y, "button": button, "clicks": clicks}

    @staticmethod
    def _prepare_type(payload):
        text = payload.get("text")
        if not isinstance(text, str):
            raise AgentError(400, "text must be a string")
        if not text or len(text) > MAX_TEXT_CHARS:
            raise AgentError(400, f"text must contain 1 to {MAX_TEXT_CHARS} characters")
        return {"type": "type", "text": text}

    @staticmethod
    def _prepare_key(payload):
        spec = payload.get("key")
        if not isinstance(spec, str) or len(spec) > MAX_KEY_CHARS or not KEY_SPEC_RE.match(spec):
            raise AgentError(400, f"key must match [A-Za-z0-9+_-]+ and be at most "
                                  f"{MAX_KEY_CHARS} characters")
        tokens = spec.split("+")
        if any(token == "" for token in tokens):
            raise AgentError(400, "key contains an empty + separated token")
        modifiers = [MODIFIER_KEYS.get(token.lower(), token.lower()) for token in tokens[:-1]]
        last = tokens[-1]
        return {"type": "key", "modifiers": modifiers,
                "key": KEY_ALIASES.get(last.lower(), last)}

    def _prepare_scroll(self, payload, geometry=None):
        x = _int_field(payload, "x", minimum=0)
        y = _int_field(payload, "y", minimum=0)
        delta_x = _int_field(payload, "deltaX", minimum=-MAX_DELTA, maximum=MAX_DELTA)
        delta_y = _int_field(payload, "deltaY", minimum=-MAX_DELTA, maximum=MAX_DELTA)
        self._require_on_screen(x, y, geometry)
        wheels = []
        if delta_y != 0:
            wheels.append(("down" if delta_y > 0 else "up", self._wheel_steps(delta_y)))
        if delta_x != 0:
            wheels.append(("right" if delta_x > 0 else "left", self._wheel_steps(delta_x)))
        return {"type": "scroll", "x": x, "y": y, "wheels": wheels}

    def _prepare_batch_action(self, payload, geometry):
        if not isinstance(payload, dict):
            raise AgentError(400, "each action must be an object")
        action_type = payload.get("type")
        if action_type == "click":
            return self._prepare_click(payload, geometry)
        if action_type == "type":
            return self._prepare_type(payload)
        if action_type == "key":
            return self._prepare_key(payload)
        if action_type == "scroll":
            return self._prepare_scroll(payload, geometry)
        raise AgentError(400, "action type must be one of click, type, key, scroll")

    @staticmethod
    def _wheel_steps(delta):
        return max(1, min(MAX_SCROLL_STEPS, round(abs(delta) / SCROLL_STEP_PX)))

    # -- action execution ----------------------------------------------

    def _apply_prepared_action(self, action):
        action_type = action["type"]
        if action_type == "click":
            self.backend.click(action["x"], action["y"], action["button"], action["clicks"])
            return None
        if action_type == "type":
            self.backend.type_text(action["text"])
            return None
        if action_type == "key":
            return self.backend.press_key(action["modifiers"], action["key"])
        if action_type == "scroll":
            self.backend.scroll(action["x"], action["y"], action["wheels"])
            return None
        raise AgentError(500, f"unsupported prepared action: {action_type!r}")

    def click(self, payload):
        action = self._prepare_click(payload)
        self._apply_prepared_action(action)
        return {"applied": True, "x": action["x"], "y": action["y"],
                "button": action["button"], "clicks": action["clicks"]}

    def type_text(self, payload):
        action = self._prepare_type(payload)
        self._apply_prepared_action(action)
        return {"applied": True, "characters": len(action["text"])}

    def press_key(self, payload):
        action = self._prepare_key(payload)
        applied = self._apply_prepared_action(action)
        return {"applied": True, "key": applied}

    def scroll(self, payload):
        action = self._prepare_scroll(payload)
        self._apply_prepared_action(action)
        return {"applied": True}

    def action_batch(self, payload):
        actions = payload.get("actions")
        if not isinstance(actions, list):
            raise AgentError(400, "actions must be an array")
        if not 1 <= len(actions) <= MAX_ACTIONS_PER_BATCH:
            raise AgentError(
                400,
                f"actions must contain 1 to {MAX_ACTIONS_PER_BATCH} entries",
            )
        total_text_chars = sum(
            len(action["text"])
            for action in actions
            if isinstance(action, dict)
            and action.get("type") == "type"
            and isinstance(action.get("text"), str)
        )
        if total_text_chars > MAX_TEXT_CHARS:
            raise AgentError(
                400,
                f"action batch may type at most {MAX_TEXT_CHARS} characters",
            )
        needs_geometry = any(
            isinstance(action, dict) and action.get("type") in ("click", "scroll")
            for action in actions
        )
        geometry = self.backend.geometry() if needs_geometry else None
        prepared = [self._prepare_batch_action(action, geometry) for action in actions]

        for index, action in enumerate(prepared):
            try:
                self._apply_prepared_action(action)
            except Exception as exc:  # noqa: BLE001
                # A backend can fail outside AgentError - a ctypes or Quartz
                # call raising TypeError, for example. Letting that escape gives
                # the Gateway a 5xx with no outcome, which its contract reads as
                # "nothing was applied"; that is false the moment an earlier
                # action has already reached the desktop, so every failure is
                # reported as Partial with the count that did land.
                message = exc.message if isinstance(exc, AgentError) else f"internal error: {exc}"
                return {
                    "applied": False,
                    "outcome": "Partial",
                    "retrySafe": False,
                    "actionCount": len(prepared),
                    "completedActions": index,
                    "failedActionIndex": index,
                    "failedActionType": action["type"],
                    "error": message,
                }
        return {
            "applied": True,
            "outcome": "Applied",
            "actionCount": len(prepared),
            "completedActions": len(prepared),
        }

    def launch(self, payload):
        app = payload.get("app")
        if not isinstance(app, str):
            raise AgentError(400, "app must be a string")
        return {"applied": True, "command": self.backend.launch(app)}

    def handle_action(self, path, payload):
        if path == "/actions":
            return self.action_batch(payload)
        if path == "/launch":
            return self.launch(payload)
        raise AgentError(404, f"not found: {path}")


# ----------------------------------------------------------------------
# HTTP layer
# ----------------------------------------------------------------------


class AgentHTTPRequestHandler(BaseHTTPRequestHandler):
    server_version = AGENT_VERSION

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


# ----------------------------------------------------------------------
# Startup
# ----------------------------------------------------------------------


def default_token_file(platform=None, env=None):
    platform = sys.platform if platform is None else platform
    env = os.environ if env is None else env
    if platform.startswith("win"):
        # ntpath, not os.path: the guest path is a Windows path even when this
        # branch is exercised from a test on another operating system.
        return ntpath.join(env.get("ProgramData", r"C:\ProgramData"), WINDOWS_TOKEN_FILE)
    return POSIX_TOKEN_FILE


def load_token(path):
    try:
        with open(path, "r", encoding="utf-8") as fh:
            token = fh.readline().strip()
    except OSError as exc:
        raise SystemExit(f"desktop-agent: cannot read token file {path}: {exc}")
    if len(token) < MIN_TOKEN_CHARS:
        raise SystemExit(f"desktop-agent: token in {path} is shorter than "
                         f"{MIN_TOKEN_CHARS} characters")
    return token


def resolve_token(env=None, platform=None):
    """Take the token from the environment, else from the token file."""
    env = os.environ if env is None else env
    inline = env.get("DESKTOP_AGENT_TOKEN")
    if inline:
        token = inline.strip()
        if len(token) < MIN_TOKEN_CHARS:
            raise SystemExit(f"desktop-agent: DESKTOP_AGENT_TOKEN is shorter than "
                             f"{MIN_TOKEN_CHARS} characters")
        return token
    return load_token(env.get("DESKTOP_AGENT_TOKEN_FILE")
                      or default_token_file(platform=platform, env=env))


def main():
    env = os.environ
    # The backend is resolved first so a guest image that is missing a
    # dependency says so even when the token has not been delivered yet.
    backend_name = resolve_backend_name(env.get("DESKTOP_AGENT_BACKEND", "auto"))
    backend = create_backend_or_exit(backend_name, env)
    token = resolve_token(env)
    agent = DesktopAgent(token=token, backend=backend)
    bind = env.get("DESKTOP_AGENT_BIND", DEFAULT_BIND)
    port = int(env.get("DESKTOP_AGENT_PORT", str(DEFAULT_PORT)))
    server = AgentHTTPServer((bind, port), agent)
    print(f"desktop-agent: {AGENT_VERSION} backend={backend.name} os={os_name()} "
          f"listening on {bind}:{port}", file=sys.stderr, flush=True)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()


if __name__ == "__main__":
    main()
