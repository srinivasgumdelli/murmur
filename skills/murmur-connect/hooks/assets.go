package hooks

import "embed"

// Assets contains the portable Claude Code hook scripts and settings.
//
//go:embed *.sh hooks.json
var Assets embed.FS
