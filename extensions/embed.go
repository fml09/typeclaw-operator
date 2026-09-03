// Package extensions carries the Platform Extensions the operator ships inside
// its own image.
//
// A Platform Extension is administrator-owned and immutable: the operator
// renders the embedded source into a ConfigMap and projects it read-only into
// the Managed Runtime, which activates it through TYPECLAW_PLATFORM_EXTENSIONS.
// Nothing is ever written into the Agent Folder, so an agent cannot edit, shadow,
// or delete the extension that grants it desktop control, and upgrading the
// operator image upgrades the extension everywhere at once.
package extensions

import _ "embed"

// PersonalDesktopComputerUse is the TypeClaw plugin that drives one Personal
// Desktop through its Desktop Gateway.
//
//go:embed personal-desktop-computer-use/index.ts
var PersonalDesktopComputerUse string
