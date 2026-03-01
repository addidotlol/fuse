package encode

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

// FindFFmpeg locates the ffmpeg binary on the system.
// Search order: FFMPEG_PATH env var > PATH > common Windows locations.
func FindFFmpeg() (string, error) {
	// 1. Explicit env var
	if p := os.Getenv("FFMPEG_PATH"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}

	// 2. System PATH
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		return p, nil
	}

	// 3. Common Windows locations
	if runtime.GOOS == "windows" {
		candidates := []string{
			// WinGet (Gyan build)
			filepath.Join(os.Getenv("LOCALAPPDATA"),
				"Microsoft", "WinGet", "Packages"),
			`C:\ffmpeg\bin`,
			`C:\Program Files\ffmpeg\bin`,
			`C:\Program Files\Streamlink\ffmpeg`,
		}

		for _, dir := range candidates {
			// For the WinGet path, we need to glob since the package dir has a hash
			if strings.Contains(dir, "WinGet") {
				matches, _ := filepath.Glob(filepath.Join(dir, "Gyan.FFmpeg*", "ffmpeg-*", "bin", "ffmpeg.exe"))
				if len(matches) > 0 {
					return matches[0], nil
				}
				continue
			}
			p := filepath.Join(dir, "ffmpeg.exe")
			if _, err := os.Stat(p); err == nil {
				return p, nil
			}
		}
	}

	return "", fmt.Errorf("ffmpeg not found (set FFMPEG_PATH or add ffmpeg to PATH)")
}

// ProbeEncoder determines the best available AV1 encoder.
// Returns "av1_qsv" if Intel Quick Sync is available and functional,
// otherwise falls back to "libsvtav1".
func ProbeEncoder(ffmpegPath string) string {
	// Check if av1_qsv is listed in encoders
	listCmd := exec.Command(ffmpegPath, "-hide_banner", "-encoders")
	listCmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: 0x08000000, // CREATE_NO_WINDOW
	}
	out, err := listCmd.CombinedOutput()
	if err != nil {
		log.Printf("encode: failed to list encoders: %v", err)
		return "libsvtav1"
	}

	if !strings.Contains(string(out), "av1_qsv") {
		log.Println("encode: av1_qsv not compiled into FFmpeg, using libsvtav1")
		return "libsvtav1"
	}

	// Smoke-test av1_qsv with a tiny encode to verify hardware is present
	testCmd := exec.Command(ffmpegPath,
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "color=black:s=256x256:d=0.04:r=25",
		"-frames:v", "1",
		"-c:v", "av1_qsv",
		"-b:v", "1M",
		"-f", "null", "-",
	)
	testCmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: 0x08000000, // CREATE_NO_WINDOW
	}
	if err := testCmd.Run(); err != nil {
		log.Printf("encode: av1_qsv smoke test failed: %v, using libsvtav1", err)
		return "libsvtav1"
	}

	log.Println("encode: av1_qsv available and functional (hardware AV1 encoding)")
	return "av1_qsv"
}
