---
repository: nerve-oss
status: approved
date: 2026-11-05
---

# Isolate the empty Core29 rollback qualification

Prerequisite of the owner's approved successor QA mechanism in nerve-cloud.
The Cloud38 provider-start trigger depends on Core29 columns. The shared
`TestCoreMigration29EmptyDownSucceeds` accidentally applies Cloud through the
combined latest-schema helper before testing a Core-only rollback. Its Cloud
mirror therefore tries to remove columns while a later Cloud trigger depends
on them. This is a fixture ownership error, not permission to cascade-drop
Cloud objects or weaken any guarded Down.

Scope: change only that test's setup to the real `MigrateCore(ctx, db)` owner.
Keep the existing actual Core32→28 Down and dropped-policy-table assertions.
No runtime, migration, auth, dependency, workflow or policy change. Cloud-owned
historical tests independently remove empty Cloud successors using guarded Down.
Validate the real PostgreSQL regression and full Store suite, then required CI
and clean current-head review. Consume the accepted exact OSS source in Cloud.
