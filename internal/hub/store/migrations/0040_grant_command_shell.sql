-- Trusted command shells (A116): a Control grant on one named target session
-- may say that this session is a command shell of the source (master)
-- session. The Hub then tells the target Worker, for a send_text or send_keys
-- of that source to that target, that the shell_pane guard does not apply:
-- typing into that shell is a normal write, not a forced override. Every
-- other guard stays.
--
-- command_shell is 1 only on a Control grant (level 3) that names one target
-- session; the store and the handler keep that rule, and the CHECK below
-- refuses any other row. Existing grants keep 0: nothing changes for them.
ALTER TABLE session_grant ADD COLUMN command_shell INTEGER NOT NULL DEFAULT 0
    CHECK (command_shell = 0 OR (command_shell = 1 AND level = 3 AND target_session <> ''));
