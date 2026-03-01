package tray

import _ "embed"

// IconBytes contains the .ico file for the system tray icon.
//
//go:embed icon.ico
var IconBytes []byte
