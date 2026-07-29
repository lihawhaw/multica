ALTER TABLE issue DROP CONSTRAINT IF EXISTS issue_on_children_done_check;
ALTER TABLE issue DROP COLUMN IF EXISTS on_children_done;
