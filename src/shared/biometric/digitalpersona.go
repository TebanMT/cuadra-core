//go:build sidecar

package biometric

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png" // register PNG decoder for ExtractTemplate
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/google/uuid"

	bcrypto "github.com/cuadra/cuadra-core/src/shared/biometric/crypto"
)

// DigitalPersonaReader is the sidecar-side Reader implementation per
// ADR-004-bis: it wraps the NBIS bozorth3 and mindtct CLIs as subprocesses.
// Hardware capture itself is driven by the Tauri frontend via the
// DigitalPersona JS SDK; this struct never opens the USB device. Its job is
// (1) turn raw PNG bytes from the frontend into ANSI INCITS 378 templates
// via `mindtct -m1`, and (2) score a probe against an in-gym gallery via
// `bozorth3 -m1`.
//
// The struct is the historical Reader implementation; the name stays
// `DigitalPersonaReader` so the sidecar wiring in cmd/sidecar/main.go
// doesn't need a churn-only rename. Functionally it is the NBIS matcher.
type DigitalPersonaReader struct {
	mu sync.Mutex

	// MindtctPath / Bozorth3Path point at the binaries shipped alongside the
	// sidecar in the Tauri bundle. NewDigitalPersonaReader resolves them
	// relative to the executable on construction.
	MindtctPath  string
	Bozorth3Path string

	// WorkDir is where ExtractTemplate and Identify write transient files
	// (JPEG copies of incoming PNGs, .xyt outputs, gallery lists). Cleaned
	// per call. Defaults to os.TempDir()+"/tinta-bio".
	WorkDir string

	// GMK is how the matcher gets the per-gym master key to decrypt the
	// gallery inside Identify. Set via WithGMK at construction; nil GMK
	// means Identify will refuse to decrypt and return ErrNotAvailable.
	GMK bcrypto.GMKProvider

	// ActiveGym is the gym the operator is currently signed in to — used
	// to scope GMK lookups. Set by the sidecar auth flow via SetActiveGym;
	// uuid.Nil means "no operator signed in" and Identify returns
	// ErrNotAvailable.
	ActiveGym uuid.UUID

	// connectCBs / disconnectCBs are vestigial — hot-plug events now come
	// from the frontend (the JS SDK fires DeviceConnected/Disconnected via
	// the Lite Client). Kept on the interface for older callers; in this
	// implementation they're never fired from Go. Cleanup is tracked in a
	// future ADR.
	connectCBs    []func()
	disconnectCBs []func()
}

// NewDigitalPersonaReader builds the matcher. It searches for mindtct +
// bozorth3 alongside the sidecar executable; if either is missing, the
// matcher still constructs but Available()/ExtractTemplate/Identify will
// return ErrNotAvailable so the kiosk falls back to PIN/manual cleanly
// instead of crashing the whole sidecar at boot.
func NewDigitalPersonaReader() *DigitalPersonaReader {
	r := &DigitalPersonaReader{
		WorkDir: filepath.Join(os.TempDir(), "tinta-bio"),
	}
	r.resolveBinaries()
	if r.MindtctPath == "" || r.Bozorth3Path == "" {
		log.Printf("[biometric] NBIS binaries not found alongside sidecar — fingerprint matching disabled (kiosko cae a PIN/manual)")
	} else {
		log.Printf("[biometric] NBIS matcher ready (mindtct=%s, bozorth3=%s)", r.MindtctPath, r.Bozorth3Path)
	}
	_ = os.MkdirAll(r.WorkDir, 0o700)
	return r
}

// WithGMK wires the GMK provider used to decrypt the gallery inside
// Identify. Returns the same pointer for fluent wiring in main.go.
func (r *DigitalPersonaReader) WithGMK(p bcrypto.GMKProvider) *DigitalPersonaReader {
	r.GMK = p
	return r
}

// SetActiveGym is called by the sidecar after a successful operator login
// so the matcher knows which GMK to fetch when decrypting candidate
// templates. Passing uuid.Nil clears it.
func (r *DigitalPersonaReader) SetActiveGym(gymID uuid.UUID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ActiveGym = gymID
}

func (r *DigitalPersonaReader) resolveBinaries() {
	suffix := ""
	if isWindows() {
		suffix = ".exe"
	}
	candidates := []string{}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates, dir)
	}
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates, cwd)
	}
	for _, dir := range candidates {
		mp := filepath.Join(dir, "mindtct"+suffix)
		bp := filepath.Join(dir, "bozorth3"+suffix)
		if fileExists(mp) && fileExists(bp) {
			r.MindtctPath = mp
			r.Bozorth3Path = bp
			return
		}
	}
	// Last-resort: PATH lookup, useful in dev when the binaries live in
	// /usr/local/bin or the developer's NBIS build dir.
	if mp, err := exec.LookPath("mindtct"); err == nil {
		r.MindtctPath = mp
	}
	if bp, err := exec.LookPath("bozorth3"); err == nil {
		r.Bozorth3Path = bp
	}
}

func (r *DigitalPersonaReader) Info() ReaderInfo {
	return ReaderInfo{
		DeviceID:  "",
		Vendor:    "HID/Crossmatch",
		Model:     "U.are.U 4500 (NBIS matcher, captura en frontend)",
		Connected: r.Available(context.Background()),
	}
}

func (r *DigitalPersonaReader) OnConnect(cb func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.connectCBs = append(r.connectCBs, cb)
}

func (r *DigitalPersonaReader) OnDisconnect(cb func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.disconnectCBs = append(r.disconnectCBs, cb)
}

// Capture / Enroll are no-ops in the NBIS pipeline — capture lives in the
// frontend per ADR-004-bis §2. The interface keeps the methods so cloud and
// test paths compile unchanged.
func (*DigitalPersonaReader) Capture(_ context.Context) (*CaptureResult, error) {
	return nil, ErrCaptureMovedToFrontend
}

func (*DigitalPersonaReader) Enroll(_ context.Context, _ int) (*CaptureResult, error) {
	return nil, ErrCaptureMovedToFrontend
}

// Available reports whether the matcher has the NBIS binaries it needs.
// The kiosk loop polls this on start and after suspected disconnects to
// hide fingerprint UI when matching can't run.
func (r *DigitalPersonaReader) Available(_ context.Context) bool {
	return r.MindtctPath != "" && r.Bozorth3Path != "" &&
		fileExists(r.MindtctPath) && fileExists(r.Bozorth3Path)
}

// ExtractTemplate turns a PNG (or JPEG) from the frontend into an ANSI
// INCITS 378 template (.xyt text per `mindtct -m1`). Steps:
//
//  1. Decode the image with the Go stdlib (no CGO, no libpng).
//  2. Re-encode as 8-bit grayscale JPEG so the NBIS build (--STDLIBS, no
//     libpng) can read it via its libjpegb path.
//  3. Run `mindtct -m1 input.jpg output_root` — writes output_root.xyt.
//  4. Read the .xyt back as Bytes; QualityScore is approximated from the
//     minutiae count (≥30 minutiae → quality 80, ≥20 → 60, otherwise reject).
//
// The temp files are removed before return on success; on error we leave
// the JPEG behind for forensic debugging in WorkDir.
func (r *DigitalPersonaReader) ExtractTemplate(ctx context.Context, imageBytes []byte) (*CaptureResult, error) {
	if !r.Available(ctx) {
		return nil, ErrNotAvailable
	}
	if len(imageBytes) == 0 {
		return nil, ErrNoFingerDetected
	}

	gray, err := decodeToGray(imageBytes)
	if err != nil {
		return nil, fmt.Errorf("biometric: decode image: %w", err)
	}

	job, err := newJob(r.WorkDir, "extract")
	if err != nil {
		return nil, err
	}
	defer job.cleanup()

	jpegPath := filepath.Join(job.dir, "input.jpg")
	if err := writeGrayJPEG(jpegPath, gray); err != nil {
		return nil, fmt.Errorf("biometric: encode jpeg: %w", err)
	}

	outRoot := filepath.Join(job.dir, "tpl")
	cmd := exec.CommandContext(ctx, r.MindtctPath, "-m1", jpegPath, outRoot)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("biometric: mindtct: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}

	xytPath := outRoot + ".xyt"
	xytBytes, err := os.ReadFile(xytPath)
	if err != nil {
		return nil, ErrTemplateExtractFailed
	}
	minutiae := countLines(xytBytes)
	if minutiae < minMinutiaeForUsableTemplate {
		return nil, ErrQualityThreshold
	}
	return &CaptureResult{
		Bytes:        xytBytes,
		Format:       FormatXYTM1,
		QualityScore: qualityFromMinutiae(minutiae),
	}, nil
}

// Identify writes the probe and each (decrypted) gallery template to a
// temp dir, then runs `bozorth3 -m1 -p probe.xyt -G gallery.lis -o
// scores.out` once — bozorth iterates internally. Measured at 40ms against
// 150 socios on an M4 Pro; budget for a typical barrio gym (≤300 socios)
// stays well under 100ms.
//
// Returns ErrNoMatch when the highest score is below `threshold`. Returns
// ErrNotAvailable when the matcher isn't ready or no gym is active.
func (r *DigitalPersonaReader) Identify(
	ctx context.Context,
	probe *CaptureResult,
	gallery []EncryptedTemplate,
	threshold float64,
) (*MatchResult, error) {
	if !r.Available(ctx) {
		return nil, ErrNotAvailable
	}
	if probe == nil || len(probe.Bytes) == 0 {
		return nil, ErrNoFingerDetected
	}
	if len(gallery) == 0 {
		return nil, ErrNoMatch
	}

	r.mu.Lock()
	gymID := r.ActiveGym
	gmkProvider := r.GMK
	r.mu.Unlock()
	if gymID == uuid.Nil || gmkProvider == nil {
		return nil, ErrNotAvailable
	}
	gmk, err := gmkProvider.GetGMK(ctx, gymID)
	if err != nil {
		return nil, fmt.Errorf("biometric: get gmk: %w", err)
	}
	defer bcrypto.Zero(gmk)

	job, err := newJob(r.WorkDir, "match")
	if err != nil {
		return nil, err
	}
	defer job.cleanup()

	probePath := filepath.Join(job.dir, "probe.xyt")
	if err := os.WriteFile(probePath, probe.Bytes, 0o600); err != nil {
		return nil, err
	}

	// Decrypt and write each gallery template to its own .xyt file. Track
	// the ordered list of MemberIDs so we can map bozorth3's positional
	// output back to the right socio.
	galleryListPath := filepath.Join(job.dir, "gallery.lis")
	listFile, err := os.OpenFile(galleryListPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	memberOrder := make([]string, 0, len(gallery))
	for i, et := range gallery {
		plain, derr := bcrypto.DecryptTemplate(gmk, et.Bytes)
		if derr != nil {
			// One bad blob shouldn't fail the whole match — skip and log.
			log.Printf("[biometric] identify: skip member=%s decrypt failed: %v", et.MemberID, derr)
			continue
		}
		path := filepath.Join(job.dir, fmt.Sprintf("g_%04d.xyt", i))
		if werr := os.WriteFile(path, plain, 0o600); werr != nil {
			bcrypto.Zero(plain)
			return nil, werr
		}
		bcrypto.Zero(plain)
		if _, werr := fmt.Fprintln(listFile, path); werr != nil {
			_ = listFile.Close()
			return nil, werr
		}
		memberOrder = append(memberOrder, et.MemberID)
	}
	_ = listFile.Close()
	if len(memberOrder) == 0 {
		return nil, ErrNoMatch
	}

	scoresPath := filepath.Join(job.dir, "scores.out")
	cmd := exec.CommandContext(
		ctx, r.Bozorth3Path,
		"-m1",
		"-p", probePath,
		"-G", galleryListPath,
		"-o", scoresPath,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("biometric: bozorth3: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}

	bestIdx, bestScore, err := readBestScore(scoresPath)
	if err != nil {
		return nil, err
	}
	// Log every probe's raw score — the only way to calibrate the threshold
	// against a real reader and finger population in the field.
	log.Printf("[biometric] identify: best score=%d threshold=%.0f gallery=%d match=%t",
		bestScore, threshold, len(gallery), float64(bestScore) >= threshold)
	if float64(bestScore) < threshold {
		return nil, ErrNoMatch
	}
	if bestIdx < 0 || bestIdx >= len(memberOrder) {
		return nil, errors.New("biometric: bozorth3 returned out-of-range index")
	}
	return &MatchResult{
		MemberID: memberOrder[bestIdx],
		Score:    float64(bestScore),
	}, nil
}

// ─────────────── helpers ───────────────

const (
	// FormatXYTM1 is the Format tag we stamp on templates produced by
	// `mindtct -m1`. ANSI INCITS 378-2004 coordinate conventions, one
	// minutia per text line.
	FormatXYTM1 = "xyt_m1"

	// minMinutiaeForUsableTemplate is the floor below which we refuse the
	// extraction — UC-028 fails fast on poor captures. NIST docs note ≥20
	// minutiae are needed for reliable 1:N matching with bozorth3.
	minMinutiaeForUsableTemplate = 20
)

type job struct {
	dir string
}

func newJob(parent, kind string) (*job, error) {
	dir, err := os.MkdirTemp(parent, kind+"-*")
	if err != nil {
		return nil, err
	}
	return &job{dir: dir}, nil
}

func (j *job) cleanup() { _ = os.RemoveAll(j.dir) }

func decodeToGray(raw []byte) (*image.Gray, error) {
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		// image.Decode needs the format registered. PNG is via image/png
		// import (registered); JPEG via the blank import above.
		return nil, err
	}
	if g, ok := img.(*image.Gray); ok {
		return g, nil
	}
	bounds := img.Bounds()
	gray := image.NewGray(bounds)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			gray.Set(x, y, img.At(x, y))
		}
	}
	return gray, nil
}

// writeGrayJPEG writes a *image.Gray as baseline JPEG. Quality 95 keeps
// minutiae fidelity high; lower values measurably degrade mindtct's
// extraction rate. JPEG (not PNG) because the NBIS build uses --STDLIBS
// (no libpng) but supports libjpegb out of the box.
func writeGrayJPEG(path string, gray *image.Gray) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return jpeg.Encode(f, gray, &jpeg.Options{Quality: 95})
}

func isWindows() bool { return runtime.GOOS == "windows" }

func countLines(b []byte) int {
	n := 0
	scanner := bufio.NewScanner(bytes.NewReader(b))
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) != "" {
			n++
		}
	}
	return n
}

func qualityFromMinutiae(n int) int {
	switch {
	case n >= 60:
		return 90
	case n >= 40:
		return 80
	case n >= 30:
		return 70
	case n >= 20:
		return 60
	default:
		return 0
	}
}

// readBestScore parses bozorth3's `-o` output: one decimal integer per line,
// one line per gallery entry, in the same order as gallery.lis. Returns
// the index of the highest line and its value.
func readBestScore(path string) (int, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return -1, 0, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	bestIdx, bestScore := -1, -1
	idx := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			idx++
			continue
		}
		v, perr := strconv.Atoi(line)
		if perr != nil {
			idx++
			continue
		}
		if v > bestScore {
			bestScore = v
			bestIdx = idx
		}
		idx++
	}
	if err := scanner.Err(); err != nil {
		return -1, 0, err
	}
	return bestIdx, bestScore, nil
}

func fileExists(p string) bool {
	if p == "" {
		return false
	}
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}
