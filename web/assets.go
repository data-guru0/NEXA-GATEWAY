package webassets

import "embed"

// Files contains the complete dashboard. It is compiled into the Nexa binary.
//
//go:embed index.html styles.css app.js fonts/inter.woff2 fonts/OFL.txt
var Files embed.FS
