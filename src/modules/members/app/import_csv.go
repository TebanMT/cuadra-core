package app

import (
	"context"
	"encoding/csv"
	"errors"
	"io"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"

	memErrors "github.com/cuadra/cuadra-core/src/modules/members/domain/errors"
	memberDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/member"
	membershipDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/membership"
	mtDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/membership_type"
	memRepo "github.com/cuadra/cuadra-core/src/modules/members/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// ImportMembersFromCSV — UC-046. Wizard self-serve "Importación masiva de
// socios y membresías desde Excel" (Standard tier). El BE NO auto-asigna PIN
// ni dispara welcome-WhatsApp por fila: una migración de libreta no debe
// notificar a 80 socios de golpe. El dueño asigna PINs con UC-032 después.
//
// Atomicidad: todo el batch corre dentro de un único UoW.Command — si una
// fila revienta a nivel infra (no a nivel validación de fila) hace rollback
// completo. Las filas inválidas viajan en ErrorRow / SkipRow sin abortar.
//
// El use case NO crea Payment ni renueva. Membresías que vienen en el CSV
// se asumen "ya al día" (data histórica del sistema viejo), enrollment_paid
// queda en true. Si el dueño quiere registrar el cobro histórico, lo hace
// después con UC-018.
type ImportMembersFromCSV struct {
	Members         memRepo.MemberRepository
	Memberships     memRepo.MembershipRepository
	MembershipTypes memRepo.MembershipTypeRepository
	UoW             sharedDomain.UnitOfWork
	Audit           audit.Recorder
}

func NewImportMembersFromCSV(
	members memRepo.MemberRepository,
	memberships memRepo.MembershipRepository,
	mtypes memRepo.MembershipTypeRepository,
	uow sharedDomain.UnitOfWork,
	recorder audit.Recorder,
) *ImportMembersFromCSV {
	return &ImportMembersFromCSV{
		Members:         members,
		Memberships:     memberships,
		MembershipTypes: mtypes,
		UoW:             uow,
		Audit:           recorder,
	}
}

// ImportMembersFromCSVInput is the use case input. Reader puede ser cualquier
// io.Reader que el caller provea — gin.Context.FormFile en producción,
// strings.NewReader en tests.
type ImportMembersFromCSVInput struct {
	GymID           uuid.UUID
	ActorUserID     uuid.UUID
	Reader          io.Reader
	AllowDuplicates bool
}

// ImportedRow describes a member that landed in storage.
type ImportedRow struct {
	RowNumber          int       `json:"row_number"`
	MemberID           uuid.UUID `json:"member_id"`
	Folio              string    `json:"folio"`
	FullName           string    `json:"full_name"`
	MembershipAssigned bool      `json:"membership_assigned"`
}

// SkipRow describes a row that was understood but intentionally not imported
// (e.g. duplicate phone with allow_duplicates=false).
type SkipRow struct {
	RowNumber int    `json:"row_number"`
	FullName  string `json:"full_name"`
	Phone     string `json:"phone"`
	Reason    string `json:"reason"`
}

// ErrorRow describes a row that failed validation. Message is a friendly
// Spanish string for the FE to surface verbatim.
type ErrorRow struct {
	RowNumber int    `json:"row_number"`
	FullName  string `json:"full_name"`
	Phone     string `json:"phone"`
	Message   string `json:"message"`
}

// ImportMembersFromCSVOutput is what handlers + tests assert on.
type ImportMembersFromCSVOutput struct {
	Imported      []ImportedRow `json:"imported"`
	Skipped       []SkipRow     `json:"skipped"`
	Errors        []ErrorRow    `json:"errors"`
	ImportedCount int           `json:"imported_count"`
	SkippedCount  int           `json:"skipped_count"`
	ErrorsCount   int           `json:"errors_count"`
	TotalDataRows int           `json:"total_data_rows"`
}

// Skip reasons surfaced to the FE.
const (
	SkipReasonPhoneTakenInGym = "phone_taken_in_gym"
	SkipReasonDuplicateInFile = "duplicate_in_file"
)

// Header esperado en el CSV. Si crece, sumar al final — el parser tolera
// columnas extra al final ignorándolas, pero falla si faltan obligatorias.
// Las primeras 5 son obligatorias (validateHeader exige sus nombres en
// orden); de la 6 en adelante son opcionales y se evalúan posicionalmente,
// así que añadir una nueva columna AL FINAL es backwards-compatible.
//
// `gender` (o el alias en español `genero`) es opcional. Aliases aceptados
// case-insensitive: H/hombre → hombre, M/mujer → mujer; cualquier otro
// string cae a no_especificado con warning en el log (no rechazamos la
// fila completa por un género mal tipeado).
var expectedHeader = []string{
	"full_name", "phone", "email", "birthdate", "notes",
	"membership_type_name", "membership_start_date", "membership_expiry_date",
	"gender",
}

// utf8BOM es el marcador U+FEFF que Excel agrega al inicio de los CSV
// guardados como "UTF-8". TrimPrefix sobre cada celda lo limpia.
const utf8BOM = "\ufeff"

// parsedRow es la representación intermedia post-tokenize, pre-persist. Vive
// en memoria toda la importación porque la dedupe in-file necesita un segundo
// pase sobre todas las filas válidas.
type parsedRow struct {
	rowNumber     int // 1-based, incluye header (header = fila 1, primer dato = fila 2)
	fullName      string
	phone         string
	email         *string
	birthdate     *time.Time
	notes         *string
	planName      string
	startDate     *time.Time
	expiryDate    *time.Time
	hasMembership bool // las 3 cols vinieron completas y válidas
	gender        *string
	// genderWarning queda no-vacío cuando el raw del CSV no coincidió con
	// ninguno de los aliases; el caller loggea sin abortar la fila.
	genderWarning string
	preParseError error
}

// Execute parses, validates and persists. Error sólo se devuelve para fallos
// de infraestructura (lectura del CSV, panic dentro del UoW). Filas inválidas
// viajan en out.Errors y NO abortan el batch.
func (uc *ImportMembersFromCSV) Execute(ctx context.Context, in ImportMembersFromCSVInput) (*ImportMembersFromCSVOutput, error) {
	now := time.Now().UTC()

	reader := csv.NewReader(in.Reader)
	reader.FieldsPerRecord = -1 // permitimos longitudes variables; validamos manualmente
	reader.TrimLeadingSpace = true

	header, err := reader.Read()
	if err == io.EOF {
		return nil, sharedDomain.NewValidationError(memErrors.ErrCSVMissingHeader)
	}
	if err != nil {
		return nil, sharedDomain.NewValidationError(memErrors.ErrCSVMalformed)
	}
	if err := validateHeader(header); err != nil {
		return nil, sharedDomain.NewValidationError(err)
	}

	// Primera pasada: tokenize + parse de cada fila SIN tocar la base.
	var rows []parsedRow
	rowNum := 1 // header consumido arriba
	for {
		rec, rerr := reader.Read()
		if rerr == io.EOF {
			break
		}
		rowNum++
		if rerr != nil {
			rows = append(rows, parsedRow{rowNumber: rowNum, preParseError: memErrors.ErrCSVMalformed})
			continue
		}
		rows = append(rows, parseCSVRow(rec, rowNum))
	}

	out := &ImportMembersFromCSVOutput{
		Imported: []ImportedRow{},
		Skipped:  []SkipRow{},
		Errors:   []ErrorRow{},
	}
	out.TotalDataRows = len(rows)
	if len(rows) == 0 {
		return out, nil
	}

	// Dedup in-file por phone: si dos filas válidas comparten phone normalizado,
	// la PRIMERA pasa, las siguientes van a errores con razón duplicate_in_file.
	// Lo hacemos antes de pegar a la DB para que el FE pueda señalar la fila
	// exacta sin importar setup de duplicados intra-gym.
	seenInFile := map[string]int{}
	for i := range rows {
		if rows[i].preParseError != nil || rows[i].phone == "" {
			continue
		}
		if first, ok := seenInFile[rows[i].phone]; ok {
			rows[i].preParseError = memErrors.ErrCSVDuplicateInFile
			_ = first
		} else {
			seenInFile[rows[i].phone] = rows[i].rowNumber
		}
	}

	// Segunda pasada: todo dentro de UoW.Command. Filas con preParseError o
	// validación de dominio fallida van a out.Errors. Duplicate vs base de
	// datos va a out.Skipped (no es error del archivo, es decisión del dueño).
	err = uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		// Cache de planes del gym por nombre normalizado — evita N queries
		// si el CSV trae muchas filas con membresía.
		planCache, perr := uc.loadPlanCache(tx, in.GymID)
		if perr != nil {
			return sharedDomain.NewUnexpectedError(perr)
		}

		for _, r := range rows {
			if r.preParseError != nil {
				out.Errors = append(out.Errors, ErrorRow{
					RowNumber: r.rowNumber,
					FullName:  r.fullName,
					Phone:     r.phone,
					Message:   r.preParseError.Error(),
				})
				continue
			}

			// 1) Validación de dominio del socio.
			if verr := memberDomain.ValidateFullName(r.fullName); verr != nil {
				out.Errors = append(out.Errors, ErrorRow{
					RowNumber: r.rowNumber, FullName: r.fullName, Phone: r.phone,
					Message: verr.Error(),
				})
				continue
			}
			if !memberDomain.ValidatePhone(r.phone) {
				out.Errors = append(out.Errors, ErrorRow{
					RowNumber: r.rowNumber, FullName: r.fullName, Phone: r.phone,
					Message: memErrors.ErrInvalidPhone.Error(),
				})
				continue
			}
			if r.email != nil && !memberDomain.ValidateEmail(*r.email) {
				out.Errors = append(out.Errors, ErrorRow{
					RowNumber: r.rowNumber, FullName: r.fullName, Phone: r.phone,
					Message: memErrors.ErrInvalidEmail.Error(),
				})
				continue
			}

			// 2) Si trae cols de membresía, resolver plan + validar fechas.
			var plan *mtDomain.MembershipType
			if r.hasMembership {
				p, ok := planCache[normalizeplanName(r.planName)]
				if !ok {
					out.Errors = append(out.Errors, ErrorRow{
						RowNumber: r.rowNumber, FullName: r.fullName, Phone: r.phone,
						Message: memErrors.ErrCSVPlanNotFound.Error() + " (plan: \"" + r.planName + "\")",
					})
					continue
				}
				if !p.Active {
					out.Errors = append(out.Errors, ErrorRow{
						RowNumber: r.rowNumber, FullName: r.fullName, Phone: r.phone,
						Message: memErrors.ErrMembershipTypeInactive.Error() + " (plan: \"" + r.planName + "\")",
					})
					continue
				}
				if r.expiryDate.Before(*r.startDate) {
					out.Errors = append(out.Errors, ErrorRow{
						RowNumber: r.rowNumber, FullName: r.fullName, Phone: r.phone,
						Message: memErrors.ErrCSVMembershipDates.Error(),
					})
					continue
				}
				plan = p
			}

			// 3) Duplicate phone intra-gym contra base. Sin allow_duplicates
			//    se skippea con razón; con allow_duplicates pasa.
			if !in.AllowDuplicates {
				exists, eerr := uc.Members.ExistsByGymAndPhone(tx, in.GymID, r.phone)
				if eerr != nil {
					return sharedDomain.NewUnexpectedError(eerr)
				}
				if exists {
					out.Skipped = append(out.Skipped, SkipRow{
						RowNumber: r.rowNumber, FullName: r.fullName, Phone: r.phone,
						Reason: SkipReasonPhoneTakenInGym,
					})
					continue
				}
			}

			// 4) Persistir socio. Folio incremental por gym.
			folio, ferr := uc.Members.NextFolio(tx, in.GymID)
			if ferr != nil {
				return sharedDomain.NewUnexpectedError(ferr)
			}
			memberID := uuid.New()
			m, merr := memberDomain.NewMember(memberID, in.GymID, folio, r.fullName, r.phone, in.ActorUserID, now)
			if merr != nil {
				// NewMember sólo falla por validación de dominio que ya chequeamos;
				// si llegamos acá es bug y abortamos.
				return sharedDomain.NewValidationError(merr)
			}
			if perr := m.ApplyProfileUpdate(memberDomain.ProfileUpdate{
				Email:     r.email,
				Birthdate: r.birthdate,
				Notes:     r.notes,
				Gender:    r.gender,
			}, now); perr != nil {
				out.Errors = append(out.Errors, ErrorRow{
					RowNumber: r.rowNumber, FullName: r.fullName, Phone: r.phone,
					Message: perr.Error(),
				})
				continue
			}
			if r.genderWarning != "" {
				log.Printf("[import-csv] row %d gym=%s: género %q no reconocido; usando no_especificado",
					r.rowNumber, in.GymID, r.genderWarning)
			}

			// Membresía importada se asume "ya al día" — el socio venía con
			// inscripción pagada en la libreta vieja. enrollment_paid=true
			// evita que el dashboard la marque como "pendiente de inscripción".
			if plan != nil {
				m.EnrollmentPaid = true
			}

			if _, cerr := uc.Members.Create(tx, m); cerr != nil {
				return sharedDomain.NewUnexpectedError(cerr)
			}

			imported := ImportedRow{
				RowNumber: r.rowNumber, MemberID: memberID, Folio: folio, FullName: m.FullName,
			}

			// 5) Membership opcional. Status=active con expiry custom (no
			//    derivado de plan.DurationDays — respetamos la data histórica
			//    del sistema viejo).
			if plan != nil {
				ms := membershipDomain.New(uuid.New(), in.GymID, m.ID, plan, *r.startDate, now)
				// Override del expiry con el dato del CSV (data histórica).
				ed := truncateUTC(*r.expiryDate)
				ms.ExpiryDate = &ed
				if _, mserr := uc.Memberships.Create(tx, ms); mserr != nil {
					return sharedDomain.NewUnexpectedError(mserr)
				}
				imported.MembershipAssigned = true
			}

			// 6) Audit por fila importada.
			_ = uc.Audit.Record(ctx, tx, audit.Entry{
				GymID:       in.GymID,
				EntityType:  "members",
				EntityID:    m.ID,
				Action:      audit.ActionCreate,
				ActorUserID: &in.ActorUserID,
				Changes: map[string]any{
					"folio":               m.Folio,
					"source":              "csv_import",
					"membership_assigned": imported.MembershipAssigned,
					"row_number":          r.rowNumber,
				},
				IPAddress: audit.IPFromContext(ctx),
				UserAgent: audit.UAFromContext(ctx),
				At:        now,
			})

			out.Imported = append(out.Imported, imported)
		}

		// 7) Audit summary (uno solo para la operación). EntityID = gym_id
		//    como ancla — el "qué cambió" se mide a nivel padrón completo.
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "members",
			EntityID:    in.GymID,
			Action:      audit.ActionCreate,
			ActorUserID: &in.ActorUserID,
			Changes: map[string]any{
				"source":           "csv_import_summary",
				"imported_count":   len(out.Imported),
				"skipped_count":    len(out.Skipped),
				"errors_count":     len(out.Errors),
				"total_data_rows":  out.TotalDataRows,
				"allow_duplicates": in.AllowDuplicates,
			},
			IPAddress: audit.IPFromContext(ctx),
			UserAgent: audit.UAFromContext(ctx),
			At:        now,
		})

		return nil
	})

	if err != nil {
		// Diferenciamos: un CustomError lo devolvemos tal cual (el handler
		// sabe mapearlo a HTTP). Cualquier otro lo envolvemos.
		var ce sharedDomain.CustomError
		if errors.As(err, &ce) {
			return nil, err
		}
		return nil, sharedDomain.NewUnexpectedError(err)
	}

	out.ImportedCount = len(out.Imported)
	out.SkippedCount = len(out.Skipped)
	out.ErrorsCount = len(out.Errors)
	return out, nil
}

// loadPlanCache mete los planes del gym en un map por nombre normalizado para
// resolución O(1) en la pasada de filas. Includes inactive para poder dar el
// mensaje específico "ese plan está desactivado".
func (uc *ImportMembersFromCSV) loadPlanCache(tx sharedDomain.Transaction, gymID uuid.UUID) (map[string]*mtDomain.MembershipType, error) {
	plans, err := uc.MembershipTypes.ListByGym(tx, gymID, true)
	if err != nil {
		return nil, err
	}
	cache := make(map[string]*mtDomain.MembershipType, len(plans))
	for _, p := range plans {
		cache[normalizeplanName(p.Name)] = p
	}
	return cache, nil
}

func validateHeader(h []string) error {
	if len(h) < 5 {
		return memErrors.ErrCSVHeaderMismatch
	}
	for i, want := range expectedHeader {
		// Las primeras 5 son obligatorias; las 3 de membresía pueden faltar
		// si el dueño usa la versión "solo socios" del CSV.
		if i >= len(h) {
			if i < 5 {
				return memErrors.ErrCSVHeaderMismatch
			}
			break
		}
		got := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(h[i], "\ufeff")))
		if got == want {
			continue
		}
		// gender admite alias en espa\u00f1ol `genero` para que el due\u00f1o pueda
		// mantener el CSV en su idioma. No abrimos esto a otras columnas
		// salvo que producto lo pida.
		if want == "gender" && got == "genero" {
			continue
		}
		return memErrors.ErrCSVHeaderMismatch
	}
	return nil
}

func parseCSVRow(rec []string, rowNum int) parsedRow {
	r := parsedRow{rowNumber: rowNum}
	// Tolerancia: si la fila viene con menos columnas que las 9 esperadas
	// (8 base + gender), rellenamos con strings vacíos en vez de explotar.
	// Cualquier campo vacío que tenía que venir lo cazan las validaciones
	// de dominio.
	cells := make([]string, 9)
	for i := 0; i < 9 && i < len(rec); i++ {
		cells[i] = strings.TrimSpace(strings.TrimPrefix(rec[i], "\ufeff"))
	}
	r.fullName = cells[0]
	r.phone = sanitizePhoneRaw(cells[1])
	if v := cells[2]; v != "" {
		r.email = &v
	}
	if v := cells[3]; v != "" {
		t, err := time.Parse("2006-01-02", v)
		if err != nil {
			r.preParseError = memErrors.ErrInvalidBirthdate
			return r
		}
		bd := t
		r.birthdate = &bd
	}
	if v := cells[4]; v != "" {
		r.notes = &v
	}

	// Cols 5-7: membresía. All-or-nothing: si viene al menos una, exigimos
	// las tres. Las tres vacías → no se importa membresía (válido).
	planRaw := cells[5]
	startRaw := cells[6]
	expiryRaw := cells[7]
	someMembership := planRaw != "" || startRaw != "" || expiryRaw != ""
	allMembership := planRaw != "" && startRaw != "" && expiryRaw != ""
	if someMembership && !allMembership {
		r.preParseError = memErrors.ErrCSVMembershipPartial
		return r
	}
	if allMembership {
		st, err := time.Parse("2006-01-02", startRaw)
		if err != nil {
			r.preParseError = memErrors.ErrCSVMembershipDates
			return r
		}
		ex, err := time.Parse("2006-01-02", expiryRaw)
		if err != nil {
			r.preParseError = memErrors.ErrCSVMembershipDates
			return r
		}
		r.planName = planRaw
		r.startDate = &st
		r.expiryDate = &ex
		r.hasMembership = true
	}

	// Col 8: gender (opcional). Aliases case-insensitive:
	//   H, h, hombre, Hombre → GenderMale
	//   M, m, mujer, Mujer   → GenderFemale
	// Cualquier otro string no vacío cae a GenderUnspecified con warning
	// en el log — preferimos importar el row con género ambiguo a botarlo
	// completo por un typo. Vacío significa "no se capturó" (NULL en DB).
	if v := cells[8]; v != "" {
		g, warn := csvGenderFromAlias(v)
		gv := g
		r.gender = &gv
		if warn {
			r.genderWarning = v
		}
	}
	return r
}

// csvGenderFromAlias mapea el raw del CSV al enum del dominio. Devuelve
// (valor, warning) — warning=true cuando no matcheó ningún alias y se cayó
// a GenderUnspecified. Documenta el comportamiento del parser: el CSV no
// es la superficie de captura primaria, así que "X" o "otro" no aborta la
// fila completa; el dueño lo corrige después desde el perfil del socio.
func csvGenderFromAlias(raw string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "h", "hombre", "m_hombre", "masculino":
		return memberDomain.GenderMale, false
	case "m", "mujer", "femenino", "f":
		// "f" (femenino) lo aceptamos por simetría con "m"; en MX el form
		// usa M=mujer (acepta lo mismo el operador) pero si la libreta vieja
		// usaba F lo seguimos resolviendo.
		return memberDomain.GenderFemale, false
	case "no_especificado", "no especificado", "prefiero no decir", "n/a", "na":
		return memberDomain.GenderUnspecified, false
	default:
		return memberDomain.GenderUnspecified, true
	}
}

// sanitizePhoneRaw replica la limpieza que hace memberDomain.NewMember
// (espacios, guiones, parens) pero a nivel de input crudo del CSV para que
// la dedupe in-file use el valor normalizado.
func sanitizePhoneRaw(raw string) string {
	v := strings.TrimSpace(raw)
	v = strings.ReplaceAll(v, " ", "")
	v = strings.ReplaceAll(v, "-", "")
	v = strings.ReplaceAll(v, "(", "")
	v = strings.ReplaceAll(v, ")", "")
	return v
}

func normalizeplanName(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

func truncateUTC(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}
