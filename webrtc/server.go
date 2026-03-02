// Package webrtc provides a WebRTC server that streams AV1 video via Pion.
// It exposes an HTTP endpoint for SDP signaling and serves a test page.
package webrtc

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/intervalpli"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

// Config holds WebRTC server parameters.
type Config struct {
	Port    int    // HTTP port for signaling and static files
	WebRoot string // directory to serve static files from (e.g. "web")
}

// Server manages WebRTC peer connections and a shared AV1 video track.
type Server struct {
	track      *webrtc.TrackLocalStaticSample
	api        *webrtc.API
	httpServer *http.Server

	mu    sync.Mutex
	peers []*webrtc.PeerConnection
}

// NewServer creates a WebRTC server with a shared AV1 track and starts
// the HTTP signaling endpoint.
func NewServer(cfg Config) (*Server, error) {
	// Configure media engine with default codecs (includes AV1)
	mediaEngine := &webrtc.MediaEngine{}
	if err := mediaEngine.RegisterDefaultCodecs(); err != nil {
		return nil, fmt.Errorf("webrtc: register codecs: %w", err)
	}

	// Set up interceptors (NACK, RTCP reports, etc.)
	interceptorRegistry := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(mediaEngine, interceptorRegistry); err != nil {
		return nil, fmt.Errorf("webrtc: register interceptors: %w", err)
	}

	// Add periodic PLI (keyframe request) interceptor
	pliFactory, err := intervalpli.NewReceiverInterceptor()
	if err != nil {
		return nil, fmt.Errorf("webrtc: PLI interceptor: %w", err)
	}
	interceptorRegistry.Add(pliFactory)

	// Configure for loopback
	se := webrtc.SettingEngine{}
	se.SetIncludeLoopbackCandidate(true)
	se.SetLite(true) // ICE Lite — no connectivity checks needed for loopback
	se.SetICETimeouts(
		10*time.Second, // disconnected timeout
		30*time.Second, // failed timeout
		2*time.Second,  // keepalive interval
	)

	api := webrtc.NewAPI(
		webrtc.WithMediaEngine(mediaEngine),
		webrtc.WithInterceptorRegistry(interceptorRegistry),
		webrtc.WithSettingEngine(se),
	)

	// Create the shared AV1 video track
	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeAV1},
		"video",
		"fuse",
	)
	if err != nil {
		return nil, fmt.Errorf("webrtc: create track: %w", err)
	}

	s := &Server{
		track: track,
		api:   api,
	}

	// Set up HTTP handlers
	mux := http.NewServeMux()
	mux.HandleFunc("POST /offer", s.handleOffer)
	mux.Handle("/", http.FileServer(http.Dir(cfg.WebRoot)))

	s.httpServer = &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.Port),
		Handler: corsAllowAll(mux),
	}

	// Start HTTP server
	ln, err := net.Listen("tcp", s.httpServer.Addr)
	if err != nil {
		return nil, fmt.Errorf("webrtc: listen: %w", err)
	}

	go func() {
		log.Printf("webrtc: HTTP server listening on http://localhost:%d", cfg.Port)
		if err := s.httpServer.Serve(ln); err != http.ErrServerClosed {
			log.Printf("webrtc: HTTP server error: %v", err)
		}
	}()

	return s, nil
}

func corsAllowAll(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "*")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// handleOffer processes a WebRTC SDP offer from a browser client and returns
// an SDP answer with ICE candidates baked in (non-trickle).
func (s *Server) handleOffer(w http.ResponseWriter, r *http.Request) {
	var offer webrtc.SessionDescription
	if err := json.NewDecoder(r.Body).Decode(&offer); err != nil {
		http.Error(w, "invalid SDP offer: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Create a new PeerConnection for this viewer
	pc, err := s.api.NewPeerConnection(webrtc.Configuration{
		// No ICE servers needed for loopback
	})
	if err != nil {
		http.Error(w, "create peer connection: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Clean up on connection failure/close
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("webrtc: peer connection state: %s", state.String())
		if state == webrtc.PeerConnectionStateFailed ||
			state == webrtc.PeerConnectionStateClosed ||
			state == webrtc.PeerConnectionStateDisconnected {
			s.removePeer(pc)
			pc.Close()
		}
	})

	// Add the shared video track
	rtpSender, err := pc.AddTrack(s.track)
	if err != nil {
		pc.Close()
		http.Error(w, "add track: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Drain incoming RTCP from the sender — required for interceptors (NACK, etc.)
	go func() {
		buf := make([]byte, 1500)
		for {
			if _, _, err := rtpSender.Read(buf); err != nil {
				return
			}
		}
	}()

	// Set the remote offer
	if err := pc.SetRemoteDescription(offer); err != nil {
		pc.Close()
		http.Error(w, "set remote description: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Create answer
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		pc.Close()
		http.Error(w, "create answer: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Wait for ICE gathering to complete (non-trickle: bake all candidates into SDP)
	gatherComplete := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		pc.Close()
		http.Error(w, "set local description: "+err.Error(), http.StatusInternalServerError)
		return
	}
	<-gatherComplete

	// Track the peer
	s.mu.Lock()
	s.peers = append(s.peers, pc)
	s.mu.Unlock()

	log.Printf("webrtc: new peer connected (total: %d)", len(s.peers))

	// Return the answer with ICE candidates
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(pc.LocalDescription())
}

// removePeer removes a peer connection from tracking.
func (s *Server) removePeer(pc *webrtc.PeerConnection) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, p := range s.peers {
		if p == pc {
			s.peers = append(s.peers[:i], s.peers[i+1:]...)
			log.Printf("webrtc: peer disconnected (remaining: %d)", len(s.peers))
			return
		}
	}
}

// WriteFrame writes an encoded AV1 frame to the shared track.
// All connected peers receive the frame via RTP.
func (s *Server) WriteFrame(data []byte, duration time.Duration) error {
	return s.track.WriteSample(media.Sample{
		Data:     data,
		Duration: duration,
	})
}

// PeerCount returns the number of currently connected peers.
func (s *Server) PeerCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.peers)
}

// Close shuts down the HTTP server and all peer connections.
func (s *Server) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	s.httpServer.Shutdown(ctx)

	s.mu.Lock()
	peers := s.peers
	s.peers = nil
	s.mu.Unlock()

	for _, pc := range peers {
		pc.Close()
	}
	log.Println("webrtc: server closed")
}
