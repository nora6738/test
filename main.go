package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	defaultPort       = "8080"
	defaultXrayBin    = "/usr/local/bin/xray"
	defaultConfigPath = "/app/config.json"
	defaultBackendXH  = "127.0.0.1:18443"
	defaultBackendWS  = "127.0.0.1:18444"
	defaultPathXH     = "/bermuda-xhttp"
	defaultPathWS     = "/bermuda-ws"

	probeInterval = 3 * time.Second
	probeTimeout  = 2 * time.Second
	bootGrace     = 15 * time.Second

	backoffBase       = 1 * time.Second
	backoffMax        = 30 * time.Second
	backoffResetAfter = 5 * time.Minute
	httpDrainTimeout  = 12 * time.Second
)

type healthSnapshot struct {
	Status      string `json:"status"`
	Healthy     bool   `json:"healthy"`
	XrayAlive   bool   `json:"xray_alive"`
	XHTTPOk     bool   `json:"xhttp_ok"`
	WSOk        bool   `json:"ws_ok"`
	Restarts    int32  `json:"restarts"`
	UptimeSec   int64  `json:"uptime_sec"`
	LastProbeAt string `json:"last_probe_at"`
	ProbeError  string `json:"probe_error,omitempty"`
}

type healthMonitor struct {
	mu        sync.RWMutex
	snapshot  healthSnapshot
	xhAddr    string
	wsAddr    string
	sup       *supervisor
	startedAt time.Time
}

func newHealthMonitor(sup *supervisor, xhAddr, wsAddr string) *healthMonitor {
	return &healthMonitor{
		xhAddr:    xhAddr,
		wsAddr:    wsAddr,
		sup:       sup,
		startedAt: time.Now(),
		snapshot: healthSnapshot{
			Status:    "starting",
			Healthy:   true,
			XrayAlive: false,
		},
	}
}

func (h *healthMonitor) probeTCP(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, probeTimeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func (h *healthMonitor) runProbe() {
	xrayAlive := h.sup.isAlive()
	xhOK := xrayAlive && h.probeTCP(h.xhAddr)
	wsOK := xrayAlive && h.probeTCP(h.wsAddr)

	now := time.Now()
	booting := now.Sub(h.startedAt) < bootGrace

	status := "ok"
	healthy := true
	var probeErr string

	if !xrayAlive {
		if booting {
			status = "starting"
			healthy = true
		} else {
			status = "down"
			healthy = false
			probeErr = "xray daemon process is not running"
		}
	} else if !xhOK || !wsOK {
		if booting {
			status = "starting"
			healthy = true
		} else {
			status = "degraded"
			healthy = false
			probeErr = fmt.Sprintf("inbounds unhealthy (xh=%v, ws=%v)", xhOK, wsOK)
		}
	}

	snap := healthSnapshot{
		Status:      status,
		Healthy:     healthy,
		XrayAlive:   xrayAlive,
		XHTTPOk:     xhOK,
		WSOk:        wsOK,
		Restarts:    h.sup.getRestarts(),
		UptimeSec:   int64(now.Sub(h.startedAt).Seconds()),
		LastProbeAt: now.UTC().Format(time.RFC3339),
		ProbeError:  probeErr,
	}

	h.mu.Lock()
	h.snapshot = snap
	h.mu.Unlock()
}

func (h *healthMonitor) start(ctx context.Context) {
	ticker := time.NewTicker(probeInterval)
	defer ticker.Stop()

	h.runProbe()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.runProbe()
		}
	}
}

func (h *healthMonitor) getSnapshot() healthSnapshot {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.snapshot
}

type supervisor struct {
	binPath   string
	cfgPath   string
	mu        sync.Mutex
	cmd       *exec.Cmd
	procDone  chan struct{}
	restarts  int32
	stopping  atomic.Bool
	stopCh    chan struct{}
	doneCh    chan struct{}
	spawnedAt time.Time
	backoff   time.Duration
}

func newSupervisor(binPath, cfgPath string) *supervisor {
	return &supervisor{
		binPath: binPath,
		cfgPath: cfgPath,
		stopCh:  make(chan struct{}),
		doneCh:  make(chan struct{}),
		backoff: backoffBase,
	}
}

func (s *supervisor) isAlive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cmd != nil && s.cmd.Process != nil && s.cmd.ProcessState == nil
}

func (s *supervisor) getRestarts() int32 {
	return atomic.LoadInt32(&s.restarts)
}

func (s *supervisor) preflight() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.binPath, "run", "-test", "-c", s.cfgPath)
	cmd.Env = append(os.Environ(), "XRAY_LOCATION_ASSET=/usr/local/share/xray")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("preflight check: %v, output: %s", err, strings.TrimSpace(string(out)))
	}
	log.Printf("[Supervisor] Preflight validation passed for %s", s.cfgPath)
	return nil
}

func (s *supervisor) runLoop() {
	defer close(s.doneCh)

	for {
		if s.stopping.Load() {
			return
		}

		cmd := exec.Command(s.binPath, "run", "-c", s.cfgPath)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Setpgid:   true,
			Pdeathsig: syscall.SIGKILL,
		}
		cmd.Env = append(os.Environ(), "XRAY_LOCATION_ASSET=/usr/local/share/xray")

		if err := cmd.Start(); err != nil {
			log.Printf("[Supervisor] Failed to spawn Xray process: %v", err)
			s.sleepBackoff()
			continue
		}

		procDone := make(chan struct{})
		s.mu.Lock()
		s.cmd = cmd
		s.procDone = procDone
		s.spawnedAt = time.Now()
		s.mu.Unlock()

		log.Printf("[Supervisor] Xray daemon spawned (pid=%d, group=%d)", cmd.Process.Pid, cmd.Process.Pid)

		waitErr := cmd.Wait()
		close(procDone)

		s.mu.Lock()
		uptime := time.Since(s.spawnedAt)
		s.cmd = nil
		s.procDone = nil
		s.mu.Unlock()

		if s.stopping.Load() {
			log.Printf("[Supervisor] Xray exited during shutdown: %v", waitErr)
			return
		}

		atomic.AddInt32(&s.restarts, 1)
		log.Printf("[Supervisor] Xray exited after %s (err: %v). Scheduling restart...", uptime.Round(time.Millisecond), waitErr)

		if uptime >= backoffResetAfter {
			s.mu.Lock()
			s.backoff = backoffBase
			s.mu.Unlock()
		}

		s.sleepBackoff()
	}
}

func (s *supervisor) sleepBackoff() {
	s.mu.Lock()
	dur := s.backoff
	s.backoff *= 2
	if s.backoff > backoffMax {
		s.backoff = backoffMax
	}
	s.mu.Unlock()

	log.Printf("[Supervisor] Backoff wait: %s before respawn", dur)
	timer := time.NewTimer(dur)
	defer timer.Stop()

	select {
	case <-s.stopCh:
	case <-timer.C:
	}
}

func (s *supervisor) start() error {
	if err := s.preflight(); err != nil {
		log.Printf("[Supervisor] Warning during preflight: %v", err)
	}

	go func() {
		runtime.LockOSThread()
		s.runLoop()
	}()

	return nil
}

func (s *supervisor) stop() {
	if s.stopping.Swap(true) {
		return
	}
	close(s.stopCh)

	s.mu.Lock()
	cmd := s.cmd
	procDone := s.procDone
	s.mu.Unlock()

	if cmd != nil && cmd.Process != nil && procDone != nil {
		pgid := cmd.Process.Pid
		log.Printf("[Supervisor] Sending SIGTERM to process group %d", pgid)
		_ = syscall.Kill(-pgid, syscall.SIGTERM)

		select {
		case <-procDone:
			log.Println("[Supervisor] Xray process exited cleanly via SIGTERM")
		case <-time.After(10 * time.Second):
			log.Printf("[Supervisor] Escalating to SIGKILL for process group %d", pgid)
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			select {
			case <-procDone:
			case <-time.After(3 * time.Second):
			}
		}
	}

	<-s.doneCh
	log.Println("[Supervisor] Supervisor loop stopped")
}

func newLoopbackTransport() *http.Transport {
	dialer := &net.Dialer{
		Timeout:   3 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          512,
		MaxIdleConnsPerHost:   256,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableCompression:    true,
	}
}

func newBackendProxy(targetAddr string, tr http.RoundTripper) *httputil.ReverseProxy {
	targetURL, _ := url.Parse("http://" + targetAddr)
	return &httputil.ReverseProxy{
		Transport:     tr,
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = targetURL.Scheme
			pr.Out.URL.Host = targetURL.Host
			pr.Out.Host = pr.In.Host
			pr.SetXForwarded()
			pr.Out.Header.Set("X-Accel-Buffering", "no")
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("[Proxy Error] backend %s unreachable: %v", targetAddr, err)
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("Connection", "close")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, "502 Bad Gateway\n")
		},
	}
}

type tunnelResponseWriter struct {
	http.ResponseWriter
	statusCode  int
	wroteHeader bool
}

func (w *tunnelResponseWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.statusCode = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *tunnelResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.wroteHeader = true
	}
	return w.ResponseWriter.Write(b)
}

func (w *tunnelResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *tunnelResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := w.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, errors.New("underlying ResponseWriter does not support Hijack")
}

type statusLogger struct {
	handler http.Handler
	pathXH  string
	pathWS  string
}

func (l statusLogger) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec := &tunnelResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}
	started := time.Now()
	l.handler.ServeHTTP(rec, r)

	isTunnel := r.URL.Path == l.pathXH || r.URL.Path == l.pathWS ||
		strings.HasPrefix(r.URL.Path, l.pathXH+"/") || strings.HasPrefix(r.URL.Path, l.pathWS+"/")
	isHealth := r.URL.Path == "/healthz"

	if (isTunnel || isHealth) && rec.statusCode < 400 {
		return
	}

	remoteIP := r.RemoteAddr
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		remoteIP = host
	}

	log.Printf("[HTTP] %s %s -> %d (%s) client=%s",
		r.Method, r.URL.Path, rec.statusCode, time.Since(started).Round(time.Millisecond), remoteIP)
}

func camo404(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Server", "openresty")
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, "<html>\r\n<head><title>404 Not Found</title></head>\r\n<body>\r\n<center><h1>404 Not Found</h1></center>\r\n<hr><center>openresty</center>\r\n</body>\r\n</html>\r\n")
}

func applyMemoryLimits() {
	limitStr := strings.TrimSpace(os.Getenv("GOMEMLIMIT"))
	if limitStr == "" {
		debug.SetMemoryLimit(800 * 1024 * 1024)
		log.Println("[Runtime] GOMEMLIMIT defaulted to 800MiB")
	}
}

func getEnv(key, fallback string) string {
	if val := strings.TrimSpace(os.Getenv(key)); val != "" {
		return val
	}
	return fallback
}

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	applyMemoryLimits()

	port := getEnv("PORT", defaultPort)
	xrayBin := getEnv("BERMUDA_XRAY_BIN", defaultXrayBin)
	cfgPath := getEnv("BERMUDA_XRAY_CONFIG", defaultConfigPath)
	backendXH := getEnv("BERMUDA_BACKEND_XH", defaultBackendXH)
	backendWS := getEnv("BERMUDA_BACKEND_WS", defaultBackendWS)
	pathXH := getEnv("BERMUDA_PATH_XH", defaultPathXH)
	pathWS := getEnv("BERMUDA_PATH_WS", defaultPathWS)

	log.Printf("[Gateway] Starting BERMUDA Stealth Gateway on :%s", port)
	log.Printf("[Gateway] Targeting Xray loopback: XHTTP=%s, WS=%s", backendXH, backendWS)

	sup := newSupervisor(xrayBin, cfgPath)
	if err := sup.start(); err != nil {
		log.Printf("[Gateway] Warning: Initial supervisor preflight issue: %v. Supervisor will continue with retry loop.", err)
	}

	hm := newHealthMonitor(sup, backendXH, backendWS)
	healthCtx, cancelHealth := context.WithCancel(context.Background())
	defer cancelHealth()
	go hm.start(healthCtx)

	tr := newLoopbackTransport()
	proxyXH := newBackendProxy(backendXH, tr)
	proxyWS := newBackendProxy(backendWS, tr)

	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		snap := hm.getSnapshot()
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
		if !snap.Healthy {
			w.WriteHeader(http.StatusServiceUnavailable)
		} else {
			w.WriteHeader(http.StatusOK)
		}
		_ = json.NewEncoder(w).Encode(snap)
	})

	mux.Handle(pathXH, proxyXH)
	mux.Handle(pathXH+"/", proxyXH)
	mux.Handle(pathWS, proxyWS)
	mux.Handle(pathWS+"/", proxyWS)

	mux.HandleFunc("/", camo404)

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           statusLogger{handler: mux, pathXH: pathXH, pathWS: pathWS},
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    32 * 1024,
	}

	serverErr := make(chan error, 1)
	go func() {
		serverErr <- srv.ListenAndServe()
	}()

	sigCtx, stopSignal := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stopSignal()

	select {
	case err := <-serverErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("[Gateway] HTTP server failure: %v", err)
		}
	case <-sigCtx.Done():
		log.Println("[Gateway] Termination signal received. Draining incoming traffic...")
	}

	drainCtx, cancelDrain := context.WithTimeout(context.Background(), httpDrainTimeout)
	defer cancelDrain()
	if err := srv.Shutdown(drainCtx); err != nil {
		log.Printf("[Gateway] HTTP drain warning: %v", err)
		_ = srv.Close()
	}

	sup.stop()
	log.Println("[Gateway] Gateway shutdown complete. Ports released.")
}
