package webassets

import "embed"

// Files contains the complete dashboard. It is compiled into the Nexa binary.
//
//go:embed index.html styles.css app.js
var Files embed.FS
