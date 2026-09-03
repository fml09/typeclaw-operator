# Guest Desktop Agent

The Guest Desktop Agent is the small HTTP service that runs inside the
interactive desktop session of a Personal Desktop virtual machine. The Desktop
Gateway proxies typed computer-use actions to it and reads frames from it; the
model never talks to this port directly.

Everything in this directory is embedded into the operator image (`embed.go`)
and delivered to the guest through cloud-init (Linux) or the sysprep Secret
(Windows). A desktop never downloads its own agent.

## Protocol

All endpoints require `Authorization: Bearer <guest token>`; the token is read
at startup from `DESKTOP_AGENT_TOKEN_FILE` (default
`/etc/personal-desktop/agent-token`, `%ProgramData%\PersonalDesktop\agent-token`
on Windows) or from `DESKTOP_AGENT_TOKEN`, and must be at least 24 characters.

| Endpoint | Body | Response |
|---|---|---|
| `GET /health` | — | `{"ok":true,"agent":"desktop-agent/2.0","backend":"x11\|windows\|macos","os":"linux\|windows\|darwin","screen":{"width","height"}}` |
| `GET /windows` | — | `{"windows":[{"id","desktop","title"}]}` |
| `POST /screenshot` | — | PNG bytes with `X-Screen-Width` / `X-Screen-Height` |
| `POST /actions` | `{"actions":[…]}` | `{"applied","outcome":"Applied"\|"Partial",…}` |
| `POST /launch` | `{"app":"browser"}` | `{"applied":true,"command":[…]}` |

An action is `click`, `type`, `key` or `scroll`. A batch carries at most 16
actions and 4000 typed characters in total, is validated in full before the
first action is applied, and reports `Partial` with the failed index when an
action fails midway. Key specifications use one vocabulary on every backend
(`ctrl`/`alt`/`shift`/`cmd`+`win`+`meta`+`super`, `enter`, `esc`, `tab`,
`space`, `backspace`, `delete`, `insert`, `home`, `end`, `pageup`, `pagedown`,
the arrow keys, `F1`–`F24` and single characters).

Environment: `DESKTOP_AGENT_BACKEND` (`auto`, `x11`, `windows`, `macos`),
`DESKTOP_AGENT_PORT` (9876), `DESKTOP_AGENT_BIND` (`0.0.0.0`),
`DESKTOP_AGENT_TYPE_DELAY_MS` (6).

## Backends

- **X11** (`x11`) drives `xdotool`, `scrot` and `wmctrl`. `linux/session-autostart.sh`
  is started by an XFCE autostart entry so the agent inherits the session's
  `DISPLAY`; it sets the screen mode, disables blanking and DPMS, then execs
  the agent. The mode and blanking steps need `xrandr` and `xset` from
  `x11-xserver-utils`, which the XFCE metapackage only recommends: the image
  must install it, or `spec.personalDesktop.screen` has no effect and the
  session log says so on every start.
- **Windows** (`windows`) drives `SendInput`, GDI and the window list APIs
  through `ctypes`, and encodes captured frames to PNG with `zlib` alone.
  `windows/unattend.xml.tmpl` creates the autologon user and runs
  `windows/setup.ps1` at first logon, which installs the agent into
  `C:\ProgramData\PersonalDesktop`, ensures Python 3, keeps the session awake
  and unlocked, opens TCP 9876 and registers the `PersonalDesktopAgent` logon
  task.
- **macOS** (`macos`) drives Quartz events and `screencapture`. It needs
  `pyobjc-framework-Quartz`; without it the process exits 2 at startup rather
  than accepting actions it cannot deliver. `/health` and `X-Screen-*` report
  the display's backing **pixels** (`CGDisplayModeGetPixelWidth/Height`),
  because that is what `screencapture` writes; incoming coordinates are
  therefore frame pixels and the backend divides them by the backing scale
  factor before posting a Quartz event, which consumes points.

## Tests

```sh
python3 -m unittest discover -s guest/desktop-agent -v
```

The suite is hermetic: the X11 backend runs against fake tool executables, the
Windows backend against an injected Win32 façade, and the macOS backend against
a fake Quartz module, so it passes on any machine with Python 3.10+.
