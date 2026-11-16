//go:build server

package repositories

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"

	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	"github.com/cuadra/cuadra-core/src/shared/sync/personal/domain/repository"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type Snapshots struct{ canonical protocol.Canonicalizer }

func NewSnapshots(c protocol.Canonicalizer) (*Snapshots, error) {
	if c == nil {
		return nil, protocol.ErrCommandInvalid
	}
	return &Snapshots{canonical: c}, nil
}

var _ repository.Snapshots = (*Snapshots)(nil)

// RR is a separate capability; command and receipt repositories retain their
// stricter READ COMMITTED guard. Both paths must hold the account mutex.
func snapshotConnection(handle shared.Transaction, accountID uuid.UUID, materializing bool) (*gorm.DB, error) {
	if !materializing {
		return dataConnection(handle, accountID, true)
	}
	h, ok := handle.(*shared.GormTransaction)
	if !ok || h == nil || h.Tx == nil || h.Tx.Statement == nil || !protocol.ValidID(accountID) {
		return nil, protocol.ErrCommandScope
	}
	if _, ok = h.Tx.Statement.ConnPool.(gorm.TxCommitter); !ok {
		return nil, protocol.ErrCommandScope
	}
	tx := h.Tx.Session(&gorm.Session{Logger: logger.Default.LogMode(logger.Silent), SkipDefaultTransaction: true})
	var scope struct {
		Account, Gym, Attempt, AttemptHash, Isolation, ReadOnly string
		DataOnly                                                bool
	}
	err := tx.Raw(`SELECT coalesce(current_setting('mancuerna.account_id',true),'') AS account,
 coalesce(current_setting('tinta.gym_id',true),'') AS gym,coalesce(current_setting('mancuerna.attempt_id',true),'') AS attempt,
 coalesce(current_setting('mancuerna.attempt_hash',true),'') AS attempt_hash,current_setting('transaction_isolation') AS isolation,
 current_setting('transaction_read_only') AS read_only,(pg_has_role(current_user,'mancuerna_data','USAGE')
 AND NOT pg_has_role(current_user,'mancuerna_auth','MEMBER') AND NOT pg_has_role(current_user,'mancuerna_identity','MEMBER')) AS data_only`).Scan(&scope).Error
	if err != nil {
		return nil, err
	}
	if scope.Account != accountID.String() || scope.Gym != "" || scope.Attempt != "" || scope.AttemptHash != "" || !scope.DataOnly || scope.Isolation != "repeatable read" || scope.ReadOnly != "off" {
		return nil, protocol.ErrCommandScope
	}
	// Also enforce the MVCC touch when invoked by a caller other than the
	// standard UoW: a stale RR view cannot acquire authority after revocation.
	var locked uuid.UUID
	if err = tx.Raw("UPDATE mancuerna.sync_streams SET last_sequence=last_sequence WHERE account_id=? RETURNING account_id", accountID).Row().Scan(&locked); err != nil {
		return nil, err
	}
	return tx, nil
}
func snapshotError(err error) error {
	for _, e := range []error{protocol.ErrSnapshotMissing, protocol.ErrSnapshotExpired, protocol.ErrSnapshotConflict} {
		if errors.Is(err, e) {
			return e
		}
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" {
		return protocol.ErrSnapshotConflict
	}
	return persistenceError(err)
}
func (r *Snapshots) RequireActor(handle shared.Transaction, actor protocol.Actor, materializing bool) (err error) {
	defer func() { err = snapshotError(err) }()
	if !protocol.ValidID(actor.SessionID) || !protocol.ValidID(actor.DeviceID) {
		return protocol.ErrCommandActor
	}
	tx, err := snapshotConnection(handle, actor.AccountID, materializing)
	if err != nil {
		return err
	}
	var active bool
	if err = tx.Raw("SELECT mancuerna_private.session_active(?,?)", actor.SessionID, actor.DeviceID).Scan(&active).Error; err != nil {
		return err
	}
	if !active {
		return protocol.ErrCommandActor
	}
	return nil
}
func (r *Snapshots) State(handle shared.Transaction, accountID uuid.UUID, materializing bool) (out protocol.StreamState, err error) {
	defer func() {
		err = snapshotError(err)
		if err != nil {
			out = protocol.StreamState{}
		}
	}()
	tx, err := snapshotConnection(handle, accountID, materializing)
	if err != nil {
		return out, err
	}
	err = tx.Raw("SELECT epoch,last_sequence,compacted_through FROM mancuerna.sync_streams WHERE account_id=?", accountID).Row().Scan(&out.Epoch, &out.LastSequence, &out.CompactedThrough)
	if err == nil && (!protocol.ValidID(out.Epoch) || out.LastSequence < 0 || out.CompactedThrough < 0 || out.CompactedThrough > out.LastSequence) {
		err = protocol.ErrCommandPersistence
	}
	return out, err
}
func (r *Snapshots) Get(handle shared.Transaction, accountID, id uuid.UUID, materializing bool) (out protocol.StoredSnapshot, err error) {
	defer func() {
		err = snapshotError(err)
		if err != nil {
			out = protocol.StoredSnapshot{}
		}
	}()
	if !protocol.ValidID(id) {
		return out, protocol.ErrCommandInvalid
	}
	tx, err := snapshotConnection(handle, accountID, materializing)
	if err != nil {
		return out, err
	}
	var raw []byte
	var count int
	err = tx.Raw(`SELECT id,account_id,epoch,through_sequence,page_count,created_at,expires_at,manifest
 FROM mancuerna.sync_snapshots WHERE account_id=? AND id=?`, accountID, id).Row().Scan(&out.ID, &out.AccountID, &out.Epoch, &out.ThroughSequence, &count, &out.CreatedAt, &out.ExpiresAt, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return out, protocol.ErrSnapshotMissing
	}
	if err != nil {
		return out, err
	}
	// An old manifest-less row is retained, never reconstructed with a new TTL.
	if len(raw) == 0 {
		return out, protocol.ErrSnapshotExpired
	}
	if json.Unmarshal(raw, &out.Manifest) != nil || out.Manifest.ID != id.String() || out.Manifest.Epoch != out.Epoch.String() || out.Manifest.ThroughSequence != strconv.FormatInt(out.ThroughSequence, 10) || out.Manifest.ExpiresAt != protocol.Timestamp(out.ExpiresAt) || out.Manifest.PageCount != count || count < 1 || count > protocol.SnapshotMaxPages || len(out.Manifest.PageHashes) != count || out.Manifest.Cursor == "" {
		return out, protocol.ErrCommandPersistence
	}
	for _, hash := range out.Manifest.PageHashes {
		b, e := hex.DecodeString(hash)
		if e != nil || len(b) != 32 || hex.EncodeToString(b) != hash {
			return out, protocol.ErrCommandPersistence
		}
	}
	return out, nil
}

// Narrow root metadata detects missing/extra/unpublished revisions, including
// tombstones. Complete canonical payloads come from validated command effects.
const snapshotRoots = `WITH scope AS (SELECT ?::uuid AS aid), latest AS (
 SELECT DISTINCT ON(entity_type,entity_id) entity_type,entity_id,revision,canonical
 FROM mancuerna.record_revisions WHERE account_id=(SELECT aid FROM scope)
 ORDER BY entity_type,entity_id,revision DESC
), roots AS (
 SELECT 'personal_exercise' AS entity_type,id AS entity_id,revision FROM mancuerna.personal_exercises WHERE account_id=(SELECT aid FROM scope)
 UNION ALL SELECT 'routine',id,revision FROM mancuerna.routines WHERE account_id=(SELECT aid FROM scope)
 UNION ALL SELECT 'routine_draft',routine_id,revision FROM mancuerna.routine_drafts WHERE account_id=(SELECT aid FROM scope)
 UNION ALL SELECT 'routine_version',id,1::bigint FROM mancuerna.routine_versions WHERE account_id=(SELECT aid FROM scope)
 UNION ALL SELECT 'workout',id,revision FROM mancuerna.workouts WHERE account_id=(SELECT aid FROM scope)
 UNION ALL SELECT 'workout_set',id,revision FROM mancuerna.workout_sets WHERE account_id=(SELECT aid FROM scope)
 UNION ALL SELECT 'conflict',id,revision FROM mancuerna.sync_conflicts WHERE account_id=(SELECT aid FROM scope)
), profile AS (
 SELECT 'profile' AS entity_type,id AS entity_id,revision,jsonb_build_object('id',id::text,'display_name',display_name,'weight_unit',weight_unit,'revision',revision::text) AS canonical
 FROM mancuerna.accounts WHERE id=(SELECT aid FROM scope)
) `

func (r *Snapshots) Materialize(handle shared.Transaction, accountID, id uuid.UUID) (hashes []string, err error) {
	defer func() {
		err = snapshotError(err)
		if err != nil {
			hashes = nil
		}
	}()
	if !protocol.ValidID(id) {
		return nil, protocol.ErrCommandInvalid
	}
	tx, err := snapshotConnection(handle, accountID, true)
	if err != nil {
		return nil, err
	}
	var drift bool
	err = tx.Raw(snapshotRoots+`SELECT
 EXISTS(SELECT 1 FROM roots r FULL JOIN (SELECT * FROM latest WHERE entity_type<>'profile') l USING(entity_type,entity_id)
 WHERE r.entity_id IS NULL OR l.entity_id IS NULL OR r.revision<>l.revision)
 OR EXISTS(SELECT 1 FROM latest l FULL JOIN profile p USING(entity_type,entity_id)
 WHERE (l.entity_type='profile' OR p.entity_type='profile') AND
 (p.entity_id IS NULL OR (l.entity_id IS NULL AND p.revision<>1) OR (l.entity_id IS NOT NULL AND (l.revision<>p.revision OR l.canonical<>p.canonical))))
 OR NOT EXISTS(SELECT 1 FROM profile)`, accountID).Scan(&drift).Error
	if err != nil {
		return nil, err
	}
	if drift {
		return nil, protocol.ErrCommandPersistence
	}
	// A server cursor avoids an account-sized slice; close each FETCH result
	// before INSERT so pgx never has two commands using one busy connection.
	err = tx.Exec(`DECLARE mancuerna_snapshot_records NO SCROLL CURSOR FOR `+snapshotRoots+`, records AS (
 SELECT entity_type,entity_id,revision,canonical FROM latest WHERE entity_type<>'profile'
 UNION ALL SELECT entity_type,entity_id,revision,canonical FROM profile
), payloads AS (
 SELECT entity_type,entity_id,jsonb_build_object('entity_type',entity_type,'entity_id',entity_id::text,'revision',revision::text,'data',canonical)::text AS payload FROM records
) SELECT CASE WHEN octet_length(payload)<=1048576 THEN payload END FROM payloads ORDER BY entity_type COLLATE "C",entity_id`, accountID).Error
	if err != nil {
		return nil, err
	}
	defer func() {
		if e := tx.Exec("CLOSE mancuerna_snapshot_records").Error; err == nil {
			err = e
		}
	}()
	page := make([]protocol.Change, 0, protocol.SnapshotPageRecords)
	storedBytes, wireBytes := 2, 2
	flush := func() error {
		if len(hashes) >= protocol.SnapshotMaxPages {
			return protocol.ErrCommandPersistence
		}
		raw, e := json.Marshal(page)
		if e != nil {
			return e
		}
		canonical, e := r.canonical.Canonicalize(raw)
		if e != nil || len(canonical) > protocol.PullPageBytes {
			return protocol.ErrCommandPersistence
		}
		hash := sha256.Sum256(canonical)
		if e = tx.Exec("INSERT INTO mancuerna.sync_snapshot_pages(account_id,snapshot_id,page_number,data,hash) VALUES(?,?,?,?::jsonb,?)", accountID, id, len(hashes), string(canonical), hash[:]).Error; e != nil {
			return e
		}
		hashes = append(hashes, hex.EncodeToString(hash[:]))
		page = make([]protocol.Change, 0, protocol.SnapshotPageRecords)
		storedBytes, wireBytes = 2, 2
		return nil
	}
	for {
		var raw []byte
		e := tx.Raw("FETCH NEXT FROM mancuerna_snapshot_records").Row().Scan(&raw)
		if errors.Is(e, sql.ErrNoRows) {
			break
		}
		if e != nil {
			return nil, e
		}
		if len(raw) == 0 {
			return nil, protocol.ErrCommandPersistence
		}
		var change protocol.Change
		if json.Unmarshal(raw, &change) != nil || !validSnapshotChange(change) {
			return nil, protocol.ErrCommandPersistence
		}
		canonical, e := r.canonical.Canonicalize(raw)
		if e != nil {
			return nil, e
		}
		sepStored, sepWire := 0, 0
		if len(page) > 0 {
			sepStored, sepWire = 2, 1
		}
		if len(page) == protocol.SnapshotPageRecords || storedBytes+sepStored+len(raw) > protocol.PullPageBytes || wireBytes+sepWire+len(canonical) > protocol.PullPageBytes {
			if len(page) == 0 {
				return nil, protocol.ErrCommandPersistence
			}
			if e = flush(); e != nil {
				return nil, e
			}
			sepStored, sepWire = 0, 0
		}
		if 2+len(raw) > protocol.PullPageBytes || 2+len(canonical) > protocol.PullPageBytes {
			return nil, protocol.ErrCommandPersistence
		}
		page = append(page, change)
		storedBytes += sepStored + len(raw)
		wireBytes += sepWire + len(canonical)
	}
	if len(page) > 0 || len(hashes) == 0 {
		if err = flush(); err != nil {
			return nil, err
		}
	}
	return hashes, nil
}
func validSnapshotChange(c protocol.Change) bool {
	id, err := uuid.Parse(c.EntityID)
	rev, e := protocol.ParseRevision(c.Revision)
	if err != nil || !protocol.ValidID(id) || id.String() != c.EntityID || e != nil || rev < 1 {
		return false
	}
	switch c.EntityType {
	case "profile", "personal_exercise", "routine", "routine_draft", "routine_version", "workout", "workout_set", "conflict":
	default:
		return false
	}
	var identity struct {
		ID       string `json:"id"`
		Revision string `json:"revision"`
	}
	return json.Unmarshal(c.Data, &identity) == nil && identity.ID == c.EntityID && identity.Revision == c.Revision
}
func (r *Snapshots) Save(handle shared.Transaction, s protocol.StoredSnapshot) (err error) {
	defer func() { err = snapshotError(err) }()
	if !protocol.ValidID(s.ID) || !protocol.ValidID(s.Epoch) || s.ThroughSequence < 0 || s.CreatedAt.IsZero() || !s.ExpiresAt.Equal(s.CreatedAt.Add(protocol.SnapshotTTL)) || s.Manifest.ID != s.ID.String() || s.Manifest.Epoch != s.Epoch.String() || s.Manifest.ThroughSequence != strconv.FormatInt(s.ThroughSequence, 10) || s.Manifest.ExpiresAt != protocol.Timestamp(s.ExpiresAt) || s.Manifest.PageCount < 1 || s.Manifest.PageCount > protocol.SnapshotMaxPages || len(s.Manifest.PageHashes) != s.Manifest.PageCount {
		return protocol.ErrCommandInvalid
	}
	tx, err := snapshotConnection(handle, s.AccountID, true)
	if err != nil {
		return err
	}
	var count int
	if err = tx.Raw("SELECT count(*) FROM mancuerna.sync_snapshot_pages WHERE account_id=? AND snapshot_id=?", s.AccountID, s.ID).Scan(&count).Error; err != nil {
		return err
	}
	if count != s.Manifest.PageCount {
		return protocol.ErrCommandPersistence
	}
	raw, err := json.Marshal(s.Manifest)
	if err != nil {
		return err
	}
	return tx.Exec(`INSERT INTO mancuerna.sync_snapshots(id,account_id,epoch,through_sequence,page_count,created_at,expires_at,manifest)
 VALUES(?,?,?,?,?,?,?,?::jsonb)`, s.ID, s.AccountID, s.Epoch, s.ThroughSequence, s.Manifest.PageCount, s.CreatedAt, s.ExpiresAt, string(raw)).Error
}
func (r *Snapshots) Page(handle shared.Transaction, accountID, id uuid.UUID, number int) (out protocol.SnapshotPage, err error) {
	defer func() {
		err = snapshotError(err)
		if err != nil {
			out = protocol.SnapshotPage{}
		}
	}()
	if !protocol.ValidID(id) || number < 0 || number >= protocol.SnapshotMaxPages {
		return out, protocol.ErrCommandInvalid
	}
	tx, err := snapshotConnection(handle, accountID, false)
	if err != nil {
		return out, err
	}
	var raw, hash []byte
	err = tx.Raw("SELECT data,hash FROM mancuerna.sync_snapshot_pages WHERE account_id=? AND snapshot_id=? AND page_number=?", accountID, id, number).Row().Scan(&raw, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return out, protocol.ErrSnapshotMissing
	}
	if err != nil {
		return out, err
	}
	if len(raw) > protocol.PullPageBytes || len(hash) != 32 || json.Unmarshal(raw, &out.Records) != nil || out.Records == nil || len(out.Records) > protocol.SnapshotPageRecords {
		return out, protocol.ErrCommandPersistence
	}
	canonical, err := r.canonical.Canonicalize(raw)
	if err != nil {
		return out, err
	}
	sum := sha256.Sum256(canonical)
	if hex.EncodeToString(sum[:]) != hex.EncodeToString(hash) {
		return out, protocol.ErrCommandPersistence
	}
	for _, record := range out.Records {
		if !validSnapshotChange(record) {
			return out, protocol.ErrCommandPersistence
		}
	}
	out.SnapshotID = id.String()
	out.PageNumber = number
	out.Hash = hex.EncodeToString(sum[:])
	return out, nil
}
