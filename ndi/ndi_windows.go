// Package ndi provides a Go interface to the NDI SDK for receiving video frames.
// It uses runtime DLL loading (no CGo) to call the NDI SDK on Windows and
// receives RGBA frames with alpha channel support from NDI sources.
package ndi

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unsafe"

	"syscall"

	"golang.org/x/sys/windows"
)

// ---------------------------------------------------------------------------
// NDI C struct layouts (x64, verified against NDI SDK v6 headers)
// ---------------------------------------------------------------------------

// ndiSource mirrors NDIlib_source_t (16 bytes on x64).
type ndiSource struct {
	PNdiName    uintptr // const char*
	PUrlAddress uintptr // const char* (union with p_ip_address)
}

// ndiFindCreate mirrors NDIlib_find_create_t (24 bytes on x64).
type ndiFindCreate struct {
	ShowLocalSources uint8   // bool
	_pad0            [7]byte // alignment
	PGroups          uintptr // const char*
	PExtraIps        uintptr // const char*
}

// ndiRecvCreateV3 mirrors NDIlib_recv_create_v3_t (40 bytes on x64).
type ndiRecvCreateV3 struct {
	SourceToConnectTo ndiSource // 16 bytes
	ColorFormat       int32     // enum
	Bandwidth         int32     // enum
	AllowVideoFields  uint8     // bool
	_pad0             [7]byte   // alignment
	PNdiRecvName      uintptr   // const char*
}

// ndiVideoFrameV2 mirrors NDIlib_video_frame_v2_t (72 bytes on x64).
type ndiVideoFrameV2 struct {
	Xres               int32   // offset 0
	Yres               int32   // offset 4
	FourCC             uint32  // offset 8
	FrameRateN         int32   // offset 12
	FrameRateD         int32   // offset 16
	PictureAspectRatio float32 // offset 20
	FrameFormatType    int32   // offset 24
	_pad0              [4]byte // offset 28
	Timecode           int64   // offset 32
	PData              uintptr // offset 40
	LineStrideInBytes  int32   // offset 48 (union)
	_pad1              [4]byte // offset 52
	PMetadata          uintptr // offset 56
	Timestamp          int64   // offset 64
} // total 72

// ---------------------------------------------------------------------------
// NDI constants
// ---------------------------------------------------------------------------

const (
	// Frame types (NDIlib_frame_type_e)
	frameTypeNone         int32 = 0
	frameTypeVideo        int32 = 1
	frameTypeAudio        int32 = 2
	frameTypeMetadata     int32 = 3
	frameTypeError        int32 = 4
	frameTypeStatusChange int32 = 100

	// Recv color format (NDIlib_recv_color_format_e)
	recvColorFormatRGBXRGBA int32 = 2

	// Recv bandwidth (NDIlib_recv_bandwidth_e)
	recvBandwidthHighest int32 = 100

	// FourCC values — NDI_LIB_FOURCC(ch0,ch1,ch2,ch3) = ch0 | ch1<<8 | ch2<<16 | ch3<<24
	fourccRGBA uint32 = uint32('R') | uint32('G')<<8 | uint32('B')<<16 | uint32('A')<<24
	fourccRGBX uint32 = uint32('R') | uint32('G')<<8 | uint32('B')<<16 | uint32('X')<<24
)

// ---------------------------------------------------------------------------
// DLL function pointers (loaded at runtime)
// ---------------------------------------------------------------------------

var (
	loadOnce  sync.Once
	loadErr   error
	dllHandle windows.Handle

	procInitialize            uintptr
	procDestroy               uintptr
	procVersion               uintptr
	procFindCreateV2          uintptr
	procFindDestroy           uintptr
	procFindWaitForSources    uintptr
	procFindGetCurrentSources uintptr
	procRecvCreateV3          uintptr
	procRecvDestroy           uintptr
	procRecvCaptureV2         uintptr
	procRecvFreeVideoV2       uintptr
	procRecvGetNoConnections  uintptr
)

// findDLL locates the NDI runtime DLL by checking:
// 1. NDI_RUNTIME_DIR_V6 env var (set by NDI runtime installer)
// 2. Common installation paths
func findDLL() (string, error) {
	const dllName = "Processing.NDI.Lib.x64.dll"

	// 1. Check env var
	if dir := os.Getenv("NDI_RUNTIME_DIR_V6"); dir != "" {
		// The env var may point to the base dir; the DLL might be in a v6 subdir
		candidates := []string{
			filepath.Join(dir, dllName),
			filepath.Join(dir, "v6", dllName),
		}
		for _, p := range candidates {
			if _, err := os.Stat(p); err == nil {
				return p, nil
			}
		}
	}

	// 2. Common paths
	programFiles := os.Getenv("ProgramFiles")
	if programFiles == "" {
		programFiles = `C:\Program Files`
	}
	commonPaths := []string{
		filepath.Join(programFiles, "NDI", "NDI 6 Runtime", "v6", dllName),
		filepath.Join(programFiles, "NDI", "NDI 6 SDK", "Bin", "x64", dllName),
		filepath.Join(programFiles, "NDI", "NDI 5 Runtime", dllName),
	}
	for _, p := range commonPaths {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}

	return "", fmt.Errorf("ndi: %s not found (set NDI_RUNTIME_DIR_V6 or install NDI runtime)", dllName)
}

// loadLibrary loads the NDI DLL and resolves all function pointers.
func loadLibrary() error {
	dllPath, err := findDLL()
	if err != nil {
		return err
	}

	log.Printf("ndi: loading %s", dllPath)

	// Add the DLL's directory to the search path so any dependent DLLs
	// in the same directory can be found.
	dllDir := filepath.Dir(dllPath)
	windows.SetDllDirectory(dllDir)

	dllHandle, err = windows.LoadLibrary(dllPath)
	if err != nil {
		return fmt.Errorf("ndi: LoadLibrary failed: %w", err)
	}

	// Helper to resolve a symbol; collects errors.
	var errs []string
	resolve := func(name string) uintptr {
		proc, e := windows.GetProcAddress(dllHandle, name)
		if e != nil {
			errs = append(errs, name)
		}
		return proc
	}

	procInitialize = resolve("NDIlib_initialize")
	procDestroy = resolve("NDIlib_destroy")
	procVersion = resolve("NDIlib_version")
	procFindCreateV2 = resolve("NDIlib_find_create_v2")
	procFindDestroy = resolve("NDIlib_find_destroy")
	procFindWaitForSources = resolve("NDIlib_find_wait_for_sources")
	procFindGetCurrentSources = resolve("NDIlib_find_get_current_sources")
	procRecvCreateV3 = resolve("NDIlib_recv_create_v3")
	procRecvDestroy = resolve("NDIlib_recv_destroy")
	procRecvCaptureV2 = resolve("NDIlib_recv_capture_v2")
	procRecvFreeVideoV2 = resolve("NDIlib_recv_free_video_v2")
	procRecvGetNoConnections = resolve("NDIlib_recv_get_no_connections")

	if len(errs) > 0 {
		windows.FreeLibrary(dllHandle)
		dllHandle = 0
		return fmt.Errorf("ndi: missing exports: %s", strings.Join(errs, ", "))
	}

	return nil
}

// call wraps syscall.SyscallN for our NDI calls.
// Returns r1 (the primary return value).
func call(proc uintptr, args ...uintptr) uintptr {
	r1, _, _ := syscall.SyscallN(proc, args...)
	return r1
}

// ---------------------------------------------------------------------------
// Public types (unchanged API surface)
// ---------------------------------------------------------------------------

// Source represents an NDI source available on the network.
type Source struct {
	Name string
	URL  string
}

// Frame holds a single captured video frame with RGBA pixel data.
type Frame struct {
	Width    int
	Height   int
	Stride   int    // bytes per row (usually Width*4 for RGBA, but may have padding)
	Data     []byte // RGBA pixel data, length = Height * Stride
	HasAlpha bool   // true if source provides alpha (FourCC RGBA vs RGBX)
}

// Finder discovers NDI sources on the network.
type Finder struct {
	inst uintptr // NDIlib_find_instance_t (opaque handle)
}

// Receiver connects to a specific NDI source and captures video frames.
type Receiver struct {
	inst uintptr // NDIlib_recv_instance_t (opaque handle)
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

// Initialize loads the NDI runtime DLL and initializes the library.
// Must be called before any other NDI function.
func Initialize() error {
	loadOnce.Do(func() {
		loadErr = loadLibrary()
	})
	if loadErr != nil {
		return loadErr
	}

	ok := call(procInitialize)
	if ok == 0 {
		return fmt.Errorf("ndi: NDIlib_initialize failed (CPU not supported or runtime not installed)")
	}

	verPtr := call(procVersion)
	ver := "(unknown)"
	if verPtr != 0 {
		ver = goStringFromPtr(verPtr)
	}
	log.Printf("ndi: initialized NDI %s", ver)
	return nil
}

// Destroy shuts down the NDI library. Call when done with all NDI operations.
func Destroy() {
	if procDestroy != 0 {
		call(procDestroy)
	}
}

// ---------------------------------------------------------------------------
// Finder
// ---------------------------------------------------------------------------

// NewFinder creates a finder that discovers NDI sources on the network.
func NewFinder() (*Finder, error) {
	settings := ndiFindCreate{
		ShowLocalSources: 1, // true
	}

	inst := call(procFindCreateV2, uintptr(unsafe.Pointer(&settings)))
	if inst == 0 {
		return nil, fmt.Errorf("ndi: failed to create finder")
	}
	return &Finder{inst: inst}, nil
}

// GetSources waits up to the given timeout for sources and returns the current list.
func (f *Finder) GetSources(timeout time.Duration) []Source {
	ms := uint32(timeout.Milliseconds())
	call(procFindWaitForSources, f.inst, uintptr(ms))

	var numSources uint32
	pSources := call(procFindGetCurrentSources, f.inst, uintptr(unsafe.Pointer(&numSources)))
	if numSources == 0 || pSources == 0 {
		return nil
	}

	// Interpret the returned pointer as a slice of ndiSource structs (16 bytes each).
	n := int(numSources)
	cSources := unsafe.Slice((*ndiSource)(unsafe.Pointer(pSources)), n)
	sources := make([]Source, n)
	for i := 0; i < n; i++ {
		sources[i] = Source{
			Name: goStringFromPtr(cSources[i].PNdiName),
			URL:  goStringFromPtr(cSources[i].PUrlAddress),
		}
	}
	return sources
}

// Close destroys the finder instance.
func (f *Finder) Close() {
	if f.inst != 0 {
		call(procFindDestroy, f.inst)
		f.inst = 0
	}
}

// ---------------------------------------------------------------------------
// Receiver
// ---------------------------------------------------------------------------

// NewReceiver creates a receiver connected to the given NDI source.
// It requests RGBA color format to preserve alpha channel transparency.
func NewReceiver(source Source) (*Receiver, error) {
	cName := newCString(source.Name)

	var cURLPtr uintptr
	var cURL cString
	if source.URL != "" {
		cURL = newCString(source.URL)
		cURLPtr = cURL.ptr
	}

	cRecvName := newCString("Fuse")

	settings := ndiRecvCreateV3{
		SourceToConnectTo: ndiSource{
			PNdiName:    cName.ptr,
			PUrlAddress: cURLPtr,
		},
		ColorFormat:      recvColorFormatRGBXRGBA, // RGBA when alpha present
		Bandwidth:        recvBandwidthHighest,
		AllowVideoFields: 0, // false — force progressive
		PNdiRecvName:     cRecvName.ptr,
	}

	inst := call(procRecvCreateV3, uintptr(unsafe.Pointer(&settings)))

	// Keep strings alive until after the call returns
	cName.keepAlive()
	cURL.keepAlive()
	cRecvName.keepAlive()

	if inst == 0 {
		return nil, fmt.Errorf("ndi: failed to create receiver for %q", source.Name)
	}

	log.Printf("ndi: receiver created for %q", source.Name)
	return &Receiver{inst: inst}, nil
}

// CaptureFrame blocks until a video frame is received or the timeout expires.
// The returned Frame owns its pixel data (copied from NDI's internal buffer).
func (r *Receiver) CaptureFrame(timeout time.Duration) (*Frame, error) {
	var videoFrame ndiVideoFrameV2
	ms := uint32(timeout.Milliseconds())

	frameType := int32(call(procRecvCaptureV2,
		r.inst,
		uintptr(unsafe.Pointer(&videoFrame)),
		0, // no audio
		0, // no metadata
		uintptr(ms),
	))

	switch frameType {
	case frameTypeVideo:
		w := int(videoFrame.Xres)
		h := int(videoFrame.Yres)
		stride := int(videoFrame.LineStrideInBytes)
		if stride == 0 {
			stride = w * 4
		}
		dataSize := h * stride

		hasAlpha := videoFrame.FourCC == fourccRGBA

		// Copy pixel data to Go-owned memory
		data := make([]byte, dataSize)
		copy(data, unsafe.Slice((*byte)(unsafe.Pointer(videoFrame.PData)), dataSize))

		// Free NDI's internal buffer immediately
		call(procRecvFreeVideoV2, r.inst, uintptr(unsafe.Pointer(&videoFrame)))

		return &Frame{
			Width:    w,
			Height:   h,
			Stride:   stride,
			Data:     data,
			HasAlpha: hasAlpha,
		}, nil

	case frameTypeNone:
		return nil, fmt.Errorf("ndi: capture timeout")

	case frameTypeError:
		return nil, fmt.Errorf("ndi: connection lost")

	case frameTypeStatusChange:
		return nil, fmt.Errorf("ndi: status change (source properties updated)")

	default:
		return nil, fmt.Errorf("ndi: non-video frame type %d", frameType)
	}
}

// Connected returns true if the receiver is currently connected to a source.
func (r *Receiver) Connected() bool {
	n := call(procRecvGetNoConnections, r.inst)
	return int32(n) > 0
}

// Close destroys the receiver instance.
func (r *Receiver) Close() {
	if r.inst != 0 {
		call(procRecvDestroy, r.inst)
		r.inst = 0
		log.Println("ndi: receiver closed")
	}
}

// ---------------------------------------------------------------------------
// String helpers (C string <-> Go string without CGo)
// ---------------------------------------------------------------------------

// goStringFromPtr reads a null-terminated UTF-8 string from a C pointer.
func goStringFromPtr(p uintptr) string {
	if p == 0 {
		return ""
	}
	// Find null terminator (scan up to 4KB, plenty for NDI names/URLs)
	var n int
	for n = 0; n < 4096; n++ {
		if *(*byte)(unsafe.Pointer(p + uintptr(n))) == 0 {
			break
		}
	}
	return string(unsafe.Slice((*byte)(unsafe.Pointer(p)), n))
}

// cString holds a null-terminated byte slice and its pointer.
// The struct must stay alive (referenced) while the pointer is in use
// to prevent GC from collecting the backing array.
type cString struct {
	buf []byte
	ptr uintptr
}

// newCString allocates a null-terminated C string backed by a Go slice.
// Keep the returned cString alive (don't let it go out of scope) while
// the ptr is being used by native code.
func newCString(s string) cString {
	b := make([]byte, len(s)+1)
	copy(b, s)
	cs := cString{buf: b, ptr: uintptr(unsafe.Pointer(&b[0]))}
	return cs
}

// keepAlive prevents GC from collecting the cString's backing memory.
// Call this after the native call returns.
func (cs *cString) keepAlive() {
	runtime.KeepAlive(cs.buf)
}
