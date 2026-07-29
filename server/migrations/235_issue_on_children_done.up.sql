-- Per-parent handoff policy: what happens when a parent's sub-issues finish
-- (MUL-5472).
--
-- Today the child-done path has exactly one reaction — wake the parent's
-- assignee — which costs a full agent run even when the parent has nothing
-- left to coordinate. This column lets a parent declare what should happen
-- instead, and defaults to 'auto' so the server infers it from the tree's
-- shape rather than making the user configure anything up front.
--
--   auto   — infer (default). A later stage with pending work still needs a
--            promotion decision, so wake. Otherwise the only open question is
--            "is the parent finished?", which is a rollup, not coordination:
--            post a receipt and ask the owner.
--   wake   — always wake the parent assignee (today's behavior).
--   notify — receipt + actionable inbox item, never start an agent run.
--   close  — receipt, then roll the parent up automatically.
--   off    — do nothing at all.
--
-- NOT NULL with a constant default is a metadata-only add on modern Postgres
-- (no table rewrite), and existing rows land on 'auto'.
ALTER TABLE issue
    ADD COLUMN on_children_done TEXT NOT NULL DEFAULT 'auto';

ALTER TABLE issue
    ADD CONSTRAINT issue_on_children_done_check
    CHECK (on_children_done IN ('auto', 'wake', 'notify', 'close', 'off'));

COMMENT ON COLUMN issue.on_children_done IS
    'Handoff policy applied when this issue''s sub-issues close a stage barrier: auto (infer from tree shape) | wake | notify | close | off. See server/internal/handler/issue_child_done.go (MUL-5472).';
