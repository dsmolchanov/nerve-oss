# Modern hosted billing status

The authenticated `nerve_billing_status` read accepts only `{}` and preserves
its original live organization/generation bearer through signed delegation.
`active_tier` and `starter_active` describe independently proven current paid
access, never a pending provider update or an offer selected by the caller.

When a saved tier change belongs to the current hosted root, the closed result
may include both `tier_change_state` and `tier_change_offer_id`. Both fields
are omitted when no such saved decision exists; a single, empty, null or
unknown value is refused. No change ID, provider ID or payment URL is exposed.

The only offer identifiers are `growth_2026_09_v2` and `scale_2026_09_v2`.
The only states are `awaiting_owner`, `attempt_prepared`, `provider_unknown`,
`pending_payment`, `operator_review`, `applied` and `terminal`.
Before application, active access remains Starter or empty when its period
expired. An applied decision permits only its exact target tier or empty when
current paid proof has expired. A terminal unsuccessful decision does not
replace separately proven current access. No state alone activates a target.

This read creates no purchase, changes no quota and does not retry a mutation.
Source/runtime qualification and the pricing release gates remain required.
