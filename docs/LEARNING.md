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
| H1 | Engine glued at a protocol to AM; eval never calls contact points | PR3 | ⬜ |
| H2 | Only firing and resolved leave the engine | PR2–PR3 | ⬜ |
| H3 | AM owns contact points, grouping + timers, pipeline, silences, notification state | PR4–PR6 | ⬜ |
| H4 | Contact point = dumb ingress adapter | PR5 | ⬜ |
| H5 | Group wait on birth only; interval after; empty+notified deletes; repeat multiple of interval; dedup gates every flush | PR4 | ⬜ |
| H6 | Inhibition ≠ grouping ≠ silence; silence drops notify, eval continues | PR6 | ⬜ |
| H7 | AM is incident-centric; resolved is first-class | PR4–PR5 | ⬜ |
| H8 `[AG]` | Steal AM policy, keep thin `PutAlerts` shell — **not implemented here** | 10 lines below after PR5 | ⬜ |

## PR log

- **PR0 (this commit):** scaffold — YAML config (durations, `repeat_interval` coercion), `-httpListenAddr` / `-config` / `-evalInterval` flags, `GET /healthz`, Dockerfile. No eval, no AM yet.
