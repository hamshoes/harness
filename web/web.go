// Package web holds the page the API serves on its TCP listener: plain HTML, CSS and JavaScript, no build step.
package web

import "embed"

//go:embed index.html app.css app.js upload.js tree.js
var Files embed.FS
