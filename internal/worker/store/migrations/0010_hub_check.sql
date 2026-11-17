-- How the pinned Hub was checked (v0.11.0 trust ceremony). hub_check is
--   none         first use, or enrolled by a version before 0.11
--   fingerprint  the pinned root matched a fingerprint the operator gave
--   invite       the pinned root matched the invitation key
--   mismatch     the pinned root did not match a fingerprint the operator
--                gave: the Worker does not connect (stored quarantine)
-- hub_check_anchor is the full fingerprint (52 characters) the operator
-- actually checked, or '' when nothing was checked. It stays when the pin
-- moves along a signed transition chain, so the Worker can say that the
-- current pin was reached by a verified rotation from the checked key.
-- hub_check_at is when hub_check was last set (Unix milliseconds).
ALTER TABLE hub_enrollment ADD COLUMN hub_check TEXT NOT NULL DEFAULT 'none'
    CHECK (hub_check IN ('none', 'fingerprint', 'invite', 'mismatch'));
ALTER TABLE hub_enrollment ADD COLUMN hub_check_at INTEGER;
ALTER TABLE hub_enrollment ADD COLUMN hub_check_anchor TEXT NOT NULL DEFAULT '';
