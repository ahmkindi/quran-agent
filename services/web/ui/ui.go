// Package ui embeds the browser client assets into the binary.
package ui

import "embed"

//go:embed index.html pion.html livekit.html app.js livekit.js hud.js star.js style.css fonts/*.woff2
var FS embed.FS
