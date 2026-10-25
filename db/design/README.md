# Design-only schema

These files describe the credit ledger (holds, settlement, reconciliation) and
identity/top-up tables planned for Phase 6. They are **not applied**: the
migrator only reads `db/migrations/`. They will be adapted to the Phase 5
schema (text deployment IDs from the catalog file, `inference_requests.id`
as text) when Phase 6 begins.
