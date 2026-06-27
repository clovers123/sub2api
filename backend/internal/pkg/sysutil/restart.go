// Package sysutil provides system-level utilities for process management.
package sysutil

import (
	"log"
	"os"
	"runtime"
	"time"
)

// RestartService triggers a service restart by gracefully exiting.
//
// This relies on systemd's Restart=always configuration to automatically
// restart the service after it exits. This is the industry-standard approach:
//   - Simple and reliable
//   - No sudo permissions needed
//   - No complex process management
//   - Leverages systemd's native restart capability
//
// Prerequisites:
//   - Linux OS with systemd
//   - Service configured with Restart=always in systemd unit file
func RestartService() error {
	if runtime.GOOS != "linux" {
		// Write a marker file so the local dev wrapper (tools/local-start.sh)
		// can detect that setup completed and restart the backend in-place.
		if err := writeRestartMarker(); err != nil {
			log.Printf("Failed to write restart marker: %v", err)
		}
		log.Println("Service restart via exit only works on Linux with systemd")
		return nil
	}

	log.Println("Initiating service restart by graceful exit...")
	log.Println("systemd will automatically restart the service (Restart=always)")

	// Give a moment for logs to flush and response to be sent
	go func() {
		time.Sleep(100 * time.Millisecond)
		os.Exit(0)
	}()

	return nil
}

// RestartServiceAsync is a fire-and-forget version of RestartService.
// It logs errors instead of returning them, suitable for goroutine usage.
func RestartServiceAsync() {
	if err := RestartService(); err != nil {
		log.Printf("Service restart failed: %v", err)
		log.Println("Please restart the service manually: sudo systemctl restart sub2api")
	}
}

// writeRestartMarker writes a marker file that the local dev wrapper can watch.
// The SUB2API_RESTART_MARKER env var (if set) specifies the path; otherwise
// it falls back to "tmp/sub2api-restart-marker".
func writeRestartMarker() error {
	markerPath := os.Getenv("SUB2API_RESTART_MARKER")
	if markerPath == "" {
		markerPath = "tmp/sub2api-restart-marker"
	}
	content := []byte(time.Now().UTC().Format(time.RFC3339) + "\n")
	return os.WriteFile(markerPath, content, 0644)
}
