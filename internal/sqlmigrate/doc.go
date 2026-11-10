// Package sqlmigrate is the single forward-only SQL migration runner of
// Minion (AD-16, spine amendment AM-2). The Hub store and the Worker store
// both embed their own NNNN_name.sql files and hand them to [Apply]; neither
// writes a runner of its own.
//
// The runner works on a *sql.DB it is given and imports no SQLite driver
// (only hub/store and worker/store do, AD-1). It
//
//   - records every applied migration in the schema_migrations table;
//   - refuses a database that has applied a version the binary does not know
//     ([ErrSchemaTooNew], code "schema_too_new");
//   - writes a VACUUM INTO backup of the database before it applies anything
//     to a database that already holds data;
//   - applies all pending migrations in one transaction, so a failing
//     migration leaves the schema untouched.
//
// Migrations never go back: there is no down step and no database downgrade.
//
// Owner: Story 3.1.
package sqlmigrate
