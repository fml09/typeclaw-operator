// Package guest carries the material a Personal Desktop virtual machine needs
// to bring up its Guest Desktop Agent: the agent itself, the Linux session
// script, and the Windows answer file and first-logon bootstrap.
//
// Everything here is compiled into the operator image and delivered to the
// guest through cloud-init or the sysprep Secret, so a desktop never downloads
// its own agent: the guest has no credential to fetch one with, an air-gapped
// cluster has nowhere to fetch it from, and an agent that arrived over the
// network would be a second, unreviewed supply chain for the component that
// holds the keyboard.
package guest

import _ "embed"

// DesktopAgentSource is the Guest Desktop Agent, written to the guest as
// /usr/local/bin/desktop-agent.py (Linux) or
// C:\ProgramData\PersonalDesktop\desktop_agent.py (Windows).
//
//go:embed desktop-agent/desktop_agent.py
var DesktopAgentSource string

// LinuxSessionScript starts the agent from the XFCE session. The operator
// substitutes @@SCREEN_WIDTH@@ and @@SCREEN_HEIGHT@@ before writing it.
//
//go:embed linux/session-autostart.sh
var LinuxSessionScript string

// WindowsUnattendTemplate is a text/template answer file with the fields
// Username, Password, Width, Height and Hostname.
//
//go:embed windows/unattend.xml.tmpl
var WindowsUnattendTemplate string

// WindowsSetupScript is the first-logon bootstrap. The operator substitutes
// @@PD_PYTHON_URL@@ and @@PD_PYTHON_SHA256@@ before writing it.
//
//go:embed windows/setup.ps1
var WindowsSetupScript string
