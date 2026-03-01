// Package tray provides a Windows system tray icon with NDI source selection,
// reconnect controls, and connection status display.
package tray

import (
	"log"
	"sync"
	"time"

	"github.com/getlantern/systray"
)

// maxSources is the pre-allocated pool size for NDI source menu items.
const maxSources = 64

// Controller is the interface that the tray uses to communicate with the pipeline.
type Controller interface {
	// GetSources returns the currently visible NDI sources.
	GetSources() []string
	// SelectSource connects to the named NDI source.
	SelectSource(name string)
	// Reconnect tears down and restarts the current pipeline.
	Reconnect()
	// Shutdown stops everything and exits.
	Shutdown()
	// Status returns the current connection state.
	Status() (connected bool, sourceName string)
}

// Run starts the system tray UI. This function blocks and must be called
// from the main goroutine (it owns the OS thread for the Windows message pump).
func Run(ctrl Controller) {
	systray.Run(
		func() { onReady(ctrl) },
		func() { onExit(ctrl) },
	)
}

var (
	mu          sync.Mutex
	sourceItems [maxSources]*systray.MenuItem
	sourceNames [maxSources]string
	mStatus     *systray.MenuItem
	mReconnect  *systray.MenuItem
)

func onReady(ctrl Controller) {
	systray.SetIcon(IconBytes)
	systray.SetTooltip("Fuse - NDI to WebRTC Bridge")

	// Status line (disabled = non-clickable, informational)
	mStatus = systray.AddMenuItem("Status: Disconnected", "Current connection status")
	mStatus.Disable()

	systray.AddSeparator()

	// NDI Sources submenu
	mSources := systray.AddMenuItem("NDI Sources", "Available NDI sources")

	// Pre-allocate source item pool, all hidden initially
	for i := 0; i < maxSources; i++ {
		item := mSources.AddSubMenuItem("", "")
		item.Hide()
		sourceItems[i] = item

		// Each item gets its own click handler
		idx := i
		go func() {
			for range item.ClickedCh {
				mu.Lock()
				name := sourceNames[idx]
				mu.Unlock()
				if name != "" {
					log.Printf("tray: selected source %q", name)
					ctrl.SelectSource(name)
				}
			}
		}()
	}

	systray.AddSeparator()

	// Reconnect button
	mReconnect = systray.AddMenuItem("Reconnect", "Reconnect to current NDI source")

	systray.AddSeparator()

	// Quit button
	mQuit := systray.AddMenuItem("Quit", "Exit Fuse")

	// Main event loop
	go func() {
		for {
			select {
			case <-mReconnect.ClickedCh:
				log.Println("tray: reconnect requested")
				ctrl.Reconnect()
			case <-mQuit.ClickedCh:
				log.Println("tray: quit requested")
				ctrl.Shutdown()
				systray.Quit()
				return
			}
		}
	}()

	// Background: poll for source changes and update status
	go pollLoop(ctrl)
}

func onExit(ctrl Controller) {
	log.Println("tray: exiting")
}

// pollLoop periodically updates the NDI source list and connection status.
func pollLoop(ctrl Controller) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	// Do an immediate first poll
	updateSources(ctrl)
	updateStatus(ctrl)

	for range ticker.C {
		updateSources(ctrl)
		updateStatus(ctrl)
	}
}

// updateSources refreshes the NDI source submenu items.
func updateSources(ctrl Controller) {
	sources := ctrl.GetSources()
	connected, currentSource := ctrl.Status()

	mu.Lock()
	defer mu.Unlock()

	for i := 0; i < maxSources; i++ {
		if i < len(sources) {
			sourceNames[i] = sources[i]
			sourceItems[i].SetTitle(sources[i])
			sourceItems[i].SetTooltip("Connect to " + sources[i])
			sourceItems[i].Show()

			// Checkmark on the currently connected source
			if connected && sources[i] == currentSource {
				sourceItems[i].Check()
			} else {
				sourceItems[i].Uncheck()
			}
		} else {
			sourceNames[i] = ""
			sourceItems[i].Hide()
		}
	}
}

// updateStatus refreshes the status display and tooltip.
func updateStatus(ctrl Controller) {
	connected, sourceName := ctrl.Status()
	if connected {
		mStatus.SetTitle("Status: Connected to " + sourceName)
		systray.SetTooltip("Fuse - Connected: " + sourceName)
		mReconnect.Enable()
	} else if sourceName != "" {
		mStatus.SetTitle("Status: Disconnected (was: " + sourceName + ")")
		systray.SetTooltip("Fuse - Disconnected")
		mReconnect.Enable() // allow retry
	} else {
		mStatus.SetTitle("Status: No source selected")
		systray.SetTooltip("Fuse - No source selected")
		mReconnect.Disable() // nothing to reconnect to
	}
}
