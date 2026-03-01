// Package encode manages an FFmpeg subprocess that encodes raw RGBA frames
// into AV1 (stacked color+alpha layout) and outputs IVF-formatted frames.
package encode

import (
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
)

// Config holds encoder parameters.
type Config struct {
	Width   int    // source frame width
	Height  int    // source frame height
	FPS     int    // target framerate
	Bitrate string // target bitrate (e.g. "8M")
	Encoder string // codec name: "av1_qsv" or "libsvtav1"
	FFmpeg  string // path to ffmpeg binary
}

// Encoder wraps an FFmpeg subprocess that reads raw RGBA from stdin,
// applies a split+vstack filter (color top, alpha bottom), encodes AV1,
// and writes IVF frames to stdout.
type Encoder struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr *logWriter

	headerParsed bool
	mu           sync.Mutex
}

// IVF file header fields (parsed once from the stream).
type ivfHeader struct {
	FourCC    [4]byte
	Width     uint16
	Height    uint16
	TimebaseD uint32 // denominator
	TimebaseN uint32 // numerator
	NumFrames uint32
}

// NewEncoder starts the FFmpeg subprocess with the appropriate filter chain
// and encoding parameters.
func NewEncoder(cfg Config) (*Encoder, error) {
	inputSize := fmt.Sprintf("%dx%d", cfg.Width, cfg.Height)

	// Build FFmpeg arguments.
	// Filter: split the RGBA input into RGB and Alpha, convert both to NV12,
	// stack vertically (color on top, alpha on bottom).
	filter := "split[rgb][a];" +
		"[rgb]format=nv12[color];" +
		"[a]alphaextract,format=nv12[alpha];" +
		"[color][alpha]vstack"

	args := []string{
		"-hide_banner",
		"-loglevel", "warning",
		// Input: raw RGBA piped via stdin
		"-f", "rawvideo",
		"-pixel_format", "rgba",
		"-video_size", inputSize,
		"-framerate", strconv.Itoa(cfg.FPS),
		"-i", "pipe:0",
		// Filter
		"-filter_complex", filter,
	}

	// Encoder-specific flags
	switch cfg.Encoder {
	case "av1_qsv":
		args = append(args,
			"-c:v", "av1_qsv",
			"-preset", "veryfast",
			"-look_ahead_depth", "0",
			"-async_depth", "1",
			"-low_power", "1",
			"-bf", "0",
			"-b:v", cfg.Bitrate,
			"-maxrate", cfg.Bitrate,
			"-g", "60",
		)
	case "libsvtav1":
		args = append(args,
			"-c:v", "libsvtav1",
			"-preset", "12",
			"-crf", "30",
			"-g", "60",
			"-svtav1-params", "tune=0:fast-decode=1",
		)
	default:
		return nil, fmt.Errorf("encode: unknown encoder %q", cfg.Encoder)
	}

	// Output: IVF to stdout
	args = append(args, "-f", "ivf", "pipe:1")

	cmd := exec.Command(cfg.FFmpeg, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: 0x08000000, // CREATE_NO_WINDOW
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("encode: stdin pipe: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("encode: stdout pipe: %w", err)
	}

	// Capture stderr for logging
	lw := &logWriter{prefix: "ffmpeg"}
	cmd.Stderr = lw

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("encode: start ffmpeg: %w", err)
	}

	log.Printf("encode: started ffmpeg (pid %d, encoder=%s, %s@%dfps)",
		cmd.Process.Pid, cfg.Encoder, inputSize, cfg.FPS)

	enc := &Encoder{
		cmd:    cmd,
		stdin:  stdin,
		stdout: stdout,
		stderr: lw,
	}

	// NOTE: We do NOT parse the IVF file header here. FFmpeg won't write the
	// header until it processes the first input frame. Parsing here would
	// deadlock because the capture loop hasn't started yet. Instead, the
	// header is parsed lazily on the first ReadFrame() call.

	return enc, nil
}

// parseFileHeader reads and validates the 32-byte IVF file header.
func (e *Encoder) parseFileHeader() error {
	var buf [32]byte
	if _, err := io.ReadFull(e.stdout, buf[:]); err != nil {
		return fmt.Errorf("read IVF header: %w", err)
	}

	// Validate signature "DKIF"
	if string(buf[0:4]) != "DKIF" {
		return fmt.Errorf("invalid IVF signature: %q", buf[0:4])
	}

	hdr := ivfHeader{
		Width:     binary.LittleEndian.Uint16(buf[12:14]),
		Height:    binary.LittleEndian.Uint16(buf[14:16]),
		TimebaseD: binary.LittleEndian.Uint32(buf[16:20]),
		TimebaseN: binary.LittleEndian.Uint32(buf[20:24]),
		NumFrames: binary.LittleEndian.Uint32(buf[24:28]),
	}
	copy(hdr.FourCC[:], buf[8:12])

	log.Printf("encode: IVF header: codec=%s, %dx%d, timebase=%d/%d",
		string(hdr.FourCC[:]), hdr.Width, hdr.Height, hdr.TimebaseN, hdr.TimebaseD)

	e.headerParsed = true
	return nil
}

// WriteFrame writes one raw RGBA frame to FFmpeg's stdin.
// The data must be exactly Width * Height * 4 bytes.
func (e *Encoder) WriteFrame(rgba []byte) error {
	_, err := e.stdin.Write(rgba)
	return err
}

// ReadFrame reads one encoded AV1 frame from the IVF output stream.
// Returns the raw AV1 OBU data for one frame.
// On the first call, it parses the 32-byte IVF file header before reading frames.
func (e *Encoder) ReadFrame() ([]byte, error) {
	// Lazily parse the IVF file header on first read.
	if !e.headerParsed {
		if err := e.parseFileHeader(); err != nil {
			return nil, fmt.Errorf("encode: parse IVF header: %w", err)
		}
	}

	// IVF frame header: 4 bytes frame_size (LE) + 8 bytes PTS (LE) = 12 bytes
	var hdr [12]byte
	if _, err := io.ReadFull(e.stdout, hdr[:]); err != nil {
		return nil, fmt.Errorf("read IVF frame header: %w", err)
	}

	frameSize := binary.LittleEndian.Uint32(hdr[0:4])
	if frameSize == 0 || frameSize > 16*1024*1024 { // sanity: max 16MB per frame
		return nil, fmt.Errorf("suspicious IVF frame size: %d bytes", frameSize)
	}

	data := make([]byte, frameSize)
	if _, err := io.ReadFull(e.stdout, data); err != nil {
		return nil, fmt.Errorf("read IVF frame data (%d bytes): %w", frameSize, err)
	}

	return data, nil
}

// Close shuts down the FFmpeg subprocess gracefully.
func (e *Encoder) Close() error {
	// Close stdin to signal EOF — FFmpeg will flush and exit.
	if e.stdin != nil {
		e.stdin.Close()
	}

	// Wait for FFmpeg to exit.
	err := e.cmd.Wait()
	if err != nil {
		log.Printf("encode: ffmpeg exited: %v", err)
	}
	return err
}

// logWriter captures FFmpeg stderr output and logs each line.
type logWriter struct {
	prefix string
	buf    []byte
}

func (w *logWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	// Flush complete lines
	for {
		idx := -1
		for i, b := range w.buf {
			if b == '\n' {
				idx = i
				break
			}
		}
		if idx < 0 {
			break
		}
		line := string(w.buf[:idx])
		w.buf = w.buf[idx+1:]
		if line != "" {
			log.Printf("%s: %s", w.prefix, line)
		}
	}
	return len(p), nil
}
