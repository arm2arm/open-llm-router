// Package web embeds the portal built by npm run build. The placeholder keeps
// API-only Go development possible before the first frontend build.
package web

import "embed"

//go:embed all:dist
var Assets embed.FS
