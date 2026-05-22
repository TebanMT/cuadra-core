//go:build sidecar

package app_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	memApp "github.com/cuadra/cuadra-core/src/modules/members/app"
	memErrors "github.com/cuadra/cuadra-core/src/modules/members/domain/errors"
	memberDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/member"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// ---------------------------------------------------------------------------
// UC-046 — Importación masiva de socios + membresías desde CSV
// ---------------------------------------------------------------------------

const csvHeader = "full_name,phone,email,birthdate,notes,membership_type_name,membership_start_date,membership_expiry_date"

func newImportUC(f *membersFixture) *memApp.ImportMembersFromCSV {
	return memApp.NewImportMembersFromCSV(f.memberRepo, f.membershipR, f.mtRepo, f.uow, f.recorder)
}

func runImport(t *testing.T, f *membersFixture, body string, allowDup bool) *memApp.ImportMembersFromCSVOutput {
	t.Helper()
	uc := newImportUC(f)
	out, err := uc.Execute(context.Background(), memApp.ImportMembersFromCSVInput{
		GymID:           f.gymID,
		ActorUserID:     f.ownerID,
		Reader:          strings.NewReader(body),
		AllowDuplicates: allowDup,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	return out
}

func TestUC046_HappyPath_FiveSocios(t *testing.T) {
	f := setupMembersFixture(t)
	body := csvHeader + "\n" +
		"Ana López,5512345670,ana@example.com,1990-03-15,nota A,,,\n" +
		"Pedro Ramírez,5512345671,,,,,,\n" +
		"María Sánchez,5512345672,maria@example.com,,,,,\n" +
		"Carlos Díaz,5512345673,,1985-07-22,,,,\n" +
		"Lucía Fernández,5512345674,,,,,,\n"
	out := runImport(t, f, body, false)

	if out.ImportedCount != 5 {
		t.Fatalf("imported=%d, want 5; out=%+v", out.ImportedCount, out)
	}
	if out.SkippedCount != 0 || out.ErrorsCount != 0 {
		t.Errorf("skipped=%d errors=%d, want 0/0 (out=%+v)", out.SkippedCount, out.ErrorsCount, out)
	}
	if out.TotalDataRows != 5 {
		t.Errorf("total_data_rows=%d, want 5", out.TotalDataRows)
	}
	for _, row := range out.Imported {
		if row.MembershipAssigned {
			t.Errorf("row %d: membership_assigned=true (expected false; no plan cols)", row.RowNumber)
		}
	}
}

func TestUC046_PhoneDuplicateIntraGym_SkippedWithReason(t *testing.T) {
	f := setupMembersFixture(t)
	// Pre-existente.
	createUC := memApp.NewCreateMember(f.memberRepo, f.membershipR, f.mtRepo, f.uow, f.recorder)
	typeID := f.createMembershipType(t, "Mensual")
	_, err := createUC.Execute(context.Background(), memApp.CreateMemberInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		FullName: "Original Existente", Phone: "5599999999",
		MembershipTypeID: typeID,
		StartDate:        truncatedToday(),
	})
	if err != nil {
		t.Fatalf("seed member: %v", err)
	}

	body := csvHeader + "\n" + "Duplicado Phone,5599999999,,,,,,\n"
	out := runImport(t, f, body, false)

	if out.ImportedCount != 0 {
		t.Errorf("imported=%d, want 0", out.ImportedCount)
	}
	if out.SkippedCount != 1 {
		t.Fatalf("skipped=%d, want 1 (out=%+v)", out.SkippedCount, out)
	}
	if got := out.Skipped[0].Reason; got != memApp.SkipReasonPhoneTakenInGym {
		t.Errorf("skip reason=%q, want %q", got, memApp.SkipReasonPhoneTakenInGym)
	}
	if out.Skipped[0].RowNumber != 2 {
		t.Errorf("skip row_number=%d, want 2", out.Skipped[0].RowNumber)
	}
}

func TestUC046_PhoneDuplicateIntraGym_AllowDup_Imported(t *testing.T) {
	f := setupMembersFixture(t)
	createUC := memApp.NewCreateMember(f.memberRepo, f.membershipR, f.mtRepo, f.uow, f.recorder)
	typeID := f.createMembershipType(t, "Mensual")
	if _, err := createUC.Execute(context.Background(), memApp.CreateMemberInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		FullName: "Original", Phone: "5599999999",
		MembershipTypeID: typeID, StartDate: truncatedToday(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	body := csvHeader + "\n" + "Hermano Gemelo,5599999999,,,,,,\n"
	out := runImport(t, f, body, true)
	if out.ImportedCount != 1 || out.SkippedCount != 0 {
		t.Fatalf("allow_duplicates should import: %+v", out)
	}
}

func TestUC046_MixedRows(t *testing.T) {
	f := setupMembersFixture(t)
	// Pre-existente para forzar 1 skipped.
	createUC := memApp.NewCreateMember(f.memberRepo, f.membershipR, f.mtRepo, f.uow, f.recorder)
	typeID := f.createMembershipType(t, "Mensual")
	if _, err := createUC.Execute(context.Background(), memApp.CreateMemberInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		FullName: "Preexistente", Phone: "5588888888",
		MembershipTypeID: typeID, StartDate: truncatedToday(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	body := csvHeader + "\n" +
		"Válido Uno,5511111111,,,,,,\n" +
		"Válido Dos,5511111112,,,,,,\n" +
		"Phone Inválido,abc,,,,,,\n" +
		"Válido Tres,5511111113,,,,,,\n" +
		"Skip por duplicado,5588888888,,,,,,\n"
	out := runImport(t, f, body, false)

	if out.ImportedCount != 3 {
		t.Errorf("imported=%d, want 3 (out=%+v)", out.ImportedCount, out)
	}
	if out.ErrorsCount != 1 {
		t.Errorf("errors=%d, want 1", out.ErrorsCount)
	}
	if out.SkippedCount != 1 {
		t.Errorf("skipped=%d, want 1", out.SkippedCount)
	}
	// Verifica que la fila del error reporta el row_number original.
	if out.Errors[0].RowNumber != 4 {
		t.Errorf("error row_number=%d, want 4 (header=1, data=2..6)", out.Errors[0].RowNumber)
	}
}

func TestUC046_HeaderOnly_EmptyArrays(t *testing.T) {
	f := setupMembersFixture(t)
	out := runImport(t, f, csvHeader+"\n", false)
	if out.ImportedCount != 0 || out.SkippedCount != 0 || out.ErrorsCount != 0 {
		t.Errorf("expected all zero counts, got %+v", out)
	}
	if out.Imported == nil || out.Skipped == nil || out.Errors == nil {
		t.Errorf("collections must be non-nil to serialize as [] (got nils)")
	}
}

func TestUC046_BirthdayInvalido_FilaAErrores_RestoImporta(t *testing.T) {
	f := setupMembersFixture(t)
	body := csvHeader + "\n" +
		"Ok Uno,5511111111,,,,,,\n" +
		"Bad Birth,5511111112,,15/03/1990,,,,\n" +
		"Ok Dos,5511111113,,,,,,\n"
	out := runImport(t, f, body, false)
	if out.ImportedCount != 2 {
		t.Errorf("imported=%d, want 2", out.ImportedCount)
	}
	if out.ErrorsCount != 1 {
		t.Fatalf("errors=%d, want 1 (out=%+v)", out.ErrorsCount, out)
	}
	if out.Errors[0].RowNumber != 3 {
		t.Errorf("error row=%d, want 3", out.Errors[0].RowNumber)
	}
}

func TestUC046_Atomicidad_HeaderInvalido_NoCreaNada(t *testing.T) {
	f := setupMembersFixture(t)
	// Header con typo en la 1ra columna.
	body := "FULL_NAME_TYPO,phone,email,birthdate,notes,membership_type_name,membership_start_date,membership_expiry_date\n" +
		"Ana,5511111111,,,,,,\n"
	uc := newImportUC(f)
	_, err := uc.Execute(context.Background(), memApp.ImportMembersFromCSVInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		Reader: strings.NewReader(body),
	})
	if err == nil {
		t.Fatalf("expected validation error for bad header, got nil")
	}
	var ce sharedDomain.CustomError
	if !errors.As(err, &ce) || ce.ErrorCode != sharedDomain.CodeValidation {
		t.Fatalf("expected validation CustomError, got %+v", err)
	}

	// Nada quedó persistido: el padrón sigue vacío.
	tx, _ := f.uow.Query(context.Background())
	exists, qerr := f.memberRepo.ExistsByGymAndPhone(tx, f.gymID, "5511111111")
	if qerr != nil {
		t.Fatalf("query existing: %v", qerr)
	}
	if exists {
		t.Errorf("la fila de Ana NO debió persistirse tras header inválido")
	}
}

func TestUC046_ConMembresia_PlanResueltoEnGym(t *testing.T) {
	f := setupMembersFixture(t)
	_ = f.createMembershipType(t, "Mensual")

	body := csvHeader + "\n" +
		"Con Plan,5577777777,,,,Mensual,2026-04-01,2026-05-01\n" +
		"Sin Plan,5577777778,,,,,,\n"
	out := runImport(t, f, body, false)
	if out.ImportedCount != 2 {
		t.Fatalf("imported=%d, want 2 (out=%+v)", out.ImportedCount, out)
	}
	// Encontrar la fila "Con Plan" y validar que membership_assigned=true.
	var conPlanAsgn, sinPlanAsgn bool
	for _, r := range out.Imported {
		switch r.FullName {
		case "Con Plan":
			conPlanAsgn = r.MembershipAssigned
		case "Sin Plan":
			sinPlanAsgn = r.MembershipAssigned
		}
	}
	if !conPlanAsgn {
		t.Errorf("'Con Plan' debió quedar con membership_assigned=true")
	}
	if sinPlanAsgn {
		t.Errorf("'Sin Plan' NO debió tener membership_assigned")
	}
}

func TestUC046_ConMembresia_PlanInexistente_ErrorEnFila(t *testing.T) {
	f := setupMembersFixture(t)
	_ = f.createMembershipType(t, "Mensual")

	body := csvHeader + "\n" +
		"Plan Ghost,5588888887,,,,Trimestral,2026-04-01,2026-07-01\n"
	out := runImport(t, f, body, false)
	if out.ImportedCount != 0 {
		t.Errorf("imported=%d, want 0", out.ImportedCount)
	}
	if out.ErrorsCount != 1 {
		t.Fatalf("errors=%d, want 1 (out=%+v)", out.ErrorsCount, out)
	}
	if !strings.Contains(out.Errors[0].Message, "Trimestral") {
		t.Errorf("error message should name the missing plan, got %q", out.Errors[0].Message)
	}
}

func TestUC046_DedupInFile(t *testing.T) {
	f := setupMembersFixture(t)
	body := csvHeader + "\n" +
		"Primera Aparición,5566554433,,,,,,\n" +
		"Segunda Aparición,5566554433,,,,,,\n"
	out := runImport(t, f, body, false)
	if out.ImportedCount != 1 {
		t.Errorf("imported=%d, want 1 (la 2da debe ir a errors por dup in-file)", out.ImportedCount)
	}
	if out.ErrorsCount != 1 {
		t.Fatalf("errors=%d, want 1", out.ErrorsCount)
	}
	if out.Errors[0].RowNumber != 3 {
		t.Errorf("dup in-file should be row 3, got %d", out.Errors[0].RowNumber)
	}
}

// truncatedToday es un helper que devuelve la fecha de hoy a medianoche UTC,
// para no chocar con ValidateStartDate en seeds previos del fixture.
func truncatedToday() time.Time {
	t := time.Now().UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// compile-time check: el use case se construye con el shape del fixture.
var _ = func(f *membersFixture) *memApp.ImportMembersFromCSV { return newImportUC(f) }

// silenciamos imports no usados cuando los tests evolucionan.
var _ = memErrors.ErrCSVHeaderMismatch
var _ = memberDomain.StatusActive

// ---------------------------------------------------------------------------
// UC-046 — gender column (DA-012.7)
// ---------------------------------------------------------------------------

const csvHeaderWithGender = csvHeader + ",gender"
const csvHeaderWithGenderEs = csvHeader + ",genero"

// TestUC046_Gender_Aliases — cubre los aliases documentados:
//   - "H" y "hombre" → hombre
//   - "Mujer" y "MUJER" → mujer (case-insensitive)
//   - "X" → no_especificado (fallback con warning, NO bloquea el row)
//   - vacío → NULL
func TestUC046_Gender_Aliases(t *testing.T) {
	f := setupMembersFixture(t)
	body := csvHeaderWithGender + "\n" +
		"Hombre Uno,5512300001,,,,,,,H\n" +
		"Hombre Dos,5512300002,,,,,,,hombre\n" +
		"Mujer Una,5512300003,,,,,,,Mujer\n" +
		"Mujer Dos,5512300004,,,,,,,MUJER\n" +
		"Inválido,5512300005,,,,,,,X\n" +
		"Vacío,5512300006,,,,,,,\n"
	out := runImport(t, f, body, false)
	if out.ImportedCount != 6 {
		t.Fatalf("imported=%d, want 6 (out=%+v)", out.ImportedCount, out)
	}
	if out.ErrorsCount != 0 {
		t.Errorf("errors=%d, want 0 — la fila X NO debe abortar", out.ErrorsCount)
	}

	// Leer cada uno del repo y validar el bucket asignado.
	type expect struct {
		fullName string
		want     *string
	}
	hombre, mujer, unspec := memberDomain.GenderMale, memberDomain.GenderFemale, memberDomain.GenderUnspecified
	expects := []expect{
		{"Hombre Uno", &hombre},
		{"Hombre Dos", &hombre},
		{"Mujer Una", &mujer},
		{"Mujer Dos", &mujer},
		{"Inválido", &unspec}, // alias desconocido cae a no_especificado
		{"Vacío", nil},        // vacío queda NULL
	}
	byName := map[string]memApp.ImportedRow{}
	for _, r := range out.Imported {
		byName[r.FullName] = r
	}
	tx, err := f.uow.Query(context.Background())
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	for _, e := range expects {
		row, ok := byName[e.fullName]
		if !ok {
			t.Errorf("missing imported row for %q", e.fullName)
			continue
		}
		m, gerr := f.memberRepo.GetByID(tx, row.MemberID)
		if gerr != nil {
			t.Errorf("get %q: %v", e.fullName, gerr)
			continue
		}
		if e.want == nil {
			if m.Gender != nil {
				t.Errorf("%q gender = %v, want nil", e.fullName, *m.Gender)
			}
			continue
		}
		if m.Gender == nil || *m.Gender != *e.want {
			t.Errorf("%q gender = %v, want %v", e.fullName, m.Gender, *e.want)
		}
	}
}

// TestUC046_Gender_SpanishAlias — el header en español `genero` debe
// aceptarse igual que `gender`, para que el dueño pueda mantener el CSV en
// su idioma sin renombrar columnas.
func TestUC046_Gender_SpanishAlias(t *testing.T) {
	f := setupMembersFixture(t)
	body := csvHeaderWithGenderEs + "\n" +
		"Test Genero,5512300010,,,,,,,M\n"
	out := runImport(t, f, body, false)
	if out.ImportedCount != 1 {
		t.Fatalf("imported=%d, want 1 (out=%+v)", out.ImportedCount, out)
	}
	tx, _ := f.uow.Query(context.Background())
	m, _ := f.memberRepo.GetByID(tx, out.Imported[0].MemberID)
	if m.Gender == nil || *m.Gender != memberDomain.GenderFemale {
		t.Errorf("gender = %v, want mujer", m.Gender)
	}
}

// TestUC046_Gender_HeaderMissingStillWorks — un CSV pre-feature (8 cols
// sin gender) debe seguir importando sin error.
func TestUC046_Gender_HeaderMissingStillWorks(t *testing.T) {
	f := setupMembersFixture(t)
	body := csvHeader + "\n" + "Sin Genero,5512300020,,,,,,\n"
	out := runImport(t, f, body, false)
	if out.ImportedCount != 1 {
		t.Fatalf("imported=%d, want 1 (out=%+v)", out.ImportedCount, out)
	}
	tx, _ := f.uow.Query(context.Background())
	m, _ := f.memberRepo.GetByID(tx, out.Imported[0].MemberID)
	if m.Gender != nil {
		t.Errorf("gender should stay nil for missing column, got %v", *m.Gender)
	}
}
