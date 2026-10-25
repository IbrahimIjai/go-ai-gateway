# Design-only schema

`003_identity_and_topups.up.sql` sketches users, memberships and payment
top-ups with webhook de-duplication. It is **not applied**: the migrator only
reads `db/migrations/`. It becomes a migration when a real payment provider is
integrated; until then credits are granted through the admin API.
