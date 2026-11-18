-- The verified lineage of the pin (v0.11.0 trust ceremony). When the pin of
-- a checked enrollment (hub_check_anchor set) moves along a signed transition
-- chain, the keys it moved through (the anchor's successors, not the current
-- pin) are recorded here as full fingerprints separated by spaces. A setting
-- hub_fingerprint that names one of them names an authenticated predecessor
-- of the pin (TUF root rotation). A new check (a new anchor) empties it.
ALTER TABLE hub_enrollment ADD COLUMN hub_check_lineage TEXT NOT NULL DEFAULT '';
