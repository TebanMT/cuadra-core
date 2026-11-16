-- Overnight check of a drawer's first opening of the day. When the last count
-- left a known amount, opening_expected_cash freezes what should be in the
-- drawer (that cash left plus cash that moved after the count) and
-- opening_note explains a different opening. Both stay NULL when there is no
-- trustworthy reference (first opening ever, last count unfinished or old).
-- Opening differences are reported, never block the opening.
BEGIN;
ALTER TABLE cash_close_events ADD COLUMN opening_expected_cash INTEGER;
ALTER TABLE cash_close_events ADD COLUMN opening_note TEXT;
INSERT INTO _migrations(version,name,applied_at) VALUES(49,'049_cash_opening_check',CAST(strftime('%s','now') AS INTEGER)*1000);
COMMIT;
