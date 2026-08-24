# NeuralMail MCP Contract (v1)

This document defines the MCP interface for NeuralMail: resources, tools, and JSON schemas.

## Conventions
- JSON Schema draft: 2020-12
- All IDs are opaque strings.
- Timestamps are RFC3339 strings.
- `confidence` is a float in `[0.0, 1.0]`.

## Resource URIs
- `email://inboxes/{inbox_id}`
- `email://inboxes/{inbox_id}/threads?status=open&label=...`
- `email://threads/{thread_id}`
- `email://messages/{message_id}`
- `email://threads/{thread_id}/summary`

## Core Types (JSON Schema)
```json
{
  "$id": "neuralmail/types.json",
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "definitions": {
    "id": {"type": "string", "minLength": 1},
    "timestamp": {"type": "string", "format": "date-time"},
    "direction": {"type": "string", "enum": ["inbound", "outbound"]},
    "confidence": {"type": "number", "minimum": 0, "maximum": 1},
    "label": {"type": "string"},
    "participant": {
      "type": "object",
      "additionalProperties": false,
      "properties": {
        "name": {"type": "string"},
        "email": {"type": "string", "format": "email"}
      },
      "required": ["email"]
    },
    "outbound_attachment": {
      "type": "object",
      "additionalProperties": false,
      "properties": {
        "filename": {"type": "string", "minLength": 1, "maxLength": 255},
        "content_type": {
          "type": "string",
          "enum": [
            "image/png",
            "image/jpeg",
            "image/webp",
            "application/pdf",
            "text/plain",
            "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
            "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
          ]
        },
        "content_base64": {"type": "string", "contentEncoding": "base64"}
      },
      "required": ["filename", "content_type", "content_base64"]
    }
  }
}
```

## Resources (Schemas)
### Inbox
```json
{
  "$id": "neuralmail/resources/inbox.json",
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "id": {"$ref": "neuralmail/types.json#/definitions/id"},
    "address": {"type": "string", "format": "email"},
    "labels": {"type": "array", "items": {"$ref": "neuralmail/types.json#/definitions/label"}},
    "status": {"type": "string", "enum": ["active", "paused"]}
  },
  "required": ["id", "address", "status"]
}
```

### Thread
```json
{
  "$id": "neuralmail/resources/thread.json",
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "id": {"$ref": "neuralmail/types.json#/definitions/id"},
    "inbox_id": {"$ref": "neuralmail/types.json#/definitions/id"},
    "subject": {"type": "string"},
    "status": {"type": "string", "enum": ["open", "closed", "snoozed"]},
    "labels": {"type": "array", "items": {"$ref": "neuralmail/types.json#/definitions/label"}},
    "participants": {"type": "array", "items": {"$ref": "neuralmail/types.json#/definitions/participant"}},
    "updated_at": {"$ref": "neuralmail/types.json#/definitions/timestamp"}
  },
  "required": ["id", "inbox_id", "status", "updated_at"]
}
```

### Message
```json
{
  "$id": "neuralmail/resources/message.json",
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "id": {"$ref": "neuralmail/types.json#/definitions/id"},
    "thread_id": {"$ref": "neuralmail/types.json#/definitions/id"},
    "direction": {"$ref": "neuralmail/types.json#/definitions/direction"},
    "from": {"$ref": "neuralmail/types.json#/definitions/participant"},
    "to": {"type": "array", "items": {"$ref": "neuralmail/types.json#/definitions/participant"}},
    "cc": {"type": "array", "items": {"$ref": "neuralmail/types.json#/definitions/participant"}},
    "subject": {"type": "string"},
    "text": {"type": "string"},
    "html": {"type": "string"},
    "created_at": {"$ref": "neuralmail/types.json#/definitions/timestamp"}
  },
  "required": ["id", "thread_id", "direction", "created_at"]
}
```

### Thread Summary
```json
{
  "$id": "neuralmail/resources/thread_summary.json",
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "thread_id": {"$ref": "neuralmail/types.json#/definitions/id"},
    "summary": {"type": "string"},
    "updated_at": {"$ref": "neuralmail/types.json#/definitions/timestamp"}
  },
  "required": ["thread_id", "summary", "updated_at"]
}
```

## Tools
Each tool has an input and output schema.

### 1) list_threads
List threads in an inbox with filters.

Input schema:
```json
{
  "$id": "neuralmail/tools/list_threads.input.json",
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "inbox_id": {"$ref": "neuralmail/types.json#/definitions/id"},
    "status": {"type": "string", "enum": ["open", "closed", "snoozed"]},
    "label": {"type": "string"},
    "updated_after": {"$ref": "neuralmail/types.json#/definitions/timestamp"},
    "limit": {"type": "integer", "minimum": 1, "maximum": 200, "default": 50},
    "cursor": {"type": "string"}
  },
  "required": ["inbox_id"]
}
```

Output schema:
```json
{
  "$id": "neuralmail/tools/list_threads.output.json",
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "threads": {"type": "array", "items": {"$ref": "neuralmail/resources/thread.json"}},
    "next_cursor": {"type": "string"}
  },
  "required": ["threads"]
}
```

### 2) get_thread
Fetch a thread with messages.

Input schema:
```json
{
  "$id": "neuralmail/tools/get_thread.input.json",
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "thread_id": {"$ref": "neuralmail/types.json#/definitions/id"},
    "include_messages": {"type": "boolean", "default": true}
  },
  "required": ["thread_id"]
}
```

Output schema:
```json
{
  "$id": "neuralmail/tools/get_thread.output.json",
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "thread": {"$ref": "neuralmail/resources/thread.json"},
    "messages": {"type": "array", "items": {"$ref": "neuralmail/resources/message.json"}}
  },
  "required": ["thread"]
}
```

### 3) search_inbox
Semantic search over an inbox.

Input schema:
```json
{
  "$id": "neuralmail/tools/search_inbox.input.json",
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "inbox_id": {"$ref": "neuralmail/types.json#/definitions/id"},
    "query": {"type": "string"},
    "top_k": {"type": "integer", "minimum": 1, "maximum": 50, "default": 10},
    "time_range": {
      "type": "object",
      "additionalProperties": false,
      "properties": {
        "start": {"$ref": "neuralmail/types.json#/definitions/timestamp"},
        "end": {"$ref": "neuralmail/types.json#/definitions/timestamp"}
      }
    }
  },
  "required": ["inbox_id", "query"]
}
```

Output schema:
```json
{
  "$id": "neuralmail/tools/search_inbox.output.json",
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "results": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "properties": {
          "message_id": {"$ref": "neuralmail/types.json#/definitions/id"},
          "thread_id": {"$ref": "neuralmail/types.json#/definitions/id"},
          "score": {"type": "number"},
          "snippet": {"type": "string"}
        },
        "required": ["message_id", "thread_id", "score"]
      }
    }
  },
  "required": ["results"]
}
```

### 4) triage_message
Classify intent, urgency, and sentiment.

Input schema:
```json
{
  "$id": "neuralmail/tools/triage_message.input.json",
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "message_id": {"$ref": "neuralmail/types.json#/definitions/id"},
    "taxonomy_id": {"$ref": "neuralmail/types.json#/definitions/id"}
  },
  "required": ["message_id", "taxonomy_id"]
}
```

Output schema:
```json
{
  "$id": "neuralmail/tools/triage_message.output.json",
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "intent": {"type": "string"},
    "urgency": {"type": "string", "enum": ["low", "medium", "high"]},
    "sentiment": {"type": "string", "enum": ["negative", "neutral", "positive"]},
    "confidence": {"$ref": "neuralmail/types.json#/definitions/confidence"},
    "suggested_route": {"type": "string"}
  },
  "required": ["intent", "urgency", "sentiment", "confidence"]
}
```

### 5) extract_to_schema
Extract structured data with validation hints.

Input schema:
```json
{
  "$id": "neuralmail/tools/extract_to_schema.input.json",
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "message_id": {"$ref": "neuralmail/types.json#/definitions/id"},
    "schema_id": {"$ref": "neuralmail/types.json#/definitions/id"}
  },
  "required": ["message_id", "schema_id"]
}
```

Output schema:
```json
{
  "$id": "neuralmail/tools/extract_to_schema.output.json",
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "data": {"type": "object"},
    "confidence": {"$ref": "neuralmail/types.json#/definitions/confidence"},
    "missing_fields": {"type": "array", "items": {"type": "string"}},
    "validation_errors": {"type": "array", "items": {"type": "string"}}
  },
  "required": ["data", "confidence"]
}
```

### 6) draft_reply_with_policy
Draft a reply constrained by a policy.

Input schema:
```json
{
  "$id": "neuralmail/tools/draft_reply_with_policy.input.json",
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "thread_id": {"$ref": "neuralmail/types.json#/definitions/id"},
    "policy_id": {"$ref": "neuralmail/types.json#/definitions/id"},
    "goal": {"type": "string"}
  },
  "required": ["thread_id", "policy_id", "goal"]
}
```

Output schema:
```json
{
  "$id": "neuralmail/tools/draft_reply_with_policy.output.json",
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "draft": {"type": "string"},
    "risk_flags": {"type": "array", "items": {"type": "string"}},
    "cited_message_ids": {"type": "array", "items": {"$ref": "neuralmail/types.json#/definitions/id"}},
    "needs_human_approval": {"type": "boolean"}
  },
  "required": ["draft"]
}
```

### 7) send_reply
Send a reply to a thread.

Input schema:
```json
{
  "$id": "neuralmail/tools/send_reply.input.json",
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "thread_id": {"$ref": "neuralmail/types.json#/definitions/id"},
    "body_or_draft_id": {"type": "string"},
    "idempotency_key": {"type": "string"},
    "html": {"type": "string"},
    "needs_human_approval": {"type": "boolean", "default": false},
    "attachments": {
      "type": "array",
      "maxItems": 10,
      "items": {"$ref": "neuralmail/types.json#/definitions/outbound_attachment"}
    }
  },
  "required": ["thread_id", "body_or_draft_id"]
}
```

Output schema:
```json
{
  "$id": "neuralmail/tools/send_reply.output.json",
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "message_id": {"$ref": "neuralmail/types.json#/definitions/id"},
    "status": {"type": "string", "enum": ["queued", "sent"]}
  },
  "required": ["message_id", "status"]
}
```

### 8) compose_email
Compose and send a new email (not a reply).

Input schema:
```json
{
  "$id": "neuralmail/tools/compose_email.input.json",
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "inbox_id": {"$ref": "neuralmail/types.json#/definitions/id"},
    "to": {"type": "string", "format": "email"},
    "subject": {"type": "string"},
    "from_name": {"type": "string", "description": "Optional display name for the sender; the sender address remains the selected inbox address"},
    "idempotency_key": {"type": "string"},
    "body": {"type": "string"},
    "html": {"type": "string"},
    "attachments": {
      "type": "array",
      "maxItems": 10,
      "items": {"$ref": "neuralmail/types.json#/definitions/outbound_attachment"}
    }
  },
  "required": ["inbox_id", "to", "subject"],
  "anyOf": [
    {"required": ["body"]},
    {"required": ["html"]}
  ]
}
```

Output schema:
```json
{
  "$id": "neuralmail/tools/compose_email.output.json",
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "thread_id": {"$ref": "neuralmail/types.json#/definitions/id"},
    "message_id": {"$ref": "neuralmail/types.json#/definitions/id"},
    "status": {"type": "string", "enum": ["queued", "sent"]},
    "smtp_error": {"type": "string"}
  },
  "required": ["thread_id", "message_id", "status"]
}
```

### 9) nerve_billing_subscribe (MCP 2026 only)

Create or replay the authenticated generation's paid-subscription workflow.
This tool is registered only when the runtime has a `BillingProvisioner` and
the authenticated principal is an active `m2m_org` token carrying
`nerve:billing.subscribe`. An onboarding token, legacy principal, incomplete
org binding, missing scope, or unconfigured provisioner cannot list or invoke
it.

Input schema:

```json
{
  "$id": "neuralmail/tools/nerve_billing_subscribe.input.json",
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "plan_code": {
      "type": "string",
      "minLength": 1,
      "maxLength": 64,
      "pattern": "^[a-z0-9][a-z0-9._-]{0,63}$"
    },
    "idempotency_key": {"type": "string", "minLength": 1, "maxLength": 128}
  },
  "required": ["plan_code", "idempotency_key"]
}
```

The object accepts exactly those two fields. Client, organization, onboarding,
generation, Stripe customer, payment method, mandate, payment-intent, client
secret, Checkout URL, and equivalent authority or payment overrides are
rejected. The control plane derives them from the authenticated principal and
the protected client registration. Duplicate JSON fields, surrounding/control
characters in the idempotency key, and noncanonical plan codes are also
rejected.

Successful output schema:

```json
{
  "$id": "neuralmail/tools/nerve_billing_subscribe.output.json",
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "resultType": {"type": "string", "const": "complete"},
    "state": {
      "type": "string",
      "enum": ["processing", "provider_unknown", "requires_action", "active"]
    },
    "plan_code": {"type": "string", "minLength": 1, "maxLength": 64},
    "compose_enabled": {"type": "boolean"},
    "retry_at": {"type": "string", "format": "date-time"}
  },
  "required": ["resultType", "state", "plan_code", "compose_enabled"]
}
```

`active` is returned only after authoritative provider readback and committed
paid evidence, and is the only state with `compose_enabled=true`.
`processing`, `provider_unknown`, and `requires_action` are complete results
with compose still denied. They never contain a hosted action URL, card input,
client secret, provider body, or payment/authority identifier. The agent may
repeat the same idempotency key to observe the same durable workflow; changing
the plan under that key conflicts. A `requires_action` result is fail closed:
an operator must repair the preauthorized off-session mandate before a clean
retry, rather than completing an SCA challenge through MCP.

The modern error result is closed to:

- `billing_invalid_request`
- `billing_idempotency_conflict`
- `billing_mandate_unavailable`
- `billing_plan_not_allowed`
- `billing_invalid_state`
- `billing_rate_limited`
- `billing_temporarily_unavailable`

Unknown provisioner errors and invalid provisioner results are sanitized to
`billing_temporarily_unavailable`; raw provider responses are never reflected.

The `attachments` input is available only when the org-scoped `attachments`
feature flag is enabled. When disabled, `tools/list` omits the property and a
producer that still sends it receives `attachment_feature_disabled`.
Decoded attachment content must be non-empty, at most 10 MiB per file and 10
MiB in total. Filenames are trimmed server-side, limited to 255 UTF-8 bytes,
and cannot contain path separators, NUL, or control characters. Base64 decoding
is strict. Validation errors use the stable codes
`attachment_count_exceeded`, `attachment_too_large`, `attachment_empty`,
`attachment_total_too_large`, `attachment_invalid_filename`,
`attachment_type_not_allowed`, and `attachment_invalid_encoding`.

## MCP 2026 delegated onboarding profile

An authenticated `m2m_onboarding` principal carrying only `nerve:onboarding`
sees the closed tool set `nerve_onboarding_start`,
`nerve_onboarding_status`, `nerve_onboarding_verify_domain`, and
`nerve_onboarding_close`. The delegated generation, client, and token identity
come from the authenticated envelope; tool arguments cannot override them.

For `custom_domain`, `nerve_onboarding_start` durably claims the canonical
domain before any provider call and returns an ordinary complete result. While
the state is `provisioning` or `dns_pending`, that result can include
`dns_records` with `type`, `name`, `value`, optional `ttl`/`priority`, `purpose`,
and provider-observed `status`; `dns_checks` reports `ownership`, `spf`, `dkim`,
`mx`, `provider_verified`, and `receiving_enabled`. The caller must publish the
records through a separate DNS-provider connector, then call
`nerve_onboarding_verify_domain` or poll status according to `retry_at`.

Nerve never requests, accepts, or stores registrar or DNS-provider credentials
through these tools. The agent must not place such credentials in any tool
argument. A caller-supplied verified flag is not accepted. The mailbox becomes
`active` and receives an `address` only after authoritative readback proves the
ownership challenge, all required SPF and DKIM records, receiving MX, aggregate
provider verification, and receiving enabled. An authoritative later loss
returns the generation to `dns_pending` and removes effective compose authority
while retaining the generation-owned inbox for recovery; an unknown transport
outcome preserves the last authoritative state and remains retryable.

## Error Shape
All tools should return errors in a consistent shape when possible.

```json
{
  "$id": "neuralmail/errors.json",
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "code": {"type": "string"},
    "message": {"type": "string"},
    "details": {"type": "object"}
  },
  "required": ["code", "message"]
}
```
