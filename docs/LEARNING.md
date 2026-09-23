# LEARNING — marchialerts ↔ Grafana Alerting

Living document, updated per PR. [PLAN.md](../PLAN.md) is the source of truth;
this file restates the mapping **from the code that exists**, plus what we
deliberately cut.

## Mental map (PLAN §2)

| Concept | Grafana | marchialerts MVP |
|---|---|---|
| Eval input | datasource query + expressions | **in-memory samples** and/or `GET` Prometheus text / JSON scrape |
| Alert rule | Grafana-managed rule (interval, `for`, `keep_firing_for`, no-data) | YAML/JSON: selector + threshold + `for` |
| Alert instance | rule UID × label set | rule id × label set |
| Leaves engine | firing / resolved (not pending) | same — **acceptance test** |
| Fingerprint | **receiver-side** FNV-1a 64 over sorted `name 0xff value 0xff` of the alert labels (engine has its own FNV-1 CacheID; nflog uses xxhash — three hashes, PLAN §1) | one canonicalize + FNV-1a helper in `am/`; computed on ingest, never sent by the engine (same canonicalize lesson as marchimetrics SeriesID) |
| Notification policy | matcher tree + `continue` | **one default route** first; matcher tree in a later PR |
| Aggregation group | `group_by` + `aggrGroup` | map `groupKey → group`; wait / interval / repeat / delete; `group_by: [...]` = GroupByAll (aggregation off) |
| Notify pipeline | stage chain: settle → inhibit → active/mute time → silence → wait → dedup → retry → set-notifies | thin `Stage` interface: silence gate → dedup → retry → contact point |
| nflog / dedup | “should this flush actually send?” — fire / fire-subset / resolve / repeat, keyed by (receiver, groupKey) | in-memory entry per (receiver, groupKey); port of `needsUpdate` |
| Retry | exponential backoff per integration until ctx done | bounded backoff wrapper around the webhook adapter |
| Contact point | named receiver, N integrations | `stdout` + `webhook`; stretch `file` or `kafka` |
| Incident vs message | AM holds a **living fingerprint set**; payload is a batch | group notify sends `alerts[]`, not one engine tick |
| Persistence | AM: memory + snapshot file (+gossip); Grafana engine: SQL `alert_instance` | **all in-memory** — delivery state is ephemeral by design (PLAN §1) |

## Non-goals

- Alert Gateway product, Kafka mesh, JSM/Slack production routing
- Grafana UI, HA, inhibition UI, full policy tree on day one
- PromQL engine, recording rules, Mimir ruler
- Wire-format compatibility with Grafana AM
- Any database — state is in-memory; a JSON snapshot file is a PR7 stretch at most
- Full PromQL / Grafana expressions / multi-datasource eval
- HA nflog gossip, peer wait, exactly-once across replicas
- Mute/active time-interval calendar UI, notification templates
- Every Grafana receiver (Slack, email, JSM, …)

## Hypotheses status (PLAN §6)

| # | Claim | Shown by | Status |
|---|---|---|---|
| H1 | Engine glued at a protocol to AM; eval never calls contact points | PR3 | ✅ `engine.Sender` → `am.Receiver.PutAlerts` is the only egress |
| H2 | Only firing and resolved leave the engine | PR2–PR3 | ✅ `ShouldNotify` + pending-tick-sends-nothing test |
| H3 | AM owns contact points, grouping + timers, pipeline, silences, notification state | PR4–PR6 | 🔶 groups + timers + nflog under `am/` (PR4); pipeline stages + silences in PR5/PR6 |
| H4 | Contact point = dumb ingress adapter | PR5 | ✅ adapters get a fully-formed group payload; grouping tests pass with a recorder |
| H5 | Group wait on birth only; interval after; empty+notified deletes; repeat multiple of interval; dedup gates every flush | PR4 | ✅ fake-clock tests 1–10 |
| H6 | Inhibition ≠ grouping ≠ silence; silence drops notify, eval continues | PR6 | ⬜ |
| H7 | AM is incident-centric; resolved is first-class | PR4–PR5 | ✅ one POST carries firing+resolved as a group mutation |
| H8 `[AG]` | Steal AM policy, keep thin `PutAlerts` shell — **not implemented here** | 10 lines below after PR5 | ⬜ |

## PR log

- **PR0:** scaffold — YAML config (durations, `repeat_interval` coercion), `-httpListenAddr` / `-config` / `-evalInterval` flags, `GET /healthz`, Dockerfile. No eval, no AM yet.
- **PR1:** metrics in + eval tick — `metrics.Store` (latest sample per series, canonical series key), `POST /api/v1/import` (`{metric, labels, t, v}`, single or array), `engine.Evaluator` ticks `eval_interval` and logs `firing`/`ok` per (rule, series). Learned: a rule is a spec; a problem is per label set — one rule over two series yields two results. Stateless for now: no `for`, no instances, no AM.
- **PR2:** instance state machine + `for` — `engine.StateManager` keyed by rule × canonical labels; `normal → pending (for not elapsed) → firing → resolved → normal`; refire starts a fresh firing period. `Transition.ShouldNotify()` encodes **H2**: only `firing`/`resolved` may ever leave the engine — pending transitions log `notify=false`. All time logic under an injected clock; tests advance it explicitly, nobody sleeps in CI.
- **PR3:** protocol boundary + fingerprint — `engine.Sender` is the **only** egress (H1): notifiable transitions become `am.PostableAlert`s (identity labels = instance + rule labels + `alertname`; empty dropped) and go to `am.Receiver.PutAlerts`; pending ticks put **zero** alerts. Fingerprint is **receiver-side** FNV-1a over sorted `name 0xff value 0xff` — annotations never participate, a label change is a new alert, resolve reuses identical labels so the fingerprint matches, and firing alerts carry a rolling `endsAt` (≥ resendDelay) so the AM won't auto-resolve between ticks. `am.Stub` records the wire for now; real groups land in PR4.
- **PR4:** aggregation groups + three timers + dedup — `am.Dispatcher` (one default route) buckets alerts by `group_by` (`[...]` = GroupByAll) into `aggrGroup`s. **Birth** starts group wait; after the first send, flushes run at group interval; every flush is gated by the in-memory **nflog** (port of AM `DedupStage.needsUpdate`: fire / fire-subset / resolve / resolve-subset / repeat). Resolved alerts leave the group after a **successful** notify (retained ≤ 3×interval on failure); an empty group after a notified resolve is **deleted**, so the next fire pays group wait again. All timers run on an injected clock — the 10 PLAN tests (A+B coalesce, C batches, repeat reminder, rebirth, dedup gate, silent resolve, GroupByAll, failure retention, late-alert immediate flush) advance a fake clock, nobody sleeps in CI. **Learned:** timers decide *when* to flush; nflog decides *whether* the flush sends.
- **PR5:** contact points + retry — `notify.ContactPoint` is a **dumb adapter** (H4): the pipeline hands it a fully-formed group `Payload` (Grafana-ish JSON: `status`, `groupKey`, `alerts[]` with per-alert status/labels/fingerprint/startsAt/endsAt). `Stdout` logs one JSON line; `Webhook` POSTs (5xx/transport = retryable, 4xx = permanent); `Retry` wraps any adapter with bounded exponential backoff (sleep is injectable — tests never wait); `Fanout` delivers one group to all adapters (AM FanoutStage). Smoke: one POST carried A+B firing, a later POST carried both resolved — a group mutation, not a message id (H7).
