// Fuse is a lightweight video bridge that captures a VTuber avatar from
// VTube Studio via NDI, encodes it with AV1 (preserving alpha via a
// stacked-frame layout), and serves it to a browser via WebRTC.
package main

import (
	"log"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/addidotlol/fuse/encode"
	"github.com/addidotlol/fuse/ndi"
	"github.com/addidotlol/fuse/tray"
	"github.com/addidotlol/fuse/webrtc"
)

// envConfig holds configuration parsed from environment variables.
type envConfig struct {
	Port    int
	Width   int
	Height  int
	FPS     int
	Bitrate string
}

func loadConfig() envConfig {
	cfg := envConfig{
		Port:    9090,
		Width:   1920,
		Height:  1080,
		FPS:     30,
		Bitrate: "8M",
	}
	if v := os.Getenv("FUSE_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Port = n
		}
	}
	if v := os.Getenv("FUSE_WIDTH"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Width = n
		}
	}
	if v := os.Getenv("FUSE_HEIGHT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Height = n
		}
	}
	if v := os.Getenv("FUSE_FPS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.FPS = n
		}
	}
	if v := os.Getenv("FUSE_BITRATE"); v != "" {
		cfg.Bitrate = v
	}
	return cfg
}

// pipeline manages the NDI→Encode→WebRTC data flow.
type pipeline struct {
	mu       sync.Mutex
	cfg      envConfig
	ffmpeg   string
	encoder  string
	finder   *ndi.Finder
	receiver *ndi.Receiver
	enc      *encode.Encoder
	server   *webrtc.Server

	source    string
	connected bool
	stopCh    chan struct{}
	wg        sync.WaitGroup

	shutdownOnce sync.Once
	doneCh       chan struct{}
}

func newPipeline(cfg envConfig) (*pipeline, error) {
	// Find FFmpeg
	ffmpegPath, err := encode.FindFFmpeg()
	if err != nil {
		return nil, err
	}
	log.Printf("main: using ffmpeg at %s", ffmpegPath)

	// Probe best encoder
	encoderName := encode.ProbeEncoder(ffmpegPath)
	log.Printf("main: using encoder %s", encoderName)

	// Initialize NDI
	if err := ndi.Initialize(); err != nil {
		return nil, err
	}

	// Create NDI finder (runs in background)
	finder, err := ndi.NewFinder()
	if err != nil {
		return nil, err
	}

	// Start WebRTC server
	server, err := webrtc.NewServer(webrtc.Config{
		Port:    cfg.Port,
		WebRoot: "web",
	})
	if err != nil {
		return nil, err
	}

	return &pipeline{
		cfg:     cfg,
		ffmpeg:  ffmpegPath,
		encoder: encoderName,
		finder:  finder,
		server:  server,
		stopCh:  make(chan struct{}),
		doneCh:  make(chan struct{}),
	}, nil
}

// --- tray.Controller interface ---

func (p *pipeline) GetSources() []string {
	sources := p.finder.GetSources(100 * time.Millisecond)
	names := make([]string, len(sources))
	for i, s := range sources {
		names[i] = s.Name
	}
	return names
}

func (p *pipeline) SelectSource(name string) {
	p.mu.Lock()
	if p.source == name && p.connected {
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()

	// Stop current pipeline if running
	p.stop()

	// Find the full source info
	sources := p.finder.GetSources(500 * time.Millisecond)
	var src ndi.Source
	for _, s := range sources {
		if s.Name == name {
			src = s
			break
		}
	}
	if src.Name == "" {
		log.Printf("main: source %q not found", name)
		return
	}

	p.mu.Lock()
	p.source = name
	p.mu.Unlock()

	p.start(src)
}

func (p *pipeline) Reconnect() {
	p.mu.Lock()
	name := p.source
	p.mu.Unlock()

	if name == "" {
		log.Println("main: no source selected, cannot reconnect")
		return
	}

	p.stop()

	sources := p.finder.GetSources(500 * time.Millisecond)
	var src ndi.Source
	for _, s := range sources {
		if s.Name == name {
			src = s
			break
		}
	}
	if src.Name == "" {
		log.Printf("main: source %q not found for reconnect", name)
		p.mu.Lock()
		p.connected = false
		p.mu.Unlock()
		return
	}

	p.start(src)
}

func (p *pipeline) Shutdown() {
	p.shutdownOnce.Do(func() {
		p.stop()
		p.server.Close()
		p.finder.Close()
		ndi.Destroy()
		close(p.doneCh)
	})
}

func (p *pipeline) Status() (bool, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.connected, p.source
}

// start begins the capture→encode→stream loop.
// The encoder is created lazily after the first NDI frame arrives,
// so we use the actual source resolution rather than a hardcoded config.
func (p *pipeline) start(src ndi.Source) {
	receiver, err := ndi.NewReceiver(src)
	if err != nil {
		log.Printf("main: failed to create receiver: %v", err)
		return
	}

	p.mu.Lock()
	p.receiver = receiver
	p.connected = true
	p.stopCh = make(chan struct{})
	p.mu.Unlock()

	// Single goroutine: capture → (lazy encoder init) → encode → read → webrtc
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()

		var enc *encode.Encoder
		var encWidth, encHeight int
		var readDone chan struct{}

		defer func() {
			// Tear down encoder when capture loop exits
			if enc != nil {
				enc.Close()
				if readDone != nil {
					<-readDone // wait for read goroutine
				}
			}
			p.mu.Lock()
			p.enc = nil
			p.mu.Unlock()
		}()

		for {
			select {
			case <-p.stopCh:
				return
			default:
			}

			frame, err := receiver.CaptureFrame(1 * time.Second)
			if err != nil {
				// Timeout is normal when source is not yet sending
				continue
			}

			// Lazily create (or recreate) the encoder when resolution is first known or changes.
			if enc == nil || frame.Width != encWidth || frame.Height != encHeight {
				if enc != nil {
					log.Printf("main: resolution changed from %dx%d to %dx%d, restarting encoder",
						encWidth, encHeight, frame.Width, frame.Height)
					enc.Close()
					if readDone != nil {
						<-readDone
					}
				}

				encWidth = frame.Width
				encHeight = frame.Height

				enc, err = encode.NewEncoder(encode.Config{
					Width:   encWidth,
					Height:  encHeight,
					FPS:     p.cfg.FPS,
					Bitrate: p.cfg.Bitrate,
					Encoder: p.encoder,
					FFmpeg:  p.ffmpeg,
				})
				if err != nil {
					log.Printf("main: failed to create encoder: %v", err)
					return
				}

				p.mu.Lock()
				p.enc = enc
				p.mu.Unlock()

				log.Printf("main: pipeline started (source=%q, %dx%d@%dfps, encoder=%s)",
					src.Name, encWidth, encHeight, p.cfg.FPS, p.encoder)

				// Start the read goroutine for this encoder instance
				frameDuration := time.Second / time.Duration(p.cfg.FPS)
				readDone = make(chan struct{})
				currentEnc := enc
				go func() {
					defer close(readDone)
					for {
						data, err := currentEnc.ReadFrame()
						if err != nil {
							log.Printf("main: read frame error: %v", err)
							return
						}
						if err := p.server.WriteFrame(data, frameDuration); err != nil {
							log.Printf("main: write frame error: %v", err)
							return
						}
					}
				}()
			}

			// Write the RGBA frame to FFmpeg.
			// Strip row padding if stride != width*4.
			expectedSize := frame.Width * frame.Height * 4
			var rgba []byte
			if frame.Stride == frame.Width*4 {
				rgba = frame.Data
			} else {
				rgba = make([]byte, expectedSize)
				rowBytes := frame.Width * 4
				for y := 0; y < frame.Height; y++ {
					copy(rgba[y*rowBytes:(y+1)*rowBytes],
						frame.Data[y*frame.Stride:y*frame.Stride+rowBytes])
				}
			}

			if err := enc.WriteFrame(rgba[:expectedSize]); err != nil {
				log.Printf("main: encode write error: %v", err)
				return
			}
		}
	}()
}

// stop tears down the current capture→encode pipeline.
func (p *pipeline) stop() {
	p.mu.Lock()
	if p.receiver == nil && p.enc == nil {
		p.mu.Unlock()
		return
	}

	// Signal loops to stop
	select {
	case <-p.stopCh:
	default:
		close(p.stopCh)
	}
	p.mu.Unlock()

	// Wait for goroutines
	p.wg.Wait()

	p.mu.Lock()
	if p.receiver != nil {
		p.receiver.Close()
		p.receiver = nil
	}
	if p.enc != nil {
		p.enc.Close()
		p.enc = nil
	}
	p.connected = false
	p.mu.Unlock()

	log.Println("main: pipeline stopped")
}

func main() {
	// Log to fuse.log next to the executable
	logFile, err := os.OpenFile("fuse.log", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		// If we can't open the log file, fall back to stderr
		log.Printf("main: failed to open fuse.log: %v (logging to stderr)", err)
	} else {
		log.SetOutput(logFile)
		defer logFile.Close()
	}
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)

	cfg := loadConfig()
	log.Printf("main: config port=%d resolution=%dx%d fps=%d bitrate=%s",
		cfg.Port, cfg.Width, cfg.Height, cfg.FPS, cfg.Bitrate)

	p, err := newPipeline(cfg)
	if err != nil {
		log.Fatalf("main: failed to initialize: %v", err)
	}

	// Handle OS signals for graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Println("main: signal received, shutting down")
		p.Shutdown()
	}()

	// Run the system tray on the main goroutine (required — it owns the OS thread
	// for the Windows message pump via runtime.LockOSThread in systray's init()).
	// This call blocks until Quit is selected or Shutdown() is called.
	tray.Run(p)

	// Wait for clean shutdown
	<-p.doneCh
	log.Println("main: goodbye")
}
