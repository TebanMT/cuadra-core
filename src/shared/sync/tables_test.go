package sync

import "testing"

// TestSyncedTablesUniqueAndComplete ensures the registry doesn't drift —
// duplicate types or missing key columns would silently break apply / pull.
//
// Composite-key tables are exempt from the `id` / `created_at` checks
// because they have neither: their identity is the natural key declared
// in CompositeKey, and the LWW dimensions reduce to (version, updated_at).
func TestSyncedTablesUniqueAndComplete(t *testing.T) {
	seen := map[string]bool{}
	requiredCols := []string{"id", "gym_id", "version", "created_at", "updated_at", "deleted_at"}
	requiredColsComposite := []string{"gym_id", "version", "updated_at", "deleted_at"}
	for _, et := range SyncedTables {
		if seen[et.Type] {
			t.Errorf("duplicate entity_type in registry: %q", et.Type)
		}
		seen[et.Type] = true

		if et.Table == "" {
			t.Errorf("type %q has empty table name", et.Type)
		}
		colSet := map[string]bool{}
		for _, c := range et.Columns {
			colSet[c] = true
		}
		req := requiredCols
		if len(et.CompositeKey) > 0 {
			req = requiredColsComposite
			for _, k := range et.CompositeKey {
				if !colSet[k] {
					t.Errorf("type %q composite key references missing column %q", et.Type, k)
				}
			}
		}
		for _, c := range req {
			if !colSet[c] {
				t.Errorf("type %q missing required column %q", et.Type, c)
			}
		}
	}
}

func TestFindTable(t *testing.T) {
	if got := FindTable("members"); got == nil || got.Table != "members" {
		t.Errorf("FindTable(members) = %v", got)
	}
	if got := FindTable("nonexistent"); got != nil {
		t.Errorf("FindTable(nonexistent) = %v, want nil", got)
	}
}
