// Package firstparty embeds the first-party apps, so they ship inside the servicelater binary and install and
// upgrade with it. The folders next to this file are the single source of truth: there is no generated copy.
//
// Each app folder holds app.yaml, README.md and default/ (its shipped definitions). An operator's own changes live
// outside the binary in a local layer, so upgrading the binary never overwrites them (docs/apps-platform.md §4).
package firstparty

import "embed"

// FS contains one folder per app. The all: prefix keeps placeholder files such as .gitkeep, which the app loader
// ignores like any other hidden file.
//
//go:embed all:*
var FS embed.FS
