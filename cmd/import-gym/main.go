//go:build server

// Package main — `cmd/import-gym` migrates a phpMyAdmin MariaDB dump from a
// legacy gym system into Cuadra's Postgres schema.
//
// Usage:
//
//	DATABASE_URL=postgresql://... \
//	    go run -tags server ./cmd/import-gym \
//	        --gym-id <uuid>           # destination gym (must already exist)
//	        --owner-user-id <uuid>    # operator/creator stamp for legacy rows
//	        --dump ./gym.sql          # path to the MariaDB dump
//	        --reset                   # delete existing data for the gym first
//	        --dry-run                 # parse + print stats, no writes
//
// What it imports (legacy → Cuadra):
//
//	membresia              → membership_types
//	producto               → products
//	socio                  → members  (status from `estado`; legacy `clave` se
//	                         preserva en notes — NO se backfillea member_number,
//	                         lo asigna AssignMemberNumber al primer alta/check-in)
//	sociomembresia         → memberships
//	sociomembresia_pago    → payments (concept=membership)
//	salida + detallesalida → sales + sale_items + payments (concept=product)
//	entrada + detalleentrada → stock_movements (movement_type=restock)
//	visita                 → checkins (method=manual, result=allowed_active)
//	cut                    → cash_close_events
//
// What it skips: clase, clase_socio, registro, control_*, configuracion,
// rptviews, photos/fingerprint blobs (binary). All catalog tables (estado,
// ctipomembresia, ctypepayment) are mapped inline as enums.
//
// IDs from the legacy dump are mapped to Cuadra UUIDs deterministically via
// SHA-1, so re-running the importer (with --reset) produces the same UUIDs.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	infraDB "github.com/cuadra/cuadra-core/infraestructure/db"

	billingModels "github.com/cuadra/cuadra-core/src/modules/billing/infraestructure/db/models"
	chkModels "github.com/cuadra/cuadra-core/src/modules/checkins/infraestructure/db/models"
	gymModels "github.com/cuadra/cuadra-core/src/modules/gyms/infraestructure/db/models"
	memModels "github.com/cuadra/cuadra-core/src/modules/members/infraestructure/db/models"
	prodModels "github.com/cuadra/cuadra-core/src/modules/products/infraestructure/db/models"
	usersModels "github.com/cuadra/cuadra-core/src/modules/users/infraestructure/db/models"
	phonepkg "github.com/cuadra/cuadra-core/src/shared/phone"
	syncShared "github.com/cuadra/cuadra-core/src/shared/sync"
)

// uuidNS is the namespace used to mint deterministic UUIDs from legacy ints.
// Initialised in main() because uuid.NewSHA1 isn't a constant expression.
// Private to this importer so re-running gives stable IDs but doesn't collide
// with any other UUID-generation strategy in the codebase.
var uuidNS uuid.UUID

func main() {
	uuidNS = uuid.NewSHA1(uuid.NameSpaceURL, []byte("cuadra/import-gym"))

	var (
		gymIDFlag       = flag.String("gym-id", "", "destination gym UUID (defaults to deterministic from dump)")
		ownerIDFlag     = flag.String("owner-user-id", "", "owner user UUID (defaults to deterministic from dump)")
		dumpPath        = flag.String("dump", "./gym.sql", "path to the MariaDB dump")
		reset           = flag.Bool("reset", false, "delete existing data for the gym before importing")
		dryRun          = flag.Bool("dry-run", false, "parse + print stats, do not write")
		createMissing   = flag.Bool("create-missing", false, "create destination gym + owner user if they don't exist (uses legacy configuracion + the email/password flags)")
		ownerEmailFlag  = flag.String("owner-email", "", "email for the owner user (required with --create-missing if user doesn't exist)")
		ownerPwdFlag    = flag.String("owner-password", "", "password for the owner user (required with --create-missing if user doesn't exist)")
		gymNameOverride = flag.String("gym-name", "", "override the gym name (default: legacy configuracion.NombreGimnacio)")
		backfill        = flag.Bool("backfill-projector", false, "skip the dump entirely and project every existing sync_entities row for the gym into its domain table (one-time fix for historical pushes that landed before the projector was wired)")
	)
	flag.Parse()

	dumpBytes, err := os.ReadFile(*dumpPath)
	if err != nil {
		log.Fatalf("read dump: %v", err)
	}
	dump := string(dumpBytes)

	src := parseDump(dump)
	src.print()

	// Resolve destination gym + owner UUIDs. If the flags are missing we
	// derive deterministic UUIDs from the dump (so re-runs hit the same
	// rows). The fixture row gets created on demand under --create-missing.
	gymID := mustParseOrDerive(*gymIDFlag, "gym", src)
	ownerID := mustParseOrDerive(*ownerIDFlag, "owner", src)

	if *dryRun {
		log.Printf("dry-run: gym_id=%s owner_id=%s — nothing was written", gymID, ownerID)
		return
	}

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Fatalf("DATABASE_URL not set")
	}
	db := infraDB.InitPostgres(dsn)
	defer infraDB.ClosePostgres()

	if *backfill {
		if err := db.Transaction(func(tx *gorm.DB) error {
			return backfillProjector(tx, gymID)
		}); err != nil {
			log.Fatalf("backfill: %v", err)
		}
		log.Printf("backfill: ok — gym_id=%s", gymID)
		return
	}

	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := ensureGymAndOwner(tx, src, gymID, ownerID, *createMissing, *gymNameOverride, *ownerEmailFlag, *ownerPwdFlag); err != nil {
			return err
		}
		if *reset {
			if err := wipeGym(tx, gymID); err != nil {
				return fmt.Errorf("reset: %w", err)
			}
		}
		return importAll(tx, src, gymID, ownerID)
	}); err != nil {
		log.Fatalf("import: %v", err)
	}
	log.Printf("import: ok — gym_id=%s owner_id=%s", gymID, ownerID)
}

// backfillProjector replays every sync_entities row for the gym through the
// projector, populating the domain tables. Used once after wiring project()
// into the push path — historical pushes that landed before the wiring exist
// only in sync_entities and need to be replayed onto the domain tables.
func backfillProjector(tx *gorm.DB, gymID uuid.UUID) error {
	type row struct {
		EntityType string    `gorm:"column:entity_type"`
		EntityID   uuid.UUID `gorm:"column:entity_id"`
		Payload    []byte    `gorm:"column:payload"`
	}
	var rows []row
	if err := tx.Raw(`
		SELECT entity_type, entity_id, payload::text AS payload
		FROM sync_entities WHERE gym_id = ?
		ORDER BY server_updated_at`, gymID).Scan(&rows).Error; err != nil {
		return err
	}
	counts := map[string]int{}
	for _, r := range rows {
		if err := syncShared.Project(tx, r.EntityType, gymID, r.EntityID, r.Payload); err != nil {
			return fmt.Errorf("project %s/%s: %w", r.EntityType, r.EntityID, err)
		}
		counts[r.EntityType]++
	}
	for et, n := range counts {
		log.Printf("backfill: %s=%d", et, n)
	}
	return nil
}

func mustParseOrDerive(flagVal, kind string, src sourceData) uuid.UUID {
	if flagVal != "" {
		id, err := uuid.Parse(flagVal)
		if err != nil {
			log.Fatalf("invalid --%s id: %v", kind, err)
		}
		return id
	}
	// Derive deterministic — same dump → same UUIDs.
	gymKey := "default"
	if src.config != nil && src.config.NombreGimnacio != "" {
		gymKey = src.config.NombreGimnacio
	}
	return uuid.NewSHA1(uuidNS, []byte(kind+":"+gymKey))
}

// ensureGymAndOwner is the pre-flight: when the destination rows already
// exist we use them as-is. When they're missing we either create them from
// the legacy `configuracion`/`usuario` tables (with --create-missing) or
// fail with a clear error so the user knows what to do next.
func ensureGymAndOwner(tx *gorm.DB, src sourceData, gymID, ownerID uuid.UUID, createMissing bool, gymNameOverride, ownerEmail, ownerPassword string) error {
	gymExists, err := exists(tx, "gyms", gymID)
	if err != nil {
		return err
	}
	userExists, err := exists(tx, "users", ownerID)
	if err != nil {
		return err
	}
	if gymExists && userExists {
		return nil
	}
	if !createMissing {
		var missing []string
		if !gymExists {
			missing = append(missing, fmt.Sprintf("gyms.id=%s", gymID))
		}
		if !userExists {
			missing = append(missing, fmt.Sprintf("users.id=%s", ownerID))
		}
		return fmt.Errorf("destination row(s) not found: %s. Either pass --gym-id / --owner-user-id pointing at an existing pair, or re-run with --create-missing --owner-email=… --owner-password=…",
			strings.Join(missing, ", "))
	}

	now := time.Now().UTC()
	if !gymExists {
		name := gymNameOverride
		if name == "" && src.config != nil && src.config.NombreGimnacio != "" {
			name = src.config.NombreGimnacio
		}
		if name == "" {
			name = "Gym importado"
		}
		var (
			city *string
			wa   *string
			rfc  *string
		)
		if src.config != nil {
			if v := strings.TrimSpace(src.config.Domicilio); v != "" {
				city = &v
			}
			if v := phonepkg.Normalize(src.config.Telefono); v != "" {
				wa = &v
			}
			if v := strings.TrimSpace(src.config.RFC); v != "" {
				rfc = &v
			}
		}
		if err := tx.Create(&gymModels.GymModel{
			ID: gymID, GymID: gymID, Version: 1,
			CreatedAt: now, UpdatedAt: now,
			Name: &name, City: city, WhatsApp: wa, RFC: rfc,
			Country: "MX", Timezone: "America/Mexico_City",
			PaymentMethods:     `["cash","transfer","card"]`,
			KioskSettings:      `{}`,
			SubscriptionPlan:   "trial",
			SubscriptionStatus: "active",
			SetupCompletedAt:   &now,
		}).Error; err != nil {
			return fmt.Errorf("create gym: %w", err)
		}
		log.Printf("created gym %s (%s)", gymID, name)
	}
	if !userExists {
		if ownerEmail == "" || ownerPassword == "" {
			return fmt.Errorf("--create-missing requires --owner-email and --owner-password")
		}
		fullName := "Owner"
		if len(src.users) > 0 && src.users[0].Nombre != "" {
			fullName = src.users[0].Nombre
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(ownerPassword), 12)
		if err != nil {
			return fmt.Errorf("hash password: %w", err)
		}
		if err := tx.Create(&usersModels.UserModel{
			ID: ownerID, GymID: gymID, Version: 1,
			CreatedAt: now, UpdatedAt: now,
			Email:        strings.ToLower(strings.TrimSpace(ownerEmail)),
			PasswordHash: string(hash),
			FullName:     fullName, Role: "owner", Active: true,
		}).Error; err != nil {
			return fmt.Errorf("create owner: %w", err)
		}
		log.Printf("created owner %s (%s)", ownerID, ownerEmail)
	}
	return nil
}

func exists(tx *gorm.DB, table string, id uuid.UUID) (bool, error) {
	var n int64
	if err := tx.Table(table).Where("id = ?", id).Count(&n).Error; err != nil {
		return false, err
	}
	return n > 0, nil
}

// ── source rows ─────────────────────────────────────────────────────────────

type sourceData struct {
	config          *srcConfiguracion
	users           []srcUsuario
	memberships     []srcMembresia
	products        []srcProducto
	socios          []srcSocio
	socioMembs      []srcSocioMembresia
	socioMembsPagos []srcSocioMembresiaPago
	salidas         []srcSalida
	detSalidas      []srcDetalleSalida
	entradas        []srcEntrada
	detEntradas     []srcDetalleEntrada
	visitas         []srcVisita
	cuts            []srcCut
}

type srcConfiguracion struct {
	NombreGimnacio string
	Domicilio      string
	Telefono       string
	RFC            string
}

type srcUsuario struct {
	ID     int
	Estado int
	Login  string
	Nombre string
}

func (s *sourceData) print() {
	log.Printf("parsed: membresias=%d productos=%d socios=%d socioMembs=%d pagos=%d salidas=%d detSalidas=%d entradas=%d detEntradas=%d visitas=%d cuts=%d",
		len(s.memberships), len(s.products), len(s.socios), len(s.socioMembs),
		len(s.socioMembsPagos), len(s.salidas), len(s.detSalidas),
		len(s.entradas), len(s.detEntradas), len(s.visitas), len(s.cuts))
}

type srcMembresia struct {
	ID                   int
	Nombre               string
	Estado               int
	FechaCreacion        *time.Time
	Precio               float64
	Meses, Semanas, Dias int
	IDTipoMembresia      int
}

type srcProducto struct {
	ID            int
	Nombre        string
	Descripcion   string
	Estado        int
	FechaCreacion *time.Time
	Precio        float64
	Costo         float64
	Code          string
}

type srcSocio struct {
	ID                       int
	Estado                   int
	FechaCreacion            *time.Time
	Nombre, Paterno, Materno string
	Telefono                 string
	Observaciones            string
	Clave                    string
	FechaNacimiento          *time.Time
	Correo                   string
}

type srcSocioMembresia struct {
	ID                   int
	Estado               int
	FechaCreacion        *time.Time
	IDSocio              int
	IDMembresia          int
	Precio               float64
	FechaInicioMembresia *time.Time
	EstadoMembresia      string
	Meses, Semanas, Dias int
	IDTipoMembresia      int
	Vencimiento          *time.Time
}

type srcSocioMembresiaPago struct {
	ID               int
	Observacion      string
	Folio            int
	IDSocioMembresia int
	Fecha            *time.Time
	Estado           int
	Importe          float64
	IDTypePayment    int
}

type srcSalida struct {
	ID            int
	Estado        int
	FechaCreacion *time.Time
	Total         float64
	IDTypePayment int
}

type srcDetalleSalida struct {
	ID             int
	IDProducto     int
	PrecioUnitario float64
	IDSalida       int
}

type srcEntrada struct {
	ID            int
	Estado        int
	FechaCreacion *time.Time
	Total         float64
	IDTypePayment int
}

type srcDetalleEntrada struct {
	ID            int
	IDProducto    int
	CostoUnitario float64
	IDEntrada     int
}

type srcVisita struct {
	ID            int
	IDSocio       int
	FechaCreacion *time.Time
	PrecioVisita  float64
}

type srcCut struct {
	ID          int
	Date        *time.Time
	DateStart   *time.Time
	DateEnd     *time.Time
	Estado      int
	CashStart   float64
	CashEnd     float64
	Income      float64
	Egress      float64
	Observation string
}

// ── parser ──────────────────────────────────────────────────────────────────

// parseDump scans the SQL dump for INSERT blocks of the tables we care about
// and extracts their row tuples as raw values. Skips DDL, blobs, views, etc.
func parseDump(dump string) sourceData {
	var s sourceData
	tables := map[string]func([]value){
		"configuracion":       func(v []value) { c := parseConfiguracion(v); s.config = &c },
		"usuario":             func(v []value) { s.users = append(s.users, parseUsuario(v)) },
		"membresia":           func(v []value) { s.memberships = append(s.memberships, parseMembresia(v)) },
		"producto":            func(v []value) { s.products = append(s.products, parseProducto(v)) },
		"socio":               func(v []value) { s.socios = append(s.socios, parseSocio(v)) },
		"sociomembresia":      func(v []value) { s.socioMembs = append(s.socioMembs, parseSocioMembresia(v)) },
		"sociomembresia_pago": func(v []value) { s.socioMembsPagos = append(s.socioMembsPagos, parseSocioMembresiaPago(v)) },
		"salida":              func(v []value) { s.salidas = append(s.salidas, parseSalida(v)) },
		"detallesalida":       func(v []value) { s.detSalidas = append(s.detSalidas, parseDetalleSalida(v)) },
		"entrada":             func(v []value) { s.entradas = append(s.entradas, parseEntrada(v)) },
		"detalleentrada":      func(v []value) { s.detEntradas = append(s.detEntradas, parseDetalleEntrada(v)) },
		"visita":              func(v []value) { s.visitas = append(s.visitas, parseVisita(v)) },
		"cut":                 func(v []value) { s.cuts = append(s.cuts, parseCut(v)) },
	}
	streamInserts(dump, tables)
	return s
}

// streamInserts walks the dump line by line. When it hits
// `INSERT INTO `<table>` (...) VALUES`, it then reads tuples until a
// terminating `;` and dispatches each to the registered handler.
func streamInserts(dump string, tables map[string]func([]value)) {
	lines := strings.Split(dump, "\n")
	i := 0
	for i < len(lines) {
		line := lines[i]
		tableName, ok := matchInsertHeader(line)
		i++
		if !ok {
			continue
		}
		handler, want := tables[tableName]
		// Skip ahead until `;` even if we don't want this table — keeps the
		// outer loop simple and proves we exited the block cleanly.
		var buf strings.Builder
		// First tuple may be on the same line after VALUES, but in this dump
		// VALUES sits on its own line — be safe and append from the next line.
		for i < len(lines) {
			row := lines[i]
			i++
			buf.WriteString(row)
			buf.WriteByte('\n')
			if strings.HasSuffix(strings.TrimRight(row, " \t\r"), ";") {
				break
			}
		}
		if !want {
			continue
		}
		for _, tup := range splitTuples(buf.String()) {
			handler(parseTupleValues(tup))
		}
	}
}

// matchInsertHeader returns the table name if `line` is the header of an
// INSERT block we should consume.
func matchInsertHeader(line string) (string, bool) {
	const prefix = "INSERT INTO `"
	if !strings.HasPrefix(line, prefix) {
		return "", false
	}
	rest := line[len(prefix):]
	end := strings.IndexByte(rest, '`')
	if end < 0 {
		return "", false
	}
	return rest[:end], true
}

// splitTuples receives the accumulated body of an INSERT (everything after
// VALUES, terminating in `;`) and returns one string per `(...)` tuple.
// Respects single-quoted strings, hex literals, bit literals.
func splitTuples(body string) []string {
	var out []string
	var cur strings.Builder
	inString := false
	depth := 0
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch {
		case inString:
			cur.WriteByte(c)
			if c == '\\' && i+1 < len(body) {
				cur.WriteByte(body[i+1])
				i++
				continue
			}
			if c == '\'' {
				// Doubled '' inside string is an escaped quote.
				if i+1 < len(body) && body[i+1] == '\'' {
					cur.WriteByte(body[i+1])
					i++
					continue
				}
				inString = false
			}
		case c == '\'':
			inString = true
			cur.WriteByte(c)
		case c == '(' && depth == 0:
			depth++
			cur.Reset()
		case c == '(':
			depth++
			cur.WriteByte(c)
		case c == ')' && depth == 1:
			depth--
			out = append(out, cur.String())
			cur.Reset()
		case c == ')':
			depth--
			cur.WriteByte(c)
		case depth == 0:
			// Outside any tuple — separators, whitespace, terminating semicolon.
		default:
			cur.WriteByte(c)
		}
	}
	return out
}

// ── tuple value tokenizer ───────────────────────────────────────────────────

type valueKind int

const (
	vNull valueKind = iota
	vString
	vNumber
	vHex
	vBit
)

type value struct {
	kind valueKind
	s    string // for string/number/hex/bit, the raw text without delimiters
}

func (v value) isNull() bool { return v.kind == vNull }

func (v value) string() string {
	if v.kind == vString {
		return v.s
	}
	if v.kind == vNumber {
		return v.s
	}
	return ""
}

func (v value) int() int {
	if v.kind != vNumber {
		return 0
	}
	n, _ := strconv.Atoi(v.s)
	return n
}

func (v value) float() float64 {
	if v.kind != vNumber {
		return 0
	}
	f, _ := strconv.ParseFloat(v.s, 64)
	return f
}

func (v value) datetime() *time.Time {
	if v.kind != vString || v.s == "" {
		return nil
	}
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02"} {
		t, err := time.ParseInLocation(layout, v.s, time.UTC)
		if err == nil {
			return &t
		}
	}
	return nil
}

// parseTupleValues splits a tuple body (the text between the outer parens)
// into typed values. Handles strings, numbers, NULL, 0x.. blobs, b'..' bits.
func parseTupleValues(body string) []value {
	var out []value
	i := 0
	n := len(body)
	for i < n {
		// skip whitespace + leading commas
		for i < n && (body[i] == ' ' || body[i] == '\t' || body[i] == '\n' || body[i] == '\r' || body[i] == ',') {
			i++
		}
		if i >= n {
			break
		}
		c := body[i]
		switch {
		case c == '\'':
			// quoted string — consume until matching unescaped '
			i++
			start := i
			var b strings.Builder
			for i < n {
				ch := body[i]
				if ch == '\\' && i+1 < n {
					b.WriteByte(body[i+1])
					i += 2
					continue
				}
				if ch == '\'' {
					if i+1 < n && body[i+1] == '\'' {
						b.WriteByte('\'')
						i += 2
						continue
					}
					break
				}
				b.WriteByte(ch)
				i++
			}
			_ = start
			i++ // closing quote
			out = append(out, value{kind: vString, s: b.String()})
		case (c == 'N' || c == 'n') && hasPrefixCI(body[i:], "NULL"):
			i += 4
			out = append(out, value{kind: vNull})
		case c == '0' && i+1 < n && (body[i+1] == 'x' || body[i+1] == 'X'):
			i += 2
			start := i
			for i < n && isHex(body[i]) {
				i++
			}
			out = append(out, value{kind: vHex, s: body[start:i]})
		case c == 'b' && i+1 < n && body[i+1] == '\'':
			i += 2
			start := i
			for i < n && body[i] != '\'' {
				i++
			}
			out = append(out, value{kind: vBit, s: body[start:i]})
			if i < n {
				i++
			}
		default:
			// number (int, decimal, optional sign)
			start := i
			if body[i] == '+' || body[i] == '-' {
				i++
			}
			for i < n && (body[i] == '.' || (body[i] >= '0' && body[i] <= '9')) {
				i++
			}
			out = append(out, value{kind: vNumber, s: body[start:i]})
		}
	}
	return out
}

func isHex(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')
}

func hasPrefixCI(s, prefix string) bool {
	if len(s) < len(prefix) {
		return false
	}
	return strings.EqualFold(s[:len(prefix)], prefix)
}

// ── per-table parsers ───────────────────────────────────────────────────────

// Column orders below mirror the legacy dump exactly. If a column is added
// to the source schema, the slot has to grow here too.

func parseConfiguracion(v []value) srcConfiguracion {
	r := srcConfiguracion{}
	if len(v) > 1 {
		r.NombreGimnacio = v[1].string()
	}
	if len(v) > 2 {
		r.Domicilio = v[2].string()
	}
	if len(v) > 3 {
		r.Telefono = v[3].string()
	}
	if len(v) > 8 {
		r.RFC = v[8].string()
	}
	return r
}

func parseUsuario(v []value) srcUsuario {
	r := srcUsuario{}
	r.ID = v[0].int()
	r.Estado = v[1].int()
	r.Login = v[2].string()
	r.Nombre = v[3].string()
	return r
}

func parseMembresia(v []value) srcMembresia {
	r := srcMembresia{}
	r.ID = v[0].int()
	r.Nombre = v[1].string()
	r.Estado = v[2].int()
	r.FechaCreacion = v[3].datetime()
	r.Precio = v[4].float()
	// v[5] = idUsuarioCreo (skip)
	r.Meses = v[6].int()
	// v[7]/v[8] = horaInicio/horaFinal (skip)
	r.IDTipoMembresia = v[9].int()
	r.Semanas = v[10].int()
	r.Dias = v[11].int()
	return r
}

func parseProducto(v []value) srcProducto {
	r := srcProducto{}
	r.ID = v[0].int()
	r.Nombre = v[1].string()
	r.Descripcion = v[2].string()
	r.Estado = v[3].int()
	r.FechaCreacion = v[4].datetime()
	r.Precio = v[5].float()
	// v[6] = idUsuarioCreo
	r.Costo = v[7].float()
	r.Code = v[8].string()
	return r
}

func parseSocio(v []value) srcSocio {
	r := srcSocio{}
	r.ID = v[0].int()
	r.Estado = v[1].int()
	r.FechaCreacion = v[2].datetime()
	r.Nombre = v[3].string()
	r.Paterno = v[4].string()
	r.Materno = v[5].string()
	r.Telefono = v[6].string()
	r.Observaciones = v[7].string()
	// v[8] idUsuarioCreo, v[9] foto blob (skipped)
	r.Clave = v[10].string()
	// v[11] huella blob (skipped)
	r.FechaNacimiento = v[12].datetime()
	r.Correo = v[13].string()
	return r
}

func parseSocioMembresia(v []value) srcSocioMembresia {
	r := srcSocioMembresia{}
	r.ID = v[0].int()
	r.Estado = v[1].int()
	r.FechaCreacion = v[2].datetime()
	// v[3] idUsuarioCreo
	r.IDSocio = v[4].int()
	r.IDMembresia = v[5].int()
	r.Precio = v[6].float()
	r.FechaInicioMembresia = v[7].datetime()
	r.EstadoMembresia = v[8].string()
	r.Meses = v[9].int()
	r.Semanas = v[10].int()
	r.Dias = v[11].int()
	r.IDTipoMembresia = v[12].int()
	r.Vencimiento = v[13].datetime()
	return r
}

func parseSocioMembresiaPago(v []value) srcSocioMembresiaPago {
	r := srcSocioMembresiaPago{}
	r.ID = v[0].int()
	r.Observacion = v[1].string()
	r.Folio = v[2].int()
	r.IDSocioMembresia = v[3].int()
	r.Fecha = v[4].datetime()
	r.Estado = v[5].int()
	// v[6] idUsuarioCreo
	r.Importe = v[7].float()
	r.IDTypePayment = v[8].int()
	return r
}

func parseSalida(v []value) srcSalida {
	r := srcSalida{}
	r.ID = v[0].int()
	r.Estado = v[1].int()
	r.FechaCreacion = v[2].datetime()
	r.Total = v[3].float()
	// v[4] idUsuarioCreo
	r.IDTypePayment = v[5].int()
	return r
}

func parseDetalleSalida(v []value) srcDetalleSalida {
	r := srcDetalleSalida{}
	r.ID = v[0].int()
	r.IDProducto = v[1].int()
	r.PrecioUnitario = v[2].float()
	r.IDSalida = v[3].int()
	return r
}

func parseEntrada(v []value) srcEntrada {
	r := srcEntrada{}
	r.ID = v[0].int()
	r.Estado = v[1].int()
	r.FechaCreacion = v[2].datetime()
	// v[3] idUsuarioCreo
	r.Total = v[4].float()
	r.IDTypePayment = v[5].int()
	return r
}

func parseDetalleEntrada(v []value) srcDetalleEntrada {
	r := srcDetalleEntrada{}
	r.ID = v[0].int()
	r.IDProducto = v[1].int()
	r.CostoUnitario = v[2].float()
	r.IDEntrada = v[3].int()
	// v[4] idDetalleSalida (link back to a sale's COGS row, ignored)
	return r
}

func parseVisita(v []value) srcVisita {
	r := srcVisita{}
	r.ID = v[0].int()
	r.IDSocio = v[1].int()
	r.FechaCreacion = v[2].datetime()
	r.PrecioVisita = v[3].float()
	return r
}

func parseCut(v []value) srcCut {
	r := srcCut{}
	r.ID = v[0].int()
	r.Date = v[1].datetime()
	r.DateStart = v[2].datetime()
	r.DateEnd = v[3].datetime()
	// v[4] idUser
	r.Estado = v[5].int()
	r.Observation = v[6].string()
	r.CashStart = v[7].float()
	r.CashEnd = v[8].float()
	r.Income = v[9].float()
	r.Egress = v[10].float()
	return r
}

// ── ID minting ──────────────────────────────────────────────────────────────

func legacyID(table string, id int) uuid.UUID {
	return uuid.NewSHA1(uuidNS, []byte(fmt.Sprintf("%s:%d", table, id)))
}

// ── enum mappings ───────────────────────────────────────────────────────────

func mapPaymentMethod(idTypePayment int) string {
	switch idTypePayment {
	case 1:
		return "cash"
	case 2:
		return "card"
	default:
		return "transfer" // legacy "Otro" maps to transfer (closest in Cuadra enum)
	}
}

// ── Inferencia de género (sólo import legacy) ────────────────────────────────
// El sistema viejo nunca guardó sexo, pero el reporte de género (DA-012.7) lo
// necesita. guessGender hace un best-effort sobre el PRIMER nombre de pila:
// primero un diccionario de nombres mexicanos comunes (sobre todo los que NO
// siguen la regla -o/-a), y si no, la heurística de sufijo español
// (-a → mujer, -o → hombre). Es deliberadamente imperfecto: son datos de
// dogfooding y el operador puede corregir cualquier socio luego. Devuelve nil
// ("no capturado" → NULL) cuando no puede decidir. Valores = enum
// chk_members_gender: "hombre" | "mujer".
var accentReplacer = strings.NewReplacer(
	"á", "a", "é", "e", "í", "i", "ó", "o", "ú", "u", "ü", "u", "ñ", "n",
)

// genderByName cubre los nombres frecuentes que la regla de sufijo erraría
// (femeninos que no terminan en -a, masculinos que no terminan en -o).
var genderByName = map[string]string{
	// Femeninos que NO terminan en -a
	"carmen": "mujer", "guadalupe": "mujer", "beatriz": "mujer", "isabel": "mujer",
	"raquel": "mujer", "soledad": "mujer", "mercedes": "mujer", "dolores": "mujer",
	"concepcion": "mujer", "pilar": "mujer", "rosario": "mujer", "lourdes": "mujer",
	"ines": "mujer", "ruth": "mujer", "esther": "mujer", "noemi": "mujer",
	"abigail": "mujer", "itzel": "mujer", "anabel": "mujer", "maribel": "mujer",
	"ivonne": "mujer", "jazmin": "mujer", "jacqueline": "mujer", "jocelyn": "mujer",
	"jennifer": "mujer", "miriam": "mujer", "iris": "mujer", "citlali": "mujer",
	"nayeli": "mujer", "mayte": "mujer", "dulce": "mujer", "montserrat": "mujer",
	"ingrid": "mujer", "marisol": "mujer", "consuelo": "mujer", "belen": "mujer",
	"karen": "mujer", "evelyn": "mujer", "ashley": "mujer", "betsy": "mujer",
	"xochitl": "mujer", "magali": "mujer", "lizbeth": "mujer", "liz": "mujer",
	"yaretzi": "mujer", "estefani": "mujer", "ma": "mujer",
	"emily": "mujer", "emely": "mujer", "wendy": "mujer", "ivon": "mujer",
	"yvonne": "mujer", "nancy": "mujer", "lizeth": "mujer", "mitzi": "mujer",
	"britany": "mujer", "brittany": "mujer", "estefany": "mujer", "anel": "mujer",
	"yamilet": "mujer", "marlen": "mujer", "yuridia": "mujer", "nohemi": "mujer",
	// Masculinos que NO terminan en -o
	"jose": "hombre", "juan": "hombre", "luis": "hombre", "jesus": "hombre",
	"miguel": "hombre", "angel": "hombre", "manuel": "hombre", "rafael": "hombre",
	"gabriel": "hombre", "daniel": "hombre", "samuel": "hombre", "ismael": "hombre",
	"israel": "hombre", "joel": "hombre", "abel": "hombre", "uriel": "hombre",
	"axel": "hombre", "david": "hombre", "ivan": "hombre", "adrian": "hombre",
	"julian": "hombre", "fabian": "hombre", "sebastian": "hombre", "cristian": "hombre",
	"christian": "hombre", "brian": "hombre", "kevin": "hombre", "edwin": "hombre",
	"efrain": "hombre", "joaquin": "hombre", "agustin": "hombre", "martin": "hombre",
	"ruben": "hombre", "hector": "hombre", "victor": "hombre", "cesar": "hombre",
	"oscar": "hombre", "nestor": "hombre", "salvador": "hombre", "omar": "hombre",
	"saul": "hombre", "raul": "hombre", "abraham": "hombre", "elias": "hombre",
	"matias": "hombre", "tobias": "hombre", "enrique": "hombre", "felipe": "hombre",
	"jorge": "hombre", "jaime": "hombre", "vicente": "hombre", "yair": "hombre",
	"jair": "hombre", "jonathan": "hombre", "alan": "hombre", "aaron": "hombre",
	"ramon": "hombre", "simon": "hombre", "noe": "hombre", "rene": "hombre",
	"emanuel": "hombre", "emmanuel": "hombre", "leonel": "hombre", "gael": "hombre",
	"esteban": "hombre", "isaac": "hombre", "jared": "hombre", "brandon": "hombre",
	"caleb": "hombre", "kaleb": "hombre", "alexis": "hombre", "emyr": "hombre",
	"yahir": "hombre", "dilan": "hombre", "dylan": "hombre", "bryan": "hombre",
	// Resueltos a partir de los nombres reales del dump (la regla -o/-a no los cubre).
	"ailen": "mujer", "angeles": "mujer", "arely": "mujer", "aritzi": "mujer",
	"berenice": "mujer", "berlen": "mujer", "denisse": "mujer", "elizabeth": "mujer",
	"evelin": "mujer", "flor": "mujer", "gaby": "mujer", "jaqueline": "mujer",
	"jenni": "mujer", "joselin": "mujer", "judith": "mujer", "karol": "mujer",
	"leslie": "mujer", "lezli": "mujer", "luz": "mujer", "maite": "mujer",
	"mari": "mujer", "matilde": "mujer", "michel": "mujer", "nahomi": "mujer",
	"nicol": "mujer", "nicolle": "mujer", "nury": "mujer", "yoali": "mujer",
	"yuli":   "mujer",
	"adriel": "hombre", "alex": "hombre", "alexander": "hombre", "andres": "hombre",
	"avi": "hombre", "brayan": "hombre", "carlos": "hombre", "dominick": "hombre",
	"edgar": "hombre", "erick": "hombre", "henrry": "hombre", "hernan": "hombre",
	"irvin": "hombre", "javier": "hombre", "jhonatan": "hombre", "josue": "hombre",
	"nelson": "hombre", "roger": "hombre", "suriel": "hombre", "thenotorious": "hombre",
	"valentin": "hombre", "yael": "hombre",
}

// firstToken devuelve el primer nombre de pila en minúsculas y sin acentos.
func firstToken(s string) string {
	fields := strings.Fields(strings.ToLower(s))
	if len(fields) == 0 {
		return ""
	}
	return accentReplacer.Replace(strings.Trim(fields[0], ".,"))
}

func guessGender(rawName string) *string {
	name := firstToken(rawName)
	if name == "" {
		return nil
	}
	if g, ok := genderByName[name]; ok {
		return &g
	}
	switch {
	case strings.HasSuffix(name, "a"):
		g := "mujer"
		return &g
	case strings.HasSuffix(name, "o"):
		g := "hombre"
		return &g
	default:
		return nil
	}
}

func mapMemberStatus(estado int) string {
	switch estado {
	case 1:
		return "active"
	case 2:
		return "inactive"
	default:
		return "lost" // estado=3 → eliminado on the legacy side
	}
}

func mapMembershipStatus(estado int) string {
	// Cuadra membership statuses: active, expired, replaced, cancelled.
	switch estado {
	case 1:
		return "active"
	case 2:
		return "expired"
	default:
		return "cancelled"
	}
}

func mapEstadoToDeleted(estado int, fallback time.Time) *time.Time {
	if estado == 3 {
		t := fallback
		return &t
	}
	return nil
}

// durationDays mirrors `meses*30 + semanas*7 + dias` from the legacy schema.
func durationDays(meses, semanas, dias int) int {
	d := meses*30 + semanas*7 + dias
	if d <= 0 {
		d = 1
	}
	return d
}

// ── sync_entities emission ─────────────────────────────────────────────────
//
// Importer writes directly to cloud's domain tables (so the dashboard sees
// data immediately) AND mirrors each row into `sync_entities` so any sidecar
// pulling will project the data into its local SQLite. Without the second
// half, sidecars stay empty even though the cloud is populated.
//
// Payload shape mirrors what a sidecar push would have looked like — wire
// column names, money in cents (integer) so the sidecar's apply path lands
// it back in SQLite's INTEGER-cents columns cleanly, dates as YYYY-MM-DD,
// timestamps as unix milliseconds.

// syncEmitSeq da a cada fila de sync_entities un server_updated_at único y
// creciente. El import original ponía el MISMO `now` a todas, lo que rompía el
// pull del sidecar en dos formas: (1) ListSince ordena por (server_updated_at,
// entity_id) y, con timestamps idénticos, el desempate por UUID mezclaba hijos
// antes que padres → "FOREIGN KEY constraint failed" en el SQLite del sidecar;
// (2) el cursor avanza con `server_updated_at > since`, así que un bloque con el
// mismo timestamp se truncaba a la primera página. Espaciamos por rango
// topológico (1s por nivel: padres en una banda anterior a hijos) + una
// secuencia global en microsegundos (cada fila distinta, para que la paginación
// avance). El offset total queda en pocos segundos — no en el futuro lejano,
// para no ensombrecer escrituras reales posteriores al import.
var syncEmitSeq int64

// syncTopoRank ordena los entity types por dependencia de FK: un tipo nunca
// debe sincronizarse antes que aquellos a los que referencia.
func syncTopoRank(entityType string) int {
	switch entityType {
	case "gyms", "users":
		return 0
	case "membership_types", "products":
		return 1
	case "members":
		return 2
	case "memberships", "sales":
		return 3
	case "payments", "checkins", "cash_close_events":
		return 4
	case "sale_items", "stock_movements":
		return 5
	default:
		return 6
	}
}

// emitSyncEntity upserts a single (gym_id, entity_type, entity_id) row into
// sync_entities with the given payload + version. Any existing row gets
// overwritten — combined with the deterministic UUIDs the importer mints,
// re-running is safe.
func emitSyncEntity(tx *gorm.DB, gymID, entityID uuid.UUID, entityType string, version int, deletedAt *time.Time, now time.Time, payload map[string]any) error {
	// Defense in depth: pin id + gym_id from the call site, ignore whatever
	// the caller put in the map.
	payload["id"] = entityID.String()
	payload["gym_id"] = gymID.String()
	payload["version"] = version
	bytes, err := jsonMarshal(payload)
	if err != nil {
		return err
	}
	// server_updated_at único y topológicamente ordenado (ver syncEmitSeq).
	syncEmitSeq++
	serverUpdatedAt := now.
		Add(time.Duration(syncTopoRank(entityType)) * time.Second).
		Add(time.Duration(syncEmitSeq) * time.Microsecond)
	return tx.Exec(`
		INSERT INTO sync_entities (gym_id, entity_type, entity_id, version, payload, server_updated_at, deleted_at)
		VALUES (?, ?, ?, ?, ?::jsonb, ?, ?)
		ON CONFLICT (gym_id, entity_type, entity_id) DO UPDATE
		SET version = EXCLUDED.version,
		    payload = EXCLUDED.payload,
		    server_updated_at = EXCLUDED.server_updated_at,
		    deleted_at = EXCLUDED.deleted_at`,
		gymID, entityType, entityID, version, string(bytes), serverUpdatedAt, deletedAt,
	).Error
}

func jsonMarshal(v any) ([]byte, error) {
	return json.Marshal(v)
}

func toCents(dollars float64) int64 { return int64(dollars*100 + 0.5) }

func dateStr(t time.Time) string { return t.UTC().Format("2006-01-02") }

func milliPtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().UnixMilli()
}

func optString(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

func optStringPtr(p *string) any {
	if p == nil || strings.TrimSpace(*p) == "" {
		return nil
	}
	return *p
}

func uuidStrOrNil(p *uuid.UUID) any {
	if p == nil {
		return nil
	}
	return p.String()
}

// ── importer ────────────────────────────────────────────────────────────────

func wipeGym(tx *gorm.DB, gymID uuid.UUID) error {
	// Order matters: child rows first to satisfy FKs (casi todas RESTRICT).
	// La lista original se quedó corta contra el esquema actual y el wipe
	// reventaba con datos reales: applied_promotions (FK RESTRICT →
	// payments/promotions/members) bloqueaba el DELETE de payments,
	// member_fingerprints el de members, y challenges el de members vía
	// participants. Audit log + conflict_log se dejan (pueden cargar FKs
	// stale sin romper nada — todas las queries filtran por gym_id).
	//
	// SOBREVIVEN a propósito (identidad + config del gym, no datos
	// operativos): gyms, users, gym_keys (GMK biométrica), sidecar_
	// credentials, subscription_events (billing Tinta↔gym),
	// notification_templates y owner_alert_configs (toggles), releases,
	// whatsapp_opt_outs (cumplimiento Meta).
	tables := []string{
		"whatsapp_events", // ruido de webhooks (filas con gym_id NULL quedan)
		"notification_queue",
		"challenge_measurements",
		"challenge_participants",
		"challenge_categories", // FK → challenges: van ANTES que su reto
		"challenges",
		"applied_promotions",
		"sale_items",
		"sales",
		"stock_movements",
		"member_fingerprints",
		"checkins",
		"contact_attempts",
		"membership_adjustments",
		"memberships",
		"payments",
		"promotions",
		"members",
		"products",
		"membership_types",
		"expenses",
		"cash_close_events",
		"gym_ownership_transfers",
	}
	for _, t := range tables {
		if err := tx.Exec(fmt.Sprintf("DELETE FROM %s WHERE gym_id = ?", t), gymID).Error; err != nil {
			return fmt.Errorf("wipe %s: %w", t, err)
		}
	}
	// sync_entities (el journal del que sale todo pull/full-sync) se limpia
	// SELECTIVAMENTE: se borran las entradas de los tipos borrados arriba
	// (si quedaran, un full-sync resucitaría a los socios de prueba en el
	// sidecar) pero se CONSERVAN las de los tipos que sobreviven — sin el
	// journal de notification_templates/owner_alert_configs, un sidecar
	// fresco no recibiría los toggles y mostraría defaults que el cloud no
	// honra (drift silencioso).
	const keepTypes = `('gyms','users','notification_templates','owner_alert_configs','subscription_events')`
	if err := tx.Exec(
		`DELETE FROM sync_entities WHERE gym_id = ? AND entity_type NOT IN `+keepTypes,
		gymID,
	).Error; err != nil {
		return fmt.Errorf("wipe sync_entities: %w", err)
	}
	return nil
}

func importAll(tx *gorm.DB, src sourceData, gymID, ownerID uuid.UUID) error {
	now := time.Now().UTC()

	// 1. membership_types
	mtIDs := map[int]uuid.UUID{}
	for _, m := range src.memberships {
		id := legacyID("membresia", m.ID)
		mtIDs[m.ID] = id
		var deletedAt *time.Time
		if m.Estado == 3 {
			deletedAt = &now
		}
		row := memModels.MembershipTypeModel{
			ID: id, GymID: gymID, Version: 1,
			CreatedAt:    derefTime(m.FechaCreacion, now),
			UpdatedAt:    now,
			DeletedAt:    deletedAt,
			Name:         m.Nombre,
			Price:        m.Precio,
			DurationDays: durationDays(m.Meses, m.Semanas, m.Dias),
			Active:       m.Estado == 1,
		}
		if err := tx.Create(&row).Error; err != nil {
			return fmt.Errorf("membresia %d: %w", m.ID, err)
		}
		if err := emitSyncEntity(tx, gymID, id, "membership_types", 1, deletedAt, now, map[string]any{
			"created_at":      row.CreatedAt.UnixMilli(),
			"updated_at":      row.UpdatedAt.UnixMilli(),
			"name":            row.Name,
			"price":           toCents(row.Price),
			"duration_days":   row.DurationDays,
			"enrollment_fee":  0,
			"maintenance_fee": 0,
			"active":          row.Active,
		}); err != nil {
			return fmt.Errorf("emit membership_types %d: %w", m.ID, err)
		}
	}
	log.Printf("import: membership_types=%d", len(src.memberships))

	// 2. products (stock starts at 0 — reconstructed from stock_movements below)
	prodIDs := map[int]uuid.UUID{}
	prodNames := map[int]string{}
	prodPrices := map[int]float64{}
	for _, p := range src.products {
		id := legacyID("producto", p.ID)
		prodIDs[p.ID] = id
		prodNames[p.ID] = p.Nombre
		prodPrices[p.ID] = p.Precio
		var deletedAt *time.Time
		if p.Estado == 3 {
			deletedAt = &now
		}
		row := prodModels.ProductModel{
			ID: id, GymID: gymID, Version: 1,
			CreatedAt:    derefTime(p.FechaCreacion, now),
			UpdatedAt:    now,
			DeletedAt:    deletedAt,
			Name:         p.Nombre,
			Price:        p.Precio,
			Stock:        0,
			StockMinimum: 0,
			Active:       p.Estado == 1,
		}
		if err := tx.Create(&row).Error; err != nil {
			return fmt.Errorf("producto %d: %w", p.ID, err)
		}
		if err := emitSyncEntity(tx, gymID, id, "products", 1, deletedAt, now, map[string]any{
			"created_at":    row.CreatedAt.UnixMilli(),
			"updated_at":    row.UpdatedAt.UnixMilli(),
			"name":          row.Name,
			"price":         toCents(row.Price),
			"stock":         0,
			"stock_minimum": 0,
			"active":        row.Active,
		}); err != nil {
			return fmt.Errorf("emit product %d: %w", p.ID, err)
		}
	}
	log.Printf("import: products=%d", len(src.products))

	// 3. members. Folio is `idSocio` zero-padded to 5. El número de socio
	// (ADR-010) NO se backfillea desde el `clave` legacy aquí — se deja sin
	// asignar y el sidecar/cloud le da uno al primer alta/check-in; el `clave`
	// se preserva sólo en Notes para que el operador lo vea durante la
	// migración. (member_number lo asigna AssignMemberNumber.)
	memberIDs := map[int]uuid.UUID{}
	for _, s := range src.socios {
		id := legacyID("socio", s.ID)
		memberIDs[s.ID] = id
		fullName := strings.TrimSpace(s.Nombre + " " + s.Paterno + " " + s.Materno)
		var email *string
		if e := strings.TrimSpace(s.Correo); e != "" {
			email = &e
		}
		var notes *string
		bits := []string{}
		if strings.TrimSpace(s.Observaciones) != "" {
			bits = append(bits, s.Observaciones)
		}
		if strings.TrimSpace(s.Clave) != "" {
			bits = append(bits, "PIN legacy: "+s.Clave)
		}
		if len(bits) > 0 {
			n := strings.Join(bits, " · ")
			notes = &n
		}
		// Phone is NOT NULL on both Postgres and SQLite, and the projector
		// rewrites "" → NULL — so we substitute a visible placeholder when
		// the legacy row left it empty. The dash makes it obvious in the UI
		// that the operator never registered a number for this socio.
		phone := phonepkg.Normalize(s.Telefono)
		if phone == "" {
			phone = "—"
		}
		row := memModels.MemberModel{
			ID: id, GymID: gymID, Version: 1,
			CreatedAt: derefTime(s.FechaCreacion, now),
			UpdatedAt: now,
			Folio:     fmt.Sprintf("%05d", s.ID),
			FullName:  ifEmpty(fullName, "(sin nombre)"),
			Phone:     phone,
			Email:     email,
			Birthdate: s.FechaNacimiento,
			Notes:     notes,
			Status:    mapMemberStatus(s.Estado),
			Gender:    guessGender(s.Nombre),
			CreatedBy: ownerID,
		}
		if err := tx.Create(&row).Error; err != nil {
			return fmt.Errorf("socio %d: %w", s.ID, err)
		}
		var bd any
		if row.Birthdate != nil {
			bd = dateStr(*row.Birthdate)
		}
		if err := emitSyncEntity(tx, gymID, id, "members", 1, nil, now, map[string]any{
			"created_at":      row.CreatedAt.UnixMilli(),
			"updated_at":      row.UpdatedAt.UnixMilli(),
			"folio":           row.Folio,
			"full_name":       row.FullName,
			"phone":           row.Phone,
			"email":           optStringPtr(row.Email),
			"birthdate":       bd,
			"notes":           optStringPtr(row.Notes),
			"status":          row.Status,
			"enrollment_paid": row.EnrollmentPaid,
			"gender":          optStringPtr(row.Gender),
			"created_by":      row.CreatedBy.String(),
		}); err != nil {
			return fmt.Errorf("emit member %d: %w", s.ID, err)
		}
	}
	log.Printf("import: members=%d", len(src.socios))

	// 4. memberships. Cuadra's unique partial index forbids more than one
	// `status='active'` row per member, so we have to pre-rank: for each
	// member, only the row with the latest expiry that ALSO has legacy
	// estado=Activo earns the active slot — the rest collapse to `expired`.
	// Build the prepared rows first, dedup, then bulk-insert.
	membIDs := map[int]uuid.UUID{}

	type prepared struct {
		legacyID     int
		row          memModels.MembershipModel
		legacyEstado int
	}
	var prep []prepared
	for _, sm := range src.socioMembs {
		memberID, ok := memberIDs[sm.IDSocio]
		if !ok {
			continue
		}
		typeID, ok := mtIDs[sm.IDMembresia]
		if !ok {
			continue
		}
		id := legacyID("sociomembresia", sm.ID)
		membIDs[sm.ID] = id
		startDate := derefTime(sm.FechaInicioMembresia, derefTime(sm.FechaCreacion, now))
		expiry := derefTime(sm.Vencimiento, startDate.AddDate(0, 0, durationDays(sm.Meses, sm.Semanas, sm.Dias)))
		typeName := ""
		typePrice := 0.0
		for _, m := range src.memberships {
			if m.ID == sm.IDMembresia {
				typeName = m.Nombre
				typePrice = m.Precio
				break
			}
		}
		if typePrice == 0 {
			typePrice = sm.Precio
		}
		// Initial status — refined by dedup below.
		status := "expired"
		if sm.Estado == 1 && expiry.After(now) {
			status = "active"
		} else if sm.Estado == 3 {
			status = "cancelled"
		}
		// ExpiryDate del modelo es *time.Time (nullable desde la migración
		// 011). startOfDay devuelve time.Time, así que materializamos en una
		// variable para poder tomar su dirección.
		expiryDate := startOfDay(expiry)
		prep = append(prep, prepared{
			legacyID:     sm.ID,
			legacyEstado: sm.Estado,
			row: memModels.MembershipModel{
				ID: id, GymID: gymID, Version: 1,
				CreatedAt:            derefTime(sm.FechaCreacion, now),
				UpdatedAt:            now,
				MemberID:             memberID,
				MembershipTypeID:     typeID,
				TypeNameSnapshot:     typeName,
				PriceSnapshot:        typePrice,
				DurationDaysSnapshot: durationDays(sm.Meses, sm.Semanas, sm.Dias),
				StartDate:            startOfDay(startDate),
				ExpiryDate:           &expiryDate,
				Status:               status,
			},
		})
	}

	// Dedup: per member, sort active candidates by expiry DESC and keep only
	// the first as active. Everything else → expired (cancelled rows stay
	// cancelled — they don't compete for the active slot anyway).
	idxByMember := map[uuid.UUID][]int{}
	for i, p := range prep {
		if p.row.Status == "active" {
			idxByMember[p.row.MemberID] = append(idxByMember[p.row.MemberID], i)
		}
	}
	for _, idxs := range idxByMember {
		if len(idxs) <= 1 {
			continue
		}
		sort.Slice(idxs, func(a, b int) bool {
			// ExpiryDate es *time.Time; el import siempre lo setea non-nil
			// (línea ~1263), así que derefear el argumento es seguro acá.
			return prep[idxs[a]].row.ExpiryDate.After(*prep[idxs[b]].row.ExpiryDate)
		})
		for _, i := range idxs[1:] {
			prep[i].row.Status = "expired"
		}
	}

	for _, p := range prep {
		row := p.row
		if err := tx.Create(&row).Error; err != nil {
			return fmt.Errorf("sociomembresia %d: %w", p.legacyID, err)
		}
		if err := emitSyncEntity(tx, gymID, row.ID, "memberships", 1, nil, now, map[string]any{
			"created_at":             row.CreatedAt.UnixMilli(),
			"updated_at":             row.UpdatedAt.UnixMilli(),
			"member_id":              row.MemberID.String(),
			"membership_type_id":     row.MembershipTypeID.String(),
			"type_name_snapshot":     row.TypeNameSnapshot,
			"price_snapshot":         toCents(row.PriceSnapshot),
			"duration_days_snapshot": row.DurationDaysSnapshot,
			"start_date":             dateStr(row.StartDate),
			"expiry_date":            dateStr(*row.ExpiryDate),
			"status":                 row.Status,
		}); err != nil {
			return fmt.Errorf("emit membership %d: %w", p.legacyID, err)
		}
	}
	log.Printf("import: memberships=%d", len(prep))

	// 5. payments — membership concept (sociomembresia_pago)
	paymentCount := 0
	for _, p := range src.socioMembsPagos {
		var memberID *uuid.UUID
		// Find the original sociomembresia → socio.
		for _, sm := range src.socioMembs {
			if sm.ID == p.IDSocioMembresia {
				if mid, ok := memberIDs[sm.IDSocio]; ok {
					memberID = &mid
				}
				break
			}
		}
		var deletedAt *time.Time
		if p.Estado == 3 {
			deletedAt = &now
		}
		paymentID := legacyID("sociomembresia_pago", p.ID)
		row := billingModels.PaymentModel{
			ID:    paymentID,
			GymID: gymID, Version: 1,
			CreatedAt:     derefTime(p.Fecha, now),
			UpdatedAt:     now,
			DeletedAt:     deletedAt,
			Folio:         fmt.Sprintf("MEM-%05d", p.ID),
			MemberID:      memberID,
			Amount:        p.Importe,
			PaymentMethod: mapPaymentMethod(p.IDTypePayment),
			Concept:       "membership",
			PaymentDate:   dateOnly(derefTime(p.Fecha, now)),
			OperatorID:    ownerID,
		}
		if err := tx.Create(&row).Error; err != nil {
			return fmt.Errorf("sociomembresia_pago %d: %w", p.ID, err)
		}
		if err := emitSyncEntity(tx, gymID, paymentID, "payments", 1, deletedAt, now, map[string]any{
			"created_at":      row.CreatedAt.UnixMilli(),
			"updated_at":      row.UpdatedAt.UnixMilli(),
			"folio":           row.Folio,
			"member_id":       uuidStrOrNil(row.MemberID),
			"amount":          toCents(row.Amount),
			"payment_method":  row.PaymentMethod,
			"concept":         row.Concept,
			"discount_amount": 0,
			"balance_pending": 0,
			"payment_date":    dateStr(row.PaymentDate),
			"operator_id":     row.OperatorID.String(),
		}); err != nil {
			return fmt.Errorf("emit payment %d: %w", p.ID, err)
		}
		paymentCount++
	}

	// 6. sales + sale_items + product payments
	// A salida row + its detallesalida rows = 1 sale. We collapse duplicate
	// product lines into a single sale_item with quantity > 1 so the FE shows
	// "2 × Agua" rather than two separate rows.
	detsBySalida := map[int][]srcDetalleSalida{}
	for _, d := range src.detSalidas {
		detsBySalida[d.IDSalida] = append(detsBySalida[d.IDSalida], d)
	}
	saleCount := 0
	for _, s := range src.salidas {
		details := detsBySalida[s.ID]
		if len(details) == 0 {
			continue
		}
		// One payment row per sale (concept=product), then a sale row, then
		// items grouped by product.
		paymentID := legacyID("salida_payment", s.ID)
		var deletedAt *time.Time
		if s.Estado == 3 {
			deletedAt = &now
		}
		payment := billingModels.PaymentModel{
			ID: paymentID, GymID: gymID, Version: 1,
			CreatedAt: derefTime(s.FechaCreacion, now),
			UpdatedAt: now,
			DeletedAt: deletedAt,
			// Prefijo canónico "PRD-%06d" (igual que folio.PrefixForConcept /
			// el generador). Antes era "PROD-%05d": el generador hacía
			// TrimPrefix(maxFolio, "PRD-") y no quitaba "PROD-", parseaba 0 →
			// next=1 → colisión UNIQUE(gym_id, folio) en cada venta nueva.
			Folio:         fmt.Sprintf("PRD-%06d", s.ID),
			Amount:        s.Total,
			PaymentMethod: mapPaymentMethod(s.IDTypePayment),
			Concept:       "product",
			PaymentDate:   dateOnly(derefTime(s.FechaCreacion, now)),
			OperatorID:    ownerID,
		}
		if err := tx.Create(&payment).Error; err != nil {
			return fmt.Errorf("salida payment %d: %w", s.ID, err)
		}
		if err := emitSyncEntity(tx, gymID, paymentID, "payments", 1, deletedAt, now, map[string]any{
			"created_at":      payment.CreatedAt.UnixMilli(),
			"updated_at":      payment.UpdatedAt.UnixMilli(),
			"folio":           payment.Folio,
			"amount":          toCents(payment.Amount),
			"payment_method":  payment.PaymentMethod,
			"concept":         payment.Concept,
			"discount_amount": 0,
			"balance_pending": 0,
			"payment_date":    dateStr(payment.PaymentDate),
			"operator_id":     payment.OperatorID.String(),
		}); err != nil {
			return fmt.Errorf("emit salida payment %d: %w", s.ID, err)
		}
		paymentCount++

		saleID := legacyID("salida", s.ID)
		// group by product
		grouped := map[int]struct {
			qty   int
			price float64
		}{}
		for _, d := range details {
			g := grouped[d.IDProducto]
			g.qty++
			g.price = d.PrecioUnitario
			grouped[d.IDProducto] = g
		}
		var subtotal float64
		for _, g := range grouped {
			subtotal += g.price * float64(g.qty)
		}
		sale := billingModels.SaleModel{
			ID: saleID, GymID: gymID, Version: 1,
			CreatedAt: derefTime(s.FechaCreacion, now),
			UpdatedAt: now,
			DeletedAt: deletedAt,
			PaymentID: paymentID,
			Subtotal:  subtotal,
			Total:     s.Total,
		}
		if err := tx.Create(&sale).Error; err != nil {
			return fmt.Errorf("salida sale %d: %w", s.ID, err)
		}
		if err := emitSyncEntity(tx, gymID, saleID, "sales", 1, deletedAt, now, map[string]any{
			"created_at": sale.CreatedAt.UnixMilli(),
			"updated_at": sale.UpdatedAt.UnixMilli(),
			"payment_id": sale.PaymentID.String(),
			"subtotal":   toCents(sale.Subtotal),
			"discount":   0,
			"total":      toCents(sale.Total),
		}); err != nil {
			return fmt.Errorf("emit sale %d: %w", s.ID, err)
		}
		// items
		for prodID, g := range grouped {
			itemID := legacyID(fmt.Sprintf("salida_item_%d", s.ID), prodID)
			pUUID, ok := prodIDs[prodID]
			if !ok {
				continue
			}
			item := billingModels.SaleItemModel{
				ID: itemID, GymID: gymID, Version: 1,
				CreatedAt:           derefTime(s.FechaCreacion, now),
				UpdatedAt:           now,
				DeletedAt:           deletedAt,
				SaleID:              saleID,
				ProductID:           pUUID,
				ProductNameSnapshot: prodNames[prodID],
				UnitPriceSnapshot:   g.price,
				Quantity:            g.qty,
				LineTotal:           g.price * float64(g.qty),
			}
			if err := tx.Create(&item).Error; err != nil {
				return fmt.Errorf("salida item %d/%d: %w", s.ID, prodID, err)
			}
			if err := emitSyncEntity(tx, gymID, itemID, "sale_items", 1, deletedAt, now, map[string]any{
				"created_at":            item.CreatedAt.UnixMilli(),
				"updated_at":            item.UpdatedAt.UnixMilli(),
				"sale_id":               item.SaleID.String(),
				"product_id":            item.ProductID.String(),
				"product_name_snapshot": item.ProductNameSnapshot,
				"unit_price_snapshot":   toCents(item.UnitPriceSnapshot),
				"quantity":              item.Quantity,
				"line_total":            toCents(item.LineTotal),
			}); err != nil {
				return fmt.Errorf("emit sale_item %d/%d: %w", s.ID, prodID, err)
			}
			// stock movement (sale → -qty)
			delta := -g.qty
			movID := legacyID("salida_mov", int(itemID.ID()))
			mov := prodModels.StockMovementModel{
				ID:    movID,
				GymID: gymID, Version: 1,
				CreatedAt:    derefTime(s.FechaCreacion, now),
				UpdatedAt:    now,
				DeletedAt:    deletedAt,
				ProductID:    pUUID,
				MovementType: "sale",
				Delta:        delta,
				SaleItemID:   &itemID,
				OperatorID:   ownerID,
			}
			if err := tx.Create(&mov).Error; err != nil {
				return fmt.Errorf("sale stock_mov %d/%d: %w", s.ID, prodID, err)
			}
			if err := emitSyncEntity(tx, gymID, movID, "stock_movements", 1, deletedAt, now, map[string]any{
				"created_at":    mov.CreatedAt.UnixMilli(),
				"updated_at":    mov.UpdatedAt.UnixMilli(),
				"product_id":    mov.ProductID.String(),
				"movement_type": mov.MovementType,
				"delta":         mov.Delta,
				"sale_item_id":  itemID.String(),
				"operator_id":   mov.OperatorID.String(),
			}); err != nil {
				return fmt.Errorf("emit sale stock_mov %d/%d: %w", s.ID, prodID, err)
			}
		}
		saleCount++
	}
	log.Printf("import: payments=%d sales=%d", paymentCount, saleCount)

	// 7. stock_movements — restock from entradas/detalleentrada
	detsByEntrada := map[int][]srcDetalleEntrada{}
	for _, d := range src.detEntradas {
		detsByEntrada[d.IDEntrada] = append(detsByEntrada[d.IDEntrada], d)
	}
	movCount := 0
	for _, e := range src.entradas {
		details := detsByEntrada[e.ID]
		if len(details) == 0 {
			continue
		}
		// Restocks group: 1 movement per detail row (one unit each, like
		// sale items). The UI summarises by product later.
		grouped := map[int]struct {
			qty  int
			cost float64
		}{}
		for _, d := range details {
			g := grouped[d.IDProducto]
			g.qty++
			g.cost = d.CostoUnitario
			grouped[d.IDProducto] = g
		}
		for prodID, g := range grouped {
			pUUID, ok := prodIDs[prodID]
			if !ok {
				continue
			}
			cost := g.cost
			movID := legacyID(fmt.Sprintf("entrada_mov_%d", e.ID), prodID)
			mov := prodModels.StockMovementModel{
				ID:    movID,
				GymID: gymID, Version: 1,
				CreatedAt:    derefTime(e.FechaCreacion, now),
				UpdatedAt:    now,
				ProductID:    pUUID,
				MovementType: "restock",
				Delta:        g.qty,
				Cost:         &cost,
				OperatorID:   ownerID,
			}
			if err := tx.Create(&mov).Error; err != nil {
				return fmt.Errorf("entrada_mov %d/%d: %w", e.ID, prodID, err)
			}
			if err := emitSyncEntity(tx, gymID, movID, "stock_movements", 1, nil, now, map[string]any{
				"created_at":    mov.CreatedAt.UnixMilli(),
				"updated_at":    mov.UpdatedAt.UnixMilli(),
				"product_id":    mov.ProductID.String(),
				"movement_type": mov.MovementType,
				"delta":         mov.Delta,
				"cost":          toCents(cost),
				"operator_id":   mov.OperatorID.String(),
			}); err != nil {
				return fmt.Errorf("emit entrada_mov %d/%d: %w", e.ID, prodID, err)
			}
			movCount++
		}
	}
	log.Printf("import: stock_movements (restock)=%d", movCount)

	// 8. recompute products.stock = SUM(stock_movements.delta) per product,
	// then re-emit each product's sync_entity with the right stock so sidecar
	// pulls land the correct value (the initial emission used 0).
	if err := tx.Exec(`
		UPDATE products p SET stock = COALESCE((
			SELECT SUM(delta) FROM stock_movements m
			WHERE m.product_id = p.id AND m.deleted_at IS NULL
		), 0)
		WHERE p.gym_id = ?`, gymID).Error; err != nil {
		return fmt.Errorf("recompute stock: %w", err)
	}
	type prodStockRow struct {
		ID    uuid.UUID `gorm:"column:id"`
		Stock int       `gorm:"column:stock"`
	}
	var stockRows []prodStockRow
	if err := tx.Raw(`SELECT id, stock FROM products WHERE gym_id = ?`, gymID).Scan(&stockRows).Error; err != nil {
		return fmt.Errorf("read stock: %w", err)
	}
	for _, sr := range stockRows {
		// Find the original product row by ID to rebuild the payload.
		var pr prodModels.ProductModel
		if err := tx.First(&pr, "id = ?", sr.ID).Error; err != nil {
			return fmt.Errorf("read product %s: %w", sr.ID, err)
		}
		if err := emitSyncEntity(tx, gymID, sr.ID, "products", 1, pr.DeletedAt, now, map[string]any{
			"created_at":    pr.CreatedAt.UnixMilli(),
			"updated_at":    pr.UpdatedAt.UnixMilli(),
			"name":          pr.Name,
			"price":         toCents(pr.Price),
			"stock":         sr.Stock,
			"stock_minimum": pr.StockMinimum,
			"active":        pr.Active,
		}); err != nil {
			return fmt.Errorf("re-emit product %s: %w", sr.ID, err)
		}
	}

	// 9. checkins (visitas). Skip the legacy "visita generica" socio (id=1000)
	// since it's a placeholder, not a real member.
	chkCount := 0
	for _, v := range src.visitas {
		if v.IDSocio == 1000 {
			continue
		}
		mid, ok := memberIDs[v.IDSocio]
		if !ok {
			continue
		}
		chkID := legacyID("visita", v.ID)
		row := chkModels.CheckinModel{
			ID:    chkID,
			GymID: gymID, Version: 1,
			CreatedAt:  derefTime(v.FechaCreacion, now),
			UpdatedAt:  now,
			MemberID:   mid,
			CheckinAt:  derefTime(v.FechaCreacion, now),
			Method:     "manual",
			Result:     "allowed_active",
			OperatorID: &ownerID,
		}
		if err := tx.Create(&row).Error; err != nil {
			return fmt.Errorf("visita %d: %w", v.ID, err)
		}
		if err := emitSyncEntity(tx, gymID, chkID, "checkins", 1, nil, now, map[string]any{
			"created_at":      row.CreatedAt.UnixMilli(),
			"updated_at":      row.UpdatedAt.UnixMilli(),
			"member_id":       row.MemberID.String(),
			"checkin_at":      row.CheckinAt.UnixMilli(),
			"method":          row.Method,
			"result":          row.Result,
			"operator_id":     ownerID.String(),
			"manual_override": false,
		}); err != nil {
			return fmt.Errorf("emit checkin %d: %w", v.ID, err)
		}
		chkCount++
	}
	log.Printf("import: checkins=%d (skipped %d generic-visit rows)", chkCount, countGenericVisits(src.visitas))

	// 10. cash_close_events
	for _, c := range src.cuts {
		closeDate := dateOnly(derefTime(c.Date, derefTime(c.DateEnd, now)))
		var deletedAt *time.Time
		if c.Estado == 3 {
			deletedAt = &now
		}
		var obs *string
		if strings.TrimSpace(c.Observation) != "" {
			s := c.Observation
			obs = &s
		}
		ccID := legacyID("cut", c.ID)
		row := billingModels.CashCloseEventModel{
			ID:    ccID,
			GymID: gymID, Version: 1,
			CreatedAt:         derefTime(c.Date, now),
			UpdatedAt:         now,
			DeletedAt:         deletedAt,
			CloseDate:         closeDate,
			CalculatedCash:    c.Income - c.Egress + c.CashStart,
			DiscrepancyReason: obs,
			ClosedBy:          ownerID,
		}
		if err := tx.Create(&row).Error; err != nil {
			return fmt.Errorf("cut %d: %w", c.ID, err)
		}
		if err := emitSyncEntity(tx, gymID, ccID, "cash_close_events", 1, deletedAt, now, map[string]any{
			"created_at":      row.CreatedAt.UnixMilli(),
			"updated_at":      row.UpdatedAt.UnixMilli(),
			"close_date":      dateStr(row.CloseDate),
			"calculated_cash": toCents(row.CalculatedCash),
			"closed_by":       row.ClosedBy.String(),
		}); err != nil {
			return fmt.Errorf("emit cash_close %d: %w", c.ID, err)
		}
	}
	log.Printf("import: cash_close_events=%d", len(src.cuts))

	return nil
}

// ── helpers ─────────────────────────────────────────────────────────────────

func derefTime(p *time.Time, fallback time.Time) time.Time {
	if p == nil {
		return fallback
	}
	return p.UTC()
}

func dateOnly(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func startOfDay(t time.Time) time.Time { return dateOnly(t) }

func ifEmpty(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

func countGenericVisits(vs []srcVisita) int {
	n := 0
	for _, v := range vs {
		if v.IDSocio == 1000 {
			n++
		}
	}
	return n
}
