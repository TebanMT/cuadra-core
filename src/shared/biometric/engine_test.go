package biometric

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// El helper FALSO habla el protocolo NDJSON real por stdio (PROTOCOL.md)
// dentro de este mismo binario de test: cuando el Engine spawnea os.Args[0]
// con TINTA_BIO_FAKE=1, TestMain desvía a runFakeHelper antes de m.Run.
// Así el process manager se prueba contra un subproceso de verdad — pipes,
// EOF, crashes y todo — sin necesitar el .exe de Windows.
func TestMain(m *testing.M) {
	if os.Getenv("TINTA_BIO_FAKE") == "1" {
		runFakeHelper()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func runFakeHelper() {
	out := json.NewEncoder(os.Stdout)
	emit := func(v any) { _ = out.Encode(v) }

	// Igual que el helper real: anuncia el lector al arrancar.
	emit(map[string]any{"event": "reader", "state": "connected", "name": "FakeReader", "serial": "F001"})

	epoch := ""
	gallery := []GalleryCandidate{}

	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		var cmd struct {
			Cmd        string             `json:"cmd"`
			ID         string             `json:"id"`
			Epoch      string             `json:"epoch"`
			Candidates []GalleryCandidate `json:"candidates"`
			Probe      string             `json:"probe"`
			Max        int                `json:"max"`
			Fmds       []string           `json:"fmds"`
			FMD        string             `json:"fmd"`
		}
		if err := json.Unmarshal(sc.Bytes(), &cmd); err != nil {
			continue
		}
		switch cmd.Cmd {
		case "ping":
			if os.Getenv("TINTA_BIO_FAKE_CRASH_ON_PING") == "1" {
				os.Exit(3)
			}
			emit(map[string]any{"event": "result", "id": cmd.ID, "ok": true,
				"state": "connected", "galleryEpoch": epoch, "gallerySize": len(gallery)})
		case "gallery":
			epoch = cmd.Epoch
			gallery = cmd.Candidates
			emit(map[string]any{"event": "result", "id": cmd.ID, "ok": true, "gallerySize": len(gallery)})
		case "identify":
			if os.Getenv("TINTA_BIO_FAKE_CRASH_ON_IDENTIFY") == "1" {
				os.Exit(3)
			}
			max := cmd.Max
			if max <= 0 {
				max = 1
			}
			matches := []string{}
			for _, c := range gallery {
				if c.FMD == cmd.Probe && len(matches) < max {
					matches = append(matches, c.Ref)
				}
			}
			emit(map[string]any{"event": "result", "id": cmd.ID, "ok": true,
				"matches": matches, "galleryEpoch": epoch})
		case "enroll":
			if os.Getenv("TINTA_BIO_FAKE_ENROLL_FAIL") == "1" {
				emit(map[string]any{"event": "result", "id": cmd.ID, "ok": false, "code": "DP_ENROLLMENT_INVALID_SET"})
				continue
			}
			if len(cmd.Fmds) == 0 {
				emit(map[string]any{"event": "result", "id": cmd.ID, "ok": false, "code": "DP_ENROLLMENT_INVALID_SET"})
				continue
			}
			emit(map[string]any{"event": "result", "id": cmd.ID, "ok": true, "fmd": cmd.Fmds[0]})
		case "_sample":
			// Comando test-only: inyecta un dedazo espontáneo (sin result).
			emit(map[string]any{"event": "sample", "fmd": cmd.FMD, "quality": "DP_QUALITY_GOOD"})
		case "_sample_rejected":
			emit(map[string]any{"event": "sample_rejected", "code": "no_data", "quality": "DP_QUALITY_TOO_LIGHT"})
		}
	}
	// stdin EOF = shutdown limpio, igual que el helper real.
}

// ───────────────────────── harness ─────────────────────────

type testHandler struct {
	ups      chan struct{}
	downs    chan string
	samples  chan string
	rejected chan string
	readers  chan bool
}

func newTestHandler() *testHandler {
	return &testHandler{
		ups:      make(chan struct{}, 8),
		downs:    make(chan string, 8),
		samples:  make(chan string, 8),
		rejected: make(chan string, 8),
		readers:  make(chan bool, 8),
	}
}

func (h *testHandler) HandleSample(fmd, _ string)            { h.samples <- fmd }
func (h *testHandler) HandleSampleRejected(code, _ string)   { h.rejected <- code }
func (h *testHandler) HandleReaderState(c bool, _, _ string) { h.readers <- c }
func (h *testHandler) HandleHelperUp()                       { h.ups <- struct{}{} }
func (h *testHandler) HandleHelperDown(reason string)        { h.downs <- reason }

func newFakeEngine(t *testing.T, extraEnv ...string) (*Engine, *testHandler) {
	t.Helper()
	h := newTestHandler()
	e := NewEngine(EngineConfig{
		Path:              os.Args[0],
		Env:               append([]string{"TINTA_BIO_FAKE=1"}, extraEnv...),
		Logger:            log.New(os.Stderr, "[engine-test] ", 0),
		CommandTimeout:    3 * time.Second,
		RestartBackoffMin: 50 * time.Millisecond,
		RestartBackoffMax: 200 * time.Millisecond,
		PingInterval:      time.Hour, // el ping de salud no juega en estos tests
	})
	e.SetHandler(h)
	return e, h
}

func waitCh[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(3 * time.Second):
		t.Fatalf("timeout esperando %s", what)
		panic("unreachable")
	}
}

// sendRaw escribe una línea cruda al stdin del helper (comandos test-only
// del fake que no producen result).
func sendRaw(t *testing.T, e *Engine, payload map[string]any) {
	t.Helper()
	e.mu.Lock()
	stdin := e.stdin
	e.mu.Unlock()
	if stdin == nil {
		t.Fatalf("helper sin stdin (¿no está vivo?)")
	}
	line, _ := json.Marshal(payload)
	e.writeMu.Lock()
	_, err := stdin.Write(append(line, '\n'))
	e.writeMu.Unlock()
	if err != nil {
		t.Fatalf("sendRaw: %v", err)
	}
}

// ───────────────────────── tests ─────────────────────────

// TestEngine_RoundTripAndEvents: el ciclo completo contra el fake — spawn,
// reader event, gallery/ping/identify/enroll, sample espontáneo y shutdown
// limpio por EOF.
func TestEngine_RoundTripAndEvents(t *testing.T) {
	e, h := newFakeEngine(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.Start(ctx)
	defer e.Stop()

	waitCh(t, h.ups, "helper up")
	if !waitCh(t, h.readers, "reader connected") {
		t.Fatalf("primer evento de lector debió ser connected")
	}
	if !e.Alive() || !e.Connected() {
		t.Fatalf("Alive/Connected deben ser true con el fake arriba")
	}
	if info := e.Info(); info.Model != "FakeReader" || info.DeviceID != "F001" {
		t.Errorf("Info no refleja el reader event: %+v", info)
	}

	if err := e.SetGallery(ctx, "e1", []GalleryCandidate{{Ref: "r1", FMD: "fmd-a"}}); err != nil {
		t.Fatalf("SetGallery: %v", err)
	}
	state, epoch, size, err := e.Ping(ctx)
	if err != nil || state != "connected" || epoch != "e1" || size != 1 {
		t.Fatalf("Ping = (%s,%s,%d,%v), want (connected,e1,1,nil)", state, epoch, size, err)
	}

	matches, epoch, err := e.Identify(ctx, "fmd-a", 0, 0)
	if err != nil || epoch != "e1" || len(matches) != 1 || matches[0] != "r1" {
		t.Fatalf("Identify hit = (%v,%s,%v)", matches, epoch, err)
	}
	matches, _, err = e.Identify(ctx, "fmd-desconocido", 0, 0)
	if err != nil || len(matches) != 0 {
		t.Fatalf("Identify miss = (%v,%v), want vacío", matches, err)
	}

	fmd, err := e.EnrollCombine(ctx, []string{"x", "y", "z"})
	if err != nil || fmd != "x" {
		t.Fatalf("EnrollCombine = (%q,%v), want x", fmd, err)
	}

	// Evento espontáneo → Handler.
	sendRaw(t, e, map[string]any{"cmd": "_sample", "fmd": "dedazo-1"})
	if got := waitCh(t, h.samples, "sample"); got != "dedazo-1" {
		t.Errorf("sample = %q", got)
	}
	sendRaw(t, e, map[string]any{"cmd": "_sample_rejected"})
	if got := waitCh(t, h.rejected, "sample_rejected"); got != "no_data" {
		t.Errorf("sample_rejected code = %q", got)
	}

	// Stop = stdin EOF → el fake sale solo; sin evento down (es un stop).
	e.Stop()
	deadline := time.Now().Add(2 * time.Second)
	for e.Alive() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if e.Alive() {
		t.Fatalf("el helper debió morir con el EOF de stdin")
	}
}

// TestEngine_CommandError: un result ok=false llega como *CommandError con
// el código del helper (contrato para DP_ENROLLMENT_INVALID_SET).
func TestEngine_CommandError(t *testing.T) {
	e, h := newFakeEngine(t, "TINTA_BIO_FAKE_ENROLL_FAIL=1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.Start(ctx)
	defer e.Stop()
	waitCh(t, h.ups, "helper up")

	_, err := e.EnrollCombine(ctx, []string{"a", "b", "c"})
	var cmdErr *CommandError
	if !errors.As(err, &cmdErr) || cmdErr.Code != "DP_ENROLLMENT_INVALID_SET" {
		t.Fatalf("EnrollCombine err = %v, want CommandError{DP_ENROLLMENT_INVALID_SET}", err)
	}
}

// TestEngine_RestartAfterCrash: el helper muere a media operación → el
// comando en vuelo falla, avisa down, y el supervisor lo revive con backoff.
func TestEngine_RestartAfterCrash(t *testing.T) {
	e, h := newFakeEngine(t, "TINTA_BIO_FAKE_CRASH_ON_IDENTIFY=1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.Start(ctx)
	defer e.Stop()
	waitCh(t, h.ups, "helper up (1)")

	if _, _, err := e.Identify(ctx, "boom", 0, 0); err == nil {
		t.Fatalf("identify durante crash debe fallar")
	}
	waitCh(t, h.downs, "helper down")
	waitCh(t, h.ups, "helper up (2) — restart")

	// El proceso nuevo responde normal.
	pctx, pcancel := context.WithTimeout(ctx, 2*time.Second)
	defer pcancel()
	if _, _, _, err := e.Ping(pctx); err != nil {
		t.Fatalf("ping tras restart: %v", err)
	}
}

// TestEngine_MissingBinary: sin ejecutable no hay pánico ni hot-loop — el
// engine queda en no-disponible y los comandos fallan rápido.
func TestEngine_MissingBinary(t *testing.T) {
	h := newTestHandler()
	e := NewEngine(EngineConfig{
		Path:               filepath.Join(t.TempDir(), "no-existe"),
		Logger:             log.New(os.Stderr, "[engine-test] ", 0),
		CommandTimeout:     200 * time.Millisecond,
		RestartBackoffMin:  20 * time.Millisecond,
		RestartBackoffMax:  40 * time.Millisecond,
		MissingBinaryRetry: 50 * time.Millisecond,
	})
	e.SetHandler(h)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.Start(ctx)
	defer e.Stop()

	time.Sleep(150 * time.Millisecond)
	if e.Alive() || e.Connected() {
		t.Fatalf("sin binario el engine no puede estar vivo")
	}
	if err := e.SetGallery(ctx, "e1", nil); !errors.Is(err, ErrNotAvailable) {
		t.Fatalf("SetGallery sin helper = %v, want ErrNotAvailable", err)
	}
}

// TestEngine_StderrRelog humo: el fake no escribe stderr, pero el path de
// spawn con args extra no debe romper nada (cubre EngineConfig.Args).
func TestEngine_SpawnWithArgs(t *testing.T) {
	e, h := newFakeEngine(t)
	e.cfg.Args = []string{"-ignored-flag"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.Start(ctx)
	defer e.Stop()
	waitCh(t, h.ups, "helper up")
	if _, _, _, err := e.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
}
