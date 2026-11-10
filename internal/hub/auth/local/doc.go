// Package local implements local accounts: Argon2id password hashing, the
// one-time admin seed and password login (AD-10). It is the only package that
// imports argon2 (AD-1).
//
// Owner: Story 3.5.
//
// The package is split by concern and talks to the database only through the
// small interfaces it declares ([SeedStore], [LoginStore]), which *store.Store
// implements:
//
//   - password.go: [Hasher], Argon2id with 64 MiB, 3 iterations and
//     parallelism 2, PHC-encoded, verified in constant time;
//   - seed.go: [Seeder], the admin seed that runs once on an empty user table;
//   - login.go: [Authenticator], which checks credentials behind the shared
//     rate limiter and audits every attempt, and [Handler], the HTTP
//     endpoints POST /auth/login and POST /auth/logout.
package local
