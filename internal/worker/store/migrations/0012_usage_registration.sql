-- An existing enrollment gives no implicit permission for expanded telemetry.
ALTER TABLE hub_enrollment ADD COLUMN usage_policy INTEGER NOT NULL DEFAULT 0;
CREATE TRIGGER usage_registration_changed AFTER UPDATE OF hub_pub,worker_id,owner ON hub_enrollment
WHEN OLD.hub_pub<>NEW.hub_pub OR OLD.worker_id<>NEW.worker_id OR OLD.owner<>NEW.owner
BEGIN
    UPDATE hub_enrollment SET usage_policy=0 WHERE id=NEW.id;
END;
