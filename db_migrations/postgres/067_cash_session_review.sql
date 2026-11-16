-- Owner review of a past cash session whose cash can no longer be counted on
-- its own (forgotten opening, payment after the final count, a session left
-- open overnight). The session stays closed_unverified and never shows a
-- difference; the review only stops it from requesting attention. It is valid
-- while reviewed_at >= closed_at, so the CHECK deliberately ignores status:
-- an older device that refreshes the session without knowing these columns
-- must not be rejected by sync.
BEGIN;

ALTER TABLE public.cash_close_events
  ADD COLUMN IF NOT EXISTS reviewed_at TIMESTAMPTZ,
  ADD COLUMN IF NOT EXISTS reviewed_by UUID REFERENCES public.users(id) ON DELETE RESTRICT,
  ADD COLUMN IF NOT EXISTS review_note TEXT;

ALTER TABLE public.cash_close_events
  DROP CONSTRAINT IF EXISTS chk_cash_sessions_review_shape;
ALTER TABLE public.cash_close_events
  ADD CONSTRAINT chk_cash_sessions_review_shape CHECK (
    (reviewed_at IS NULL AND reviewed_by IS NULL AND review_note IS NULL)
    OR (reviewed_at IS NOT NULL AND reviewed_by IS NOT NULL
        AND review_note IS NOT NULL AND char_length(trim(review_note)) BETWEEN 1 AND 500)
  );

INSERT INTO _migrations(version, name)
VALUES(67, '067_cash_session_review')
ON CONFLICT(version) DO NOTHING;

COMMIT;
