-- App install on the Android Worker (app_install, v0.25.0).
--
-- The apps_install flag lets the source of a session grant, or the master
-- sessions of an accepted Worker, install apps on the target's Android Worker
-- from its Minion files folder. Every existing row keeps 0: installing apps is
-- never implied by files_push or any earlier flag. On a session grant it needs
-- a verified source (source_incarnation <> '') and level Control, like
-- files_push; unbinding the grant clears it.
ALTER TABLE session_grant ADD COLUMN apps_install INTEGER NOT NULL DEFAULT 0
    CHECK (apps_install = 0 OR (apps_install = 1 AND level = 3 AND source_incarnation <> ''));

ALTER TABLE control_accept ADD COLUMN apps_install INTEGER NOT NULL DEFAULT 0 CHECK (apps_install IN (0, 1));
