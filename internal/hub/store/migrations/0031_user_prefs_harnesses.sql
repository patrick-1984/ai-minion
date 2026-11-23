-- v0.20.0: the coding harnesses a user wants to see on their Workers (the
-- Harnesses page), a comma-separated list of harness names. NULL means the
-- user never chose (the page then offers every known harness); an empty
-- string is a deliberate empty choice.
ALTER TABLE user_prefs ADD COLUMN wanted_harnesses TEXT;
