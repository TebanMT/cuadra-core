package biometric

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"time"
)

// EngineConfig parameterizes the helper process manager. Zero values fall
// back to production defaults; tests override Path/Args/Env to point at a
// fake helper and shrink the timing knobs.
type EngineConfig struct {
	// Path to the tinta-bio executable. Empty → auto-resolve: TINTA_BIO_PATH
	// env, then alongside the sidecar executable, then cwd, then $PATH —
	// the same cascade resolveBinaries used for the NBIS binaries.
	Path string
	// Args/Env are appended to the spawned process (tests only).
	Args []string
	Env  []string

	Logger *log.Logger

	// CommandTimeout bounds each round-trip (default 10s — identify against
	// a barrio-gym gallery is milliseconds in the helper).
	CommandTimeout time.Duration
	// RestartBackoffMin/Max bound the respawn delay after the helper dies
	// (default 1s → 8s, doubling; reset after a stable run).
	RestartBackoffMin time.Duration
	RestartBackoffMax time.Duration
	// MissingBinaryRetry is how often to re-probe for the executable when it
	// can't be found at all (default 60s — dev machines without the helper
	// shouldn't spin a hot loop).
	MissingBinaryRetry time.Duration
	// PingInterval is the health-check cadence; a failed ping kills the
	// process so the supervisor respawns it (default 30s).
	PingInterval time.Duration
}

// Engine supervises the tinta-bio helper process and multiplexes the NDJSON
// protocol: commands (with id-correlated responses) and spontaneous events
// (fanned out to the Handler from a single dispatch goroutine).
type Engine struct {
	cfg     EngineConfig
	handler Handler

	mu              sync.Mutex
	alive           bool
	readerConnected bool
	readerName      string
	readerSerial    string
	stdin           io.WriteCloser
	proc            *os.Process
	pending         map[string]chan *helperMsg
	nextID          uint64
	stopping        bool

	writeMu sync.Mutex // one NDJSON line at a time on stdin

	// dispatch serializes Handler callbacks (events + up/down) so the hub
	// sees them in order without its own queueing.
	dispatch chan func()
	stopCh   chan struct{}
	stopOnce sync.Once
}

// NewEngine builds the process manager. Call SetHandler before Start.
func NewEngine(cfg EngineConfig) *Engine {
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	if cfg.CommandTimeout <= 0 {
		cfg.CommandTimeout = 10 * time.Second
	}
	if cfg.RestartBackoffMin <= 0 {
		cfg.RestartBackoffMin = time.Second
	}
	if cfg.RestartBackoffMax <= 0 {
		cfg.RestartBackoffMax = 8 * time.Second
	}
	if cfg.MissingBinaryRetry <= 0 {
		cfg.MissingBinaryRetry = 60 * time.Second
	}
	if cfg.PingInterval <= 0 {
		cfg.PingInterval = 30 * time.Second
	}
	return &Engine{
		cfg:      cfg,
		pending:  make(map[string]chan *helperMsg),
		dispatch: make(chan func(), 256),
		stopCh:   make(chan struct{}),
	}
}

// SetHandler wires the event sink. Must be called before Start.
func (e *Engine) SetHandler(h Handler) { e.handler = h }

// Start spawns the supervisor + dispatch goroutines and returns immediately.
// The engine keeps (re)spawning the helper until ctx is done or Stop is
// called; ctx cancellation triggers the same clean shutdown as Stop.
func (e *Engine) Start(ctx context.Context) {
	go e.runDispatch()
	go e.supervise(ctx)
	go func() {
		<-ctx.Done()
		e.Stop()
	}()
}

// Stop shuts the engine down: closes the helper's stdin (EOF = clean helper
// exit per PROTOCOL.md), kills it if it lingers, and stops the goroutines.
func (e *Engine) Stop() {
	e.stopOnce.Do(func() {
		e.mu.Lock()
		e.stopping = true
		stdin := e.stdin
		proc := e.proc
		e.mu.Unlock()
		close(e.stopCh)
		if stdin != nil {
			_ = stdin.Close()
		}
		if proc != nil {
			// Grace for the EOF-driven exit, then hard kill.
			done := make(chan struct{})
			go func() {
				for i := 0; i < 30; i++ {
					e.mu.Lock()
					gone := !e.alive
					e.mu.Unlock()
					if gone {
						break
					}
					time.Sleep(100 * time.Millisecond)
				}
				close(done)
			}()
			<-done
			e.mu.Lock()
			if e.alive && e.proc != nil {
				_ = e.proc.Kill()
			}
			e.mu.Unlock()
		}
	})
}

// Alive reports whether the helper process is currently running.
func (e *Engine) Alive() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.alive
}

// Connected reports whether the helper has an open fingerprint reader.
func (e *Engine) Connected() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.alive && e.readerConnected
}

// Info exposes the reader identity for /biometric/status.
func (e *Engine) Info() ReaderInfo {
	e.mu.Lock()
	defer e.mu.Unlock()
	model := e.readerName
	if model == "" {
		model = "U.are.U (tinta-bio)"
	}
	return ReaderInfo{
		DeviceID:  e.readerSerial,
		Vendor:    "HID/DigitalPersona",
		Model:     model,
		Connected: e.alive && e.readerConnected,
	}
}

// ─────────────────────────── commands ───────────────────────────

// Ping health-checks the helper and reports which gallery it holds.
func (e *Engine) Ping(ctx context.Context) (state, galleryEpoch string, gallerySize int, err error) {
	resp, err := e.roundTrip(ctx, map[string]any{"cmd": "ping"})
	if err != nil {
		return "", "", 0, err
	}
	return resp.State, resp.GalleryEpoch, resp.GallerySize, nil
}

// SetGallery replaces the helper's 1:N cache wholesale (PROTOCOL.md
// `gallery`). candidates may be empty (clears the gallery).
func (e *Engine) SetGallery(ctx context.Context, epoch string, candidates []GalleryCandidate) error {
	if candidates == nil {
		candidates = []GalleryCandidate{}
	}
	_, err := e.roundTrip(ctx, map[string]any{
		"cmd": "gallery", "epoch": epoch, "candidates": candidates,
	})
	return err
}

// Identify runs the probe FMD against the helper's cached gallery. Returns
// the matched refs (may be empty = no identificado) and the epoch of the
// gallery the helper matched against — the caller MUST compare it to its own
// epoch and re-send the gallery + retry on mismatch (enroll-vs-identify race).
func (e *Engine) Identify(ctx context.Context, probeFMD string, farDivisor, max int) (matches []string, galleryEpoch string, err error) {
	if farDivisor <= 0 {
		farDivisor = DefaultFarDivisor
	}
	if max <= 0 {
		max = 1
	}
	resp, err := e.roundTrip(ctx, map[string]any{
		"cmd": "identify", "probe": probeFMD, "farDivisor": farDivisor, "max": max,
	})
	if err != nil {
		return nil, "", err
	}
	return resp.Matches, resp.GalleryEpoch, nil
}

// EnrollCombine turns the accumulated pre-enroll sample FMDs into a single
// enrollment FMD. A *CommandError (e.g. DP_ENROLLMENT_INVALID_SET) means the
// set didn't converge — the flow asks for another dedazo.
func (e *Engine) EnrollCombine(ctx context.Context, fmds []string) (string, error) {
	resp, err := e.roundTrip(ctx, map[string]any{"cmd": "enroll", "fmds": fmds})
	if err != nil {
		return "", err
	}
	if resp.FMD == "" {
		return "", &CommandError{Code: "empty_enrollment_fmd"}
	}
	return resp.FMD, nil
}

// ─────────────────────────── wire types ───────────────────────────

// helperMsg is any NDJSON line the helper emits — spontaneous events and
// command results share one shape (PROTOCOL.md).
type helperMsg struct {
	Event string `json:"event"`

	// reader
	State  string `json:"state,omitempty"`
	Name   string `json:"name,omitempty"`
	Serial string `json:"serial,omitempty"`

	// sample / sample_rejected / error / result-not-ok
	FMD     string `json:"fmd,omitempty"`
	Quality string `json:"quality,omitempty"`
	Code    string `json:"code,omitempty"`
	Detail  string `json:"detail,omitempty"`

	// result
	ID           string   `json:"id,omitempty"`
	OK           bool     `json:"ok,omitempty"`
	GalleryEpoch string   `json:"galleryEpoch,omitempty"`
	GallerySize  int      `json:"gallerySize,omitempty"`
	Matches      []string `json:"matches,omitempty"`
	Score        *int64   `json:"score,omitempty"`
}

// roundTrip sends one command (payload sin "id"; se asigna aquí) and blocks
// until the helper answers, the context fires, the timeout hits, or the
// helper dies mid-flight.
func (e *Engine) roundTrip(ctx context.Context, payload map[string]any) (*helperMsg, error) {
	e.mu.Lock()
	if !e.alive || e.stdin == nil {
		e.mu.Unlock()
		return nil, ErrNotAvailable
	}
	e.nextID++
	id := strconv.FormatUint(e.nextID, 10)
	ch := make(chan *helperMsg, 1)
	e.pending[id] = ch
	stdin := e.stdin
	e.mu.Unlock()

	cleanup := func() {
		e.mu.Lock()
		delete(e.pending, id)
		e.mu.Unlock()
	}

	payload["id"] = id
	line, err := json.Marshal(payload)
	if err != nil {
		cleanup()
		return nil, err
	}
	e.writeMu.Lock()
	_, err = stdin.Write(append(line, '\n'))
	e.writeMu.Unlock()
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("tinta-bio stdin: %w", err)
	}

	timer := time.NewTimer(e.cfg.CommandTimeout)
	defer timer.Stop()
	select {
	case resp := <-ch:
		cleanup()
		if resp == nil {
			return nil, ErrHelperRestarted
		}
		if !resp.OK {
			return nil, &CommandError{Code: resp.Code}
		}
		return resp, nil
	case <-ctx.Done():
		cleanup()
		return nil, ctx.Err()
	case <-timer.C:
		cleanup()
		return nil, fmt.Errorf("tinta-bio: comando %q sin respuesta tras %s", payload["cmd"], e.cfg.CommandTimeout)
	}
}

// ─────────────────────────── supervision ───────────────────────────

func (e *Engine) supervise(ctx context.Context) {
	backoff := e.cfg.RestartBackoffMin
	warnedMissing := false
	for {
		if ctx.Err() != nil || e.isStopping() {
			return
		}
		path := e.resolvePath()
		if path == "" {
			if !warnedMissing {
				e.cfg.Logger.Printf("[tinta-bio] helper no encontrado (TINTA_BIO_PATH / junto al sidecar) — biometría deshabilitada, reintento cada %s", e.cfg.MissingBinaryRetry)
				warnedMissing = true
			}
			if !e.sleep(ctx, e.cfg.MissingBinaryRetry) {
				return
			}
			continue
		}
		warnedMissing = false

		started := time.Now()
		err := e.runOnce(ctx, path)
		if ctx.Err() != nil || e.isStopping() {
			return
		}
		reason := "exit"
		if err != nil {
			reason = err.Error()
		}
		e.notify(func(h Handler) { h.HandleHelperDown(reason) })
		// A run that survived a while earns a fresh backoff.
		if time.Since(started) > 30*time.Second {
			backoff = e.cfg.RestartBackoffMin
		}
		e.cfg.Logger.Printf("[tinta-bio] helper murió (%s) — respawn en %s", reason, backoff)
		if !e.sleep(ctx, backoff) {
			return
		}
		backoff *= 2
		if backoff > e.cfg.RestartBackoffMax {
			backoff = e.cfg.RestartBackoffMax
		}
	}
}

// runOnce spawns the helper and blocks until it dies. Marks alive/dead,
// fails in-flight commands on death, and re-logs the helper's stderr.
func (e *Engine) runOnce(ctx context.Context, path string) error {
	cmd := exec.Command(path, e.cfg.Args...)
	cmd.Env = append(os.Environ(), e.cfg.Env...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn %s: %w", path, err)
	}
	e.cfg.Logger.Printf("[tinta-bio] helper arriba pid=%d (%s)", cmd.Process.Pid, path)

	e.mu.Lock()
	e.alive = true
	e.readerConnected = false
	e.stdin = stdin
	e.proc = cmd.Process
	e.mu.Unlock()
	e.notify(func(h Handler) { h.HandleHelperUp() })

	// stderr = logging humano del helper; re-log line by line.
	go func() {
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			e.cfg.Logger.Printf("[tinta-bio] %s", sc.Text())
		}
	}()

	// Health ping tied to this process instance: a hung helper (alive but
	// mute) gets killed so the supervisor respawns it.
	pingStop := make(chan struct{})
	go func() {
		t := time.NewTicker(e.cfg.PingInterval)
		defer t.Stop()
		for {
			select {
			case <-pingStop:
				return
			case <-t.C:
				pctx, cancel := context.WithTimeout(context.Background(), e.cfg.CommandTimeout)
				_, _, _, perr := e.Ping(pctx)
				cancel()
				if perr != nil && e.Alive() && !e.isStopping() {
					e.cfg.Logger.Printf("[tinta-bio] ping falló (%v) — reiniciando helper", perr)
					_ = cmd.Process.Kill()
					return
				}
			}
		}
	}()

	// stdout loop — one NDJSON message per line.
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var msg helperMsg
		if err := json.Unmarshal(line, &msg); err != nil {
			e.cfg.Logger.Printf("[tinta-bio] línea ilegible en stdout: %v (%.120s)", err, string(line))
			continue
		}
		e.handleMsg(&msg)
	}

	close(pingStop)
	waitErr := cmd.Wait()

	// Mark dead + fail pending round-trips.
	e.mu.Lock()
	e.alive = false
	e.readerConnected = false
	e.stdin = nil
	e.proc = nil
	pending := e.pending
	e.pending = make(map[string]chan *helperMsg)
	e.mu.Unlock()
	for _, ch := range pending {
		ch <- nil
	}
	return waitErr
}

func (e *Engine) handleMsg(msg *helperMsg) {
	switch msg.Event {
	case "result":
		e.mu.Lock()
		ch := e.pending[msg.ID]
		delete(e.pending, msg.ID)
		e.mu.Unlock()
		if ch != nil {
			ch <- msg
		} else {
			e.cfg.Logger.Printf("[tinta-bio] result huérfano id=%s (¿timeout previo?)", msg.ID)
		}
	case "reader":
		connected := msg.State == "connected"
		e.mu.Lock()
		e.readerConnected = connected
		if connected {
			e.readerName = msg.Name
			e.readerSerial = msg.Serial
		}
		e.mu.Unlock()
		name, serial := msg.Name, msg.Serial
		e.notify(func(h Handler) { h.HandleReaderState(connected, name, serial) })
	case "sample":
		fmd, quality := msg.FMD, msg.Quality
		e.notify(func(h Handler) { h.HandleSample(fmd, quality) })
	case "sample_rejected":
		code, quality := msg.Code, msg.Quality
		e.notify(func(h Handler) { h.HandleSampleRejected(code, quality) })
	case "error":
		e.cfg.Logger.Printf("[tinta-bio] error del helper code=%s detail=%s", msg.Code, msg.Detail)
	default:
		e.cfg.Logger.Printf("[tinta-bio] evento desconocido %q (forward-compat: ignorado)", msg.Event)
	}
}

// notify queues a Handler callback on the dispatch goroutine. Drops (with a
// log) if the queue is saturated — samples are human-paced, so a full queue
// means the hub is stuck, not that we need more buffer.
func (e *Engine) notify(fn func(Handler)) {
	if e.handler == nil {
		return
	}
	h := e.handler
	select {
	case e.dispatch <- func() { fn(h) }:
	default:
		e.cfg.Logger.Printf("[tinta-bio] cola de eventos llena — evento descartado")
	}
}

func (e *Engine) runDispatch() {
	for {
		select {
		case <-e.stopCh:
			return
		case fn := <-e.dispatch:
			fn()
		}
	}
}

func (e *Engine) isStopping() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.stopping
}

// sleep waits d unless ctx/stop fire first. Returns false when interrupted.
func (e *Engine) sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-e.stopCh:
		return false
	case <-t.C:
		return true
	}
}

// resolvePath finds the helper executable: explicit config, TINTA_BIO_PATH
// env, alongside the sidecar executable, cwd, then $PATH — same cascade the
// NBIS resolveBinaries used, so packaging keeps working the same way (the
// Tauri bundle drops the helper next to the sidecar .exe).
func (e *Engine) resolvePath() string {
	if e.cfg.Path != "" {
		return e.cfg.Path
	}
	if p := os.Getenv("TINTA_BIO_PATH"); p != "" {
		return p
	}
	name := "tinta-bio"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	candidates := []string{}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), name))
	}
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(cwd, name))
	}
	for _, p := range candidates {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return ""
}
