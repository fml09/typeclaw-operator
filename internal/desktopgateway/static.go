package desktopgateway

import "embed"

// consoleUI carries the Desktop Console page. It is one self-contained file so
// the console has no origin to load styles or scripts from beyond noVNC, which
// the image stages locally.
//
//go:embed static/index.html
var consoleUI embed.FS
