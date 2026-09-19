-- events_pkey is already a btree on global_seq (it's the IDENTITY primary
-- key), so this second single-column index is redundant: every insert pays
-- to maintain it and no query benefits, since the planner already has the PK
-- index available for the same access pattern (see docs/adr/0001, which
-- depends on global_seq being indexed, not on this specific index existing).
DROP INDEX events_global_seq_idx;
