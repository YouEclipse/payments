# ADR 0002: Pin execution revisions and separate provider namespaces

Status: accepted for the tokenized automatic-capture SALE slice.

## Context

A PSP name is an adapter kind, not an executable payment destination. The same
provider can expose several merchant accounts, connectors and regional
endpoints. An operation that became `unknown` must not move to a different
destination when configuration changes.

Migration also introduces recovery aliases. Treating an alias as a new provider
reference or webhook-event namespace would allow two connections to claim the
same external transaction without detecting the collision. Conversely, sharing
a collision namespace must not grant one connection's webhook secret authority
over another account.

## Decision

| Field | Meaning | Owner and rule |
| --- | --- | --- |
| `provider` | Adapter kind | Gateway selects the protocol implementation, never a route. |
| `candidate_id` / `provider_connection_id` | Exact executable connection | Router selects it; Payment and Gateway persist it unchanged. |
| `connection_revision` / `provider_connection_revision` | Immutable execution configuration revision | Router returns it; Gateway binds it to a durable configuration fingerprint. |
| `provider_namespace_id` | Conservative collision namespace for provider-supplied references and event IDs | Connections and recovery aliases of the same identifier scope share it. It is not an authentication domain. |

Payment persists the connection ID, revision, namespace and provider on both
Attempt and Provider Operation. A composite foreign key prevents an Operation
from drifting away from its Attempt. Every execute, query and normalized outcome
must match this durable tuple. Capture, Void and Refund, when implemented, must
inherit it from the original approved operation; they must not ask Router again.
The versioned Router decision's `executable_until` is checked immediately before
the first Provider I/O. It is not a reason to reroute an expired operation: a
never-dispatched operation fails closed, while an already-dispatched operation
continues recovery on this frozen tuple without reevaluating today's Risk or
Router configuration.

Gateway maintains an append-only connection-revision registry. Reusing the same
connection ID and revision with a different execution fingerprint fails startup
or readiness. The current simulator fingerprint covers its normalized endpoint,
provider kind, namespace and a digest of its webhook secret, never the secret
value itself. A real adapter must include the actual merchant
account, region and credential-scope reference in this binding before it is
enabled; a human-readable account label alone is not proof of an execution scope.
Secret values must not be copied into Router, Payment, events or this registry.

The current configuration resolves one revision per connection ID. Changing an
endpoint, account scope or webhook secret therefore requires a new connection ID
and revision, while retaining the old connection and authentication material for
outstanding operations and late events. In-place secret rotation under the same
ID/revision is deliberately rejected; overlapping secret rotation needs a future
explicit credential-version contract. Old revisions remain resolvable for their
outstanding operations. A request using a
different revision or namespace cannot reuse an existing operation ID or
provider idempotency key. Gateway's request fingerprint includes these identities
as well as the immutable payment payload.

Provider references are checked and locked by `(provider_namespace_id,
provider_reference)`, including across aliases. A collision places both known
Payment owners in `review_required`, preserves already-confirmed funds facts and
stops automatic recovery. Webhook event deduplication uses
`(provider_namespace_id, event_id)` and rejects a changed correlation or payload.

Webhook authentication remains connection-specific. A path can authenticate its
own exact connection, or an explicitly configured recovery alias target whose
endpoint and webhook secret are compatible. Merely sharing a namespace does not
authorize cross-connection webhook updates. Normalized outcomes always carry the
matched operation's durable tuple, not a tuple reconstructed from the current
path or active routing configuration.

## Upgrade and recovery aliases

Payment migration `003` and Gateway migration `0002` are coordinated contract
migrations, not rolling-compatible expand/contract migrations. Stop new Confirm
traffic, drain or explicitly take manual ownership of nonterminal operations,
stop all old binaries and back up the databases before applying them. Do not run
old and new writers together.

Legacy rows are adopted as `connection_id = provider`, `revision = legacy_v1`,
`namespace = provider`. The operator must provide the original execution mapping
for this one-time adoption. Gateway cannot reconstruct historical credentials or
endpoints from an old provider name. Its first registry binding therefore relies
on this explicit upgrade gate; later rebindings are rejected by the registry.

The local `simulator_a` alias is recovery-only and points explicitly to
`simulator_a_us_primary`. It may recover an already-persisted operation but cannot
create a new one. Router only selects the canonical connections. Keep aliases,
old revisions and their webhook authentication material until nonterminal
operations, pending outbox records and the agreed late-event retention window
have drained. Removing a mapping needed by a nonterminal operation makes
Gateway readiness fail closed.

Recovery queries also bind the operation/attempt tuple, Provider idempotency key
and versioned request hash. A normal 404 or retryable hint does not prove that a
funds side effect is absent. Same-key resubmission is allowed only when Payment's
durable state proves the operation was never dispatched, or the authenticated
Gateway supplies a fresh structured absence proof bound to every identity field
and the original payload hash. Current proof minting is local/test-only; a real
adapter needs an explicit Provider query/idempotency contract before enabling it.

## Consequences

- A configuration rollout cannot silently turn a same-operation retry into a
  payment on another account or region.
- Alias/canonical collisions are visible without broadening webhook trust.
- Recovery configuration has an explicit lifecycle and cannot be deleted merely
  because Router stopped selecting it for new payments.
- The current simulator proves these identity boundaries, not live PSP account
  semantics or portable payment credentials. Those require a real adapter and
  sandbox acceptance before any live-payment claim.
