# Repository blocking invariants

This file is **owned by this repository**. `plaintalk-dev-agent` installs it once
and never overwrites it, so anything added here survives every fleet-wide policy
refresh.

The fleet-wide invariants live in the `dev-agent:policy` managed block in
`AGENTS.md` and are replaced wholesale on each refresh — do not add
repository-specific entries there, they will be deleted.

## How to use this file

Add an entry when a P1 class keeps coming back; `AGENTS.md`, "Fix the invariant,
not the instance", says at what point. A repeated finding is
a missing rule, not a new discovery: semantic review is an expensive way to
rediscover the same defect, and each round costs a full review generation.

Per the fleet policy, the commit that fixes a recurring P1 should also add the
deterministic check that would have caught it — a lint rule, a test, or a
migration assertion. This list is the human-readable index of those rules, not a
replacement for them.

Each entry should be a closed question a reviewer can answer yes or no. "No route
without an auth dependency" is checkable; "input should be validated" is not, and
an open-ended predicate is satisfiable on any non-trivial diff, which is what
makes a review loop unable to terminate.

## Invariants

<!-- Add entries below. Example shape:

- No handler under `apps/api/routes/` may be registered without a
  `Depends(require_tenant)` argument. Enforced by `tests/test_route_auth.py`.

-->

- MCP conformance checkout overrides must be canonical absolute paths before
  they are passed to a Go package test, whose working directory differs from
  the repository root. Enforced by
  `scripts/ci/test_mcp_conformance_paths.sh`.
- After an onboarding lifecycle mutation (`Start`, `VerifyDomain`, or `Close`)
  may have reached the control plane, does every transport, body-read,
  protocol, or semantically invalid-envelope failure that is not a validated
  durable result or business error return `ErrOnboardingOutcomeUnknown`?
  `Status` is the intentional read-only exception for protocol diagnostics.
  Enforced by the table-driven
  `TestClientResponseBodyTimeoutReturnsOutcomeUnknownForEveryOperation`,
  `TestClientTransportDisconnectReturnsOutcomeUnknownForEveryOperation`,
  `TestClientInvalidPostCommitResponseReturnsOutcomeUnknownForEveryMutation`,
  and `TestClientRejectsSemanticallyInvalidEnvelopesForEveryOperation` tests in
  `internal/onboarding/client_test.go`.
- Do all inbox address lookup, receiving resolution, create, ensure, and
  reactivate paths use the one canonical-equivalence rule, prefer a single
  active row before disabled history where replay is supported, fail closed on
  ambiguity within that selected tier, serialize canonical-variant writers,
  preserve stored legacy bytes, and canonicalize those bytes before outbound
  policy comparison or sender emission? Enforced by
  `TestCanonicalInboxAddressSQLMatchesGoCanonicalization`,
  `TestLegacyCanonicalInboxAddressMatrix`,
  `TestCanonicalInboxDisabledHistorySemantics`,
  `TestInboxInsertHelpersRequireTransaction`,
  `TestCanonicalInboxAddressBoundariesRejectAmbiguousLegacyRows`,
  `TestResolveReceivingInboxRejectsAmbiguityBeforeReadinessFilter`,
  `TestReactivateInboxForOrgUsesCanonicalIdentityWithoutRewritingLegacyBytes`,
  `TestReactivateInboxForOrgMapsCanonicalUniqueBackstop`,
  `TestCanonicalVariantWritersSerializeBeforePreinsertCheck`,
  `TestOutboundPolicyCanonicalizesLegacyInboxAddressForOwnedDomainProof`, and
  `TestCanonicalOutboundInboxAddressPreservesStorageOnlyInternally`.

- A candidate release identity is only free when a probe *positively proves* it
  absent. `scripts/release/prove_candidate_version_unused.sh` is the single
  implementation, run both before and after the image build, and each probe
  distinguishes absence from "could not tell": git `ls-remote --exit-code` must
  exit 2, the Releases API must return 404, and an OCI probe must report the
  registry's own manifest-unknown signal. A nonzero exit, a non-404 status, or a
  generic "not found" is not proof and must refuse. Enforced by
  `scripts/ci/test_candidate_version_probe.sh`.

- Schema quiescence opens no listener and constructs no HTTP/auth/store/provider
  dependency; serve and worker wait only for cancellation. Retryable maintenance
  responses belong to the external ingress and require deployment verification.
  Enforced by `TestSchemaQuiescenceOpensNoListener`,
  `TestSchemaQuiescenceHasNoNetworkDependency`, and
  `scripts/ci/test_schema_quiescence_executables.py`.

- Cloud mode must never reach the local debug renderer, even if the handler is
  accidentally mounted. OSS debug accepts the owner key and rejects mailbox
  keys. Enforced by `TestDebugAccessMatrix` in internal/app/debug_access_test.go.

- Self-host smoke process failures must not log credentials through argv, stderr,
  timeout/spawn exceptions or chained tracebacks. Enforced by the restored/empty
  database failure matrix in `scripts/ci/test_selfhost_smoke.py`.

- Do the RFC 5322 threading headers name the reply target exactly once, keep
  each repeated ancestor at its latest position, and stay inside a header line
  for every provider? `References` carries the ancestors and the outbox worker
  appends the reply target, so a producer that also names it there emits it
  twice; ancestors come from inbound mail, so a sender can repeat one or name
  the message's own identifier, and both the store read and the shared assembly
  helper deduplicate. Every size bound is derived from RFC 5322's 998-character
  line and the header's own name, never a round number, so a valid identifier
  is discarded only when it genuinely cannot be serialized. Enforced by
  `TestThreadingHeadersNameTheReplyTargetExactlyOnce`,
  `TestGetThreadReplyTargetDerivesAndSanitizesThreading` and
  `TestSMTPThreadingHeadersStayInsideTheLineLimit`.

- Does every hybrid installation-state change either confirm its directory
  synchronization or report the change as unconfirmed, and never report an
  unconfirmed change as success? A directory mutation is not durable until the
  sync positively succeeds, so `realSyncDirectory` propagates every failure
  including the `fs.ErrInvalid` of a filesystem without directory sync, `Save`
  and `Remove` retry it and return `*UnconfirmedError` when it does not
  succeed, any path that resumes a transitional state — a pairing awaiting
  admission, a prepared rotation, an existing binding — must itself re-write
  that state rather than returning from a fast path, and a key is offered for
  admission only when it is readable from disk. The confirmed entry is the
  whole chain the state file depends on, not only the destination: each entry
  is recorded by its parent, so `Save` confirms from the state directory up to
  the filesystem root, resolving `hybrid.state_path` to an absolute path with
  confirms every entry the state file is reachable through: each prefix of the
  absolute path, and for a prefix that is a symlink, its target's ancestry in
  turn. A relative walk terminates at `.` and never reaches the working
  directory's ancestors; confirming only the fully resolved path and the
  original spelling misses the entry recording an intermediate hop. That obligation is discharged on every save rather than
  inferred from which directories already exist — a directory can be left
  behind by an earlier save that created it and could not confirm it, and no
  record of that debt can itself be stored durably, so existence is never proof
  of durability. Enforced by `TestHybridStateSaveReportsHowFarItGot`,
  `TestHybridStateSaveRetriesTheSynchronizationAFailedSaveOwed`,
  `TestHybridStateSaveSynchronizesEveryDirectoryItCreates`,
  `TestHybridStateSaveConfirmsTheChainEvenWhenNothingIsCreated`,
  `TestHybridStateSaveConfirmsTheAbsoluteChainForARelativePath`,
  `TestHybridStateSaveConfirmsBothSidesOfASymlink`,
  `TestHybridStateSaveConfirmsEveryHopOfANestedSymlink`,
  `TestHybridStateRefusesToClaimUnsupportedSyncIsDurable`,
  `TestHybridTransitionalWritesAreCompletedOnResume`,
  `TestHybridConnectHandlesBothSidesOfAnUncertainSave`,
  `TestHybridConnectRerunPerformsTheDurabilityOperationItPromises`,
  `TestHybridConnectRerunWhilePairingReprintsTheKeyBeforeCallingCloud`,
  `TestHybridRotatePrepareShowsTheKeyEvenWhenUnconfirmed` and
  `TestHybridNeverShowsAKeyThatWasNotStored`.

- Outbox provider/budget timeouts must leave a live, bounded context for every
  outcome transition and unstarted-claim requeue within the shared batch drain
  limit; a normally expired batch must not stop Run after successful cleanup.
  Enforced by TestOutboxTimeoutPersistsEveryProviderOutcome,
  TestOutboxBatchTimeoutRequeuesUnstartedClaimsAndCanContinue and
  TestTenClaimedRowsShareOneDrainDeadlineWhenRequeueBlocks.
