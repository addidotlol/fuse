// cmd/mkicon generates icon.ico from icon.svg using headless Edge/Chrome
// for full SVG rendering fidelity (gradients, filters, etc.).
// Run once: go run ./cmd/mkicon
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	xdraw "golang.org/x/image/draw"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// findBrowser locates Edge or Chrome.
func findBrowser() (string, error) {
	// Try PATH first
	for _, name := range []string{"msedge", "chrome", "google-chrome", "chromium"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}

	if runtime.GOOS == "windows" {
		candidates := []string{
			filepath.Join(os.Getenv("ProgramFiles(x86)"), "Microsoft", "Edge", "Application", "msedge.exe"),
			filepath.Join(os.Getenv("ProgramFiles"), "Microsoft", "Edge", "Application", "msedge.exe"),
			filepath.Join(os.Getenv("ProgramFiles"), "Google", "Chrome", "Application", "chrome.exe"),
			filepath.Join(os.Getenv("ProgramFiles(x86)"), "Google", "Chrome", "Application", "chrome.exe"),
		}
		for _, p := range candidates {
			if _, err := os.Stat(p); err == nil {
				return p, nil
			}
		}
	}

	return "", fmt.Errorf("no Edge or Chrome found; install one or add it to PATH")
}

func run() error {
	const srcSize = 256

	browser, err := findBrowser()
	if err != nil {
		return err
	}
	fmt.Printf("using browser: %s\n", browser)

	// Resolve absolute path to icon.svg
	svgPath, err := filepath.Abs("icon.svg")
	if err != nil {
		return fmt.Errorf("abs path: %w", err)
	}
	if _, err := os.Stat(svgPath); err != nil {
		return fmt.Errorf("icon.svg not found: %w", err)
	}

	// Read the SVG content to inline it in the HTML
	svgContent, err := os.ReadFile(svgPath)
	if err != nil {
		return fmt.Errorf("read icon.svg: %w", err)
	}

	// Create a temporary HTML file that renders the SVG at 256x256 on a transparent background
	// We inline the SVG directly rather than using <img> to ensure gradients render correctly.
	tmpDir, err := os.MkdirTemp("", "mkicon-*")
	if err != nil {
		return fmt.Errorf("mkdirtemp: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	htmlContent := fmt.Sprintf(`<!DOCTYPE html>
<html>
<head><style>
  * { margin: 0; padding: 0; }
  body { width: %dpx; height: %dpx; background: transparent; overflow: hidden; }
  svg { width: %dpx; height: %dpx; }
</style></head>
<body>%s</body>
</html>`, srcSize, srcSize, srcSize, srcSize, string(svgContent))

	htmlPath := filepath.Join(tmpDir, "render.html")
	if err := os.WriteFile(htmlPath, []byte(htmlContent), 0644); err != nil {
		return fmt.Errorf("write html: %w", err)
	}

	// Use headless browser to screenshot the SVG.
	// Use a large window size because --headless=new treats --window-size as the
	// outer window dimensions (including window chrome), so the viewport may be
	// smaller than requested. We render into a big viewport and then crop to the
	// SVG's actual 256x256 area.
	const windowSize = 1024
	pngPath := filepath.Join(tmpDir, "icon.png")
	cmd := exec.Command(browser,
		"--headless=new",
		"--no-sandbox",
		"--default-background-color=00000000", // transparent background (RGBA hex)
		fmt.Sprintf("--screenshot=%s", pngPath),
		fmt.Sprintf("--window-size=%d,%d", windowSize, windowSize),
		fmt.Sprintf("file:///%s", filepath.ToSlash(htmlPath)),
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	fmt.Println("rendering SVG via headless browser...")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("browser screenshot failed: %w", err)
	}

	// Read the rendered PNG
	pngBytes, err := os.ReadFile(pngPath)
	if err != nil {
		return fmt.Errorf("read screenshot: %w", err)
	}

	srcImg, err := png.Decode(bytes.NewReader(pngBytes))
	if err != nil {
		return fmt.Errorf("decode PNG: %w", err)
	}

	// The screenshot may be larger than 256x256 due to the oversized window, or
	// scaled by the device pixel ratio. Crop the top-left srcSize x srcSize region
	// (where the SVG is rendered) and scale if the DPR produced a larger image.
	bounds := srcImg.Bounds()
	fmt.Printf("screenshot is %dx%d\n", bounds.Dx(), bounds.Dy())

	// Crop to the SVG area (top-left corner, srcSize x srcSize in CSS pixels).
	// If DPR > 1, the actual pixel region may be larger, so take the proportional area.
	cropW := srcSize
	cropH := srcSize
	if bounds.Dx() > windowSize {
		// DPR scaling detected - scale crop region proportionally
		dpr := bounds.Dx() / windowSize
		cropW = srcSize * dpr
		cropH = srcSize * dpr
	}
	if cropW > bounds.Dx() {
		cropW = bounds.Dx()
	}
	if cropH > bounds.Dy() {
		cropH = bounds.Dy()
	}

	cropped := image.NewRGBA(image.Rect(0, 0, cropW, cropH))
	xdraw.Copy(cropped, image.Point{}, srcImg, image.Rect(0, 0, cropW, cropH), xdraw.Src, nil)

	// Scale to exactly srcSize x srcSize if needed (e.g. due to DPR)
	var rgba *image.RGBA
	if cropW != srcSize || cropH != srcSize {
		fmt.Printf("cropped %dx%d, scaling to %dx%d\n", cropW, cropH, srcSize, srcSize)
		rgba = image.NewRGBA(image.Rect(0, 0, srcSize, srcSize))
		xdraw.CatmullRom.Scale(rgba, rgba.Bounds(), cropped, cropped.Bounds(), xdraw.Over, nil)
	} else {
		rgba = cropped
	}

	// Generate ICO with multiple sizes
	sizes := []int{16, 32, 48, 256}
	pngBufs := make([][]byte, len(sizes))

	for i, sz := range sizes {
		var dst *image.RGBA
		if sz == srcSize {
			dst = rgba
		} else {
			dst = image.NewRGBA(image.Rect(0, 0, sz, sz))
			xdraw.CatmullRom.Scale(dst, dst.Bounds(), rgba, rgba.Bounds(), xdraw.Over, nil)
		}
		var buf bytes.Buffer
		if err := png.Encode(&buf, dst); err != nil {
			return fmt.Errorf("encode %dx%d PNG: %w", sz, sz, err)
		}
		pngBufs[i] = buf.Bytes()
	}

	// Write ICO
	ico := buildICO(sizes, pngBufs)
	if err := os.WriteFile("icon.ico", ico, 0644); err != nil {
		return fmt.Errorf("write icon.ico: %w", err)
	}

	fmt.Printf("wrote icon.ico (%d bytes, %d sizes: %v)\n", len(ico), len(sizes), sizes)

	// Also copy to tray/
	if err := os.MkdirAll("tray", 0755); err == nil {
		if err := os.WriteFile(filepath.Join("tray", "icon.ico"), ico, 0644); err != nil {
			fmt.Printf("warning: could not copy to tray/icon.ico: %v\n", err)
		} else {
			fmt.Println("copied to tray/icon.ico")
		}
	}

	return nil
}

// buildICO creates an ICO file from PNG-encoded images.
func buildICO(sizes []int, pngData [][]byte) []byte {
	n := len(sizes)
	headerLen := 6
	dirLen := n * 16
	dataStart := headerLen + dirLen

	var buf bytes.Buffer
	buf.Grow(dataStart + totalLen(pngData))

	// Header
	binary.Write(&buf, binary.LittleEndian, uint16(0)) // reserved
	binary.Write(&buf, binary.LittleEndian, uint16(1)) // type: ICO
	binary.Write(&buf, binary.LittleEndian, uint16(n)) // count

	// Directory entries
	offset := uint32(dataStart)
	for i, sz := range sizes {
		w, h := uint8(sz), uint8(sz)
		if sz >= 256 {
			w, h = 0, 0 // 0 means 256 in ICO
		}
		buf.WriteByte(w)
		buf.WriteByte(h)
		buf.WriteByte(0)                                                 // palette
		buf.WriteByte(0)                                                 // reserved
		binary.Write(&buf, binary.LittleEndian, uint16(1))               // color planes
		binary.Write(&buf, binary.LittleEndian, uint16(32))              // bpp
		binary.Write(&buf, binary.LittleEndian, uint32(len(pngData[i]))) // data size
		binary.Write(&buf, binary.LittleEndian, offset)                  // offset
		offset += uint32(len(pngData[i]))
	}

	// PNG payloads
	for _, d := range pngData {
		buf.Write(d)
	}

	return buf.Bytes()
}

func totalLen(bufs [][]byte) int {
	n := 0
	for _, b := range bufs {
		n += len(b)
	}
	return n
}
