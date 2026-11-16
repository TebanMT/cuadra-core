-- Owner review of a past cash session whose cash can no longer be counted on
-- its own (forgotten opening, payment after the final count, a session left
-- open overnight). The session stays closed_unverified and never shows a
-- difference; the review only stops it from requesting attention. It is valid
-- while reviewed_at >= closed_at: any later refresh re-closes the session and
-- invalidates the review without coupling it to the status CHECK.
BEGIN;
ALTER TABLE cash_close_events ADD COLUMN reviewed_at INTEGER;
ALTER TABLE cash_close_events ADD COLUMN reviewed_by TEXT REFERENCES users(id) ON DELETE RESTRICT;
ALTER TABLE cash_close_events ADD COLUMN review_note TEXT;
INSERT INTO _migrations(version,name,applied_at) VALUES(48,'048_cash_session_review',CAST(strftime('%s','now') AS INTEGER)*1000);
COMMIT;
