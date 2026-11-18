-- v0.11.0: how the Worker checked this Hub (hub_check of its signed hello:
-- none, fingerprint, invite or mismatch; '' when it never said, for example a
-- Worker older than v0.11.0 or one that has not connected yet). The Hub keeps
-- the last report, so the approval page still shows "not verified" for a
-- pending Worker that is offline.
ALTER TABLE workers ADD COLUMN hub_check TEXT NOT NULL DEFAULT '';
