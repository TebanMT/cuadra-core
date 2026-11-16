-- Overnight check of a drawer's first opening of the day. When the last count
-- left a known amount, opening_expected_cash freezes what should be in the
-- drawer (that cash left plus cash that moved after the count) and
-- opening_note explains a different opening. Both stay NULL when there is no
-- trustworthy reference (first opening ever, last count unfinished or old).
-- Opening differences are reported, never block the opening, so the CHECK
-- only bounds the note: an older device that syncs without these columns
-- must not be rejected.
BEGIN;

ALTER TABLE public.cash_close_events
  ADD COLUMN IF NOT EXISTS opening_expected_cash NUMERIC(12,2),
  ADD COLUMN IF NOT EXISTS opening_note TEXT;

ALTER TABLE public.cash_close_events
  DROP CONSTRAINT IF EXISTS chk_cash_sessions_opening_check_shape;
ALTER TABLE public.cash_close_events
  ADD CONSTRAINT chk_cash_sessions_opening_check_shape CHECK (
    (opening_expected_cash IS NULL OR opening_expected_cash >= 0)
    AND (opening_note IS NULL OR char_length(trim(opening_note)) BETWEEN 1 AND 500)
  );

INSERT INTO _migrations(version, name)
VALUES(68, '068_cash_opening_check')
ON CONFLICT(version) DO NOTHING;

COMMIT;
