-- Worker names start with their owner (v0.9.1, A20): "<owner>-<name>", where
-- <owner> is the owner's username without an e-mail domain, in lower case,
-- with dots, underscores and spaces as dashes. New names get the prefix in
-- the store (worker_names.go); this renames the Workers that exist. A rename
-- that would collide with another Worker of the same owner is skipped (OR
-- IGNORE): that Worker keeps its name and can be renamed by hand.
WITH slugs AS (
    SELECT id AS owner_id,
           lower(replace(replace(replace(
               CASE WHEN instr(username, '@') > 1 THEN substr(username, 1, instr(username, '@') - 1) ELSE username END,
               '.', '-'), '_', '-'), ' ', '-')) AS slug
    FROM users
)
UPDATE OR IGNORE workers
   SET name = substr((SELECT slug FROM slugs WHERE slugs.owner_id = workers.owner_id) || '-' || name, 1, 64)
 WHERE (SELECT slug FROM slugs WHERE slugs.owner_id = workers.owner_id) <> ''
   AND lower(name) <> (SELECT slug FROM slugs WHERE slugs.owner_id = workers.owner_id)
   AND lower(name) NOT LIKE (SELECT slug FROM slugs WHERE slugs.owner_id = workers.owner_id) || '-%';
