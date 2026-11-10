ALTER TABLE items ADD COLUMN note TEXT;
CREATE INDEX items_name ON items (name);
