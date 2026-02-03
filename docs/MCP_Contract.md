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
    "idempotency_key": {"type": "string"}
  },
  "required": ["thread_id", "body_or_draft_id", "idempotency_key"]
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
