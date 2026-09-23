# marchialerts MVP plan

Mirror of the marchimetrics approach: read Grafana Alerting (`grafana/grafana` `pkg/services/ngalert` + `grafana/alerting`), keep the two-product DNA (engine vs Alertmanager vs contact point), cut everything that is not needed to **evaluate → policy/group → notify**.

Primary references:

- Engine: `grafana/grafana/pkg/services/ngalert/` (`schedule`, `eval`, `state`, `sender`)
- AM + receivers: `grafana/alerting` (`notify`, `dispatch` / `aggrGroup`, `silence`, `inhibit`, `nflog`, `receivers/*`)
- Upstream timer analogue: `prometheus/alertmanager/dispatch/dispatch.go`

This is a **learning MVP in this directory**, not Alert Gateway, not a Grafana fork, and not a production notifier for Airwallex. Optional `[AG]` notes are mapping exercises after the binary works.

Cluster / HA Grafana, Mimir ruler, and Grafana UI are **out of scope** (single process like marchimetrics).

---

## 1. What Grafana Alerting is doing (relevant slice)

### Roles (skip HA / multi-AM for MVP)

| Component | Role | MVP |
|---|---|---|
| Alert Engine | rules, eval tick, instance state (`for`, no-data/error) | **Yes** |
| Alertmanager protocol | `PutAlerts` / postable alerts; Grafana cannot skip this as the engine’s egress | **Yes — in-process API** |
| Built-in AM | route, group + 3 timers, notify pipeline (gates → dedup → retry), silence, inhibit, nflog | **Yes — thin subset** |
| Contact point / receiver | adapter: render a **message** from an **incident set** | **Yes — webhook + stdout** |
| Grafana UI / provisioning CRUD | clickops | Later / skip |
| External Prometheus AM | interchangeable AM | Out |

### Data path (simplified)

```
samples (fake store or HTTP scrape)
  → rule eval tick
  → alert instance = (rule × label set) state machine
       pending / firing / resolved  (+ optional recovering)
  → only firing + resolved leave the engine
  → PutAlerts (fingerprint, labels, startsAt/endsAt)
  → route → aggregation group (group_by + 3 timers)
  → notify pipeline (per group flush):
       silence / inhibit gates (eval still runs)
       → dedup vs nflog (fire subset / resolve / repeat?)
       → retry with backoff
  → contact point (webhook JSON / stdout)
```

### The three AM timers (must be exhibited, not only documented)

| Timer | When it applies |
|---|---|
| **group wait** | group **birth** only — first send of a new aggregation group |
| **group interval** | after first send — batch updates (new instances, resolves) |
| **repeat interval** | same firing set still active; Grafana coerces this to a **multiple** of group interval |

Empty group after a **notified resolve** is **deleted**. Next firing is a **new birth** (group wait again).

### Dedup (nflog) decides *whether* a flush sends

Timers decide **when** a group flushes; the nflog dedup stage decides **whether** that flush actually notifies (port of AM `DedupStage.needsUpdate`):

| Situation at flush | Send? |
|---|---|
| No nflog entry, firing alerts present | **Yes** (`fire`) |
| No nflog entry, only resolved alerts | **No** — receiver never knew about them |
| Current firing ⊄ last notified firing | **Yes** (`fire subset`) |
| Firing now empty, last entry had firing | **Yes** (`resolve`) — clears the log |
| New resolved subset | Only if `send_resolved` (`resolve subset`) |
| Nothing changed | Only if `repeat_interval` elapsed (`repeat`) |

Dedup only runs **at flush cadence** (group interval) — this is why repeat must be coerced to a multiple of group interval. After a **successful** notify, resolved alerts are deleted from the group; on failure they are kept up to `3 * group_interval` (bounds memory, mirrors AM `DeleteIfStale`).

### Fingerprint: three hashes, three jobs

The stack uses **different hashes for different identities** — do not conflate them:

| Hash | Computed by | Algorithm | Over what | Job |
|---|---|---|---|---|
| Engine instance `CacheID` | Grafana `ngalert/state` | **FNV-1 64** (SDK `data.Labels.Fingerprint`) | expanded labels = instance labels + rule extras (`alertname`, `__alert_rule_uid__`, `__alert_rule_namespace_uid__`, `grafana_folder`) | find the state in the cache; a separate `ResultFingerprint` (raw eval labels) detects “same series, labels changed” |
| AM alert fingerprint | **AM, on ingest** | **FNV-1a 64** (`prometheus/common/model`: sorted `name 0xff value 0xff`) | the postable alert’s labels **as received** | alert identity: store key, Marker (silence/inhibit), API `fingerprint` hex, group membership |
| nflog dedup hash | AM `notify.hashAlert` | **xxhash64** | sorted `name 0xff value 0xff` | firing/resolved sets inside nflog entries |

Handling rules that the MVP must exhibit:

1. **Labels are the identity.** `PostableAlert` has no fingerprint field — the receiver derives it. Annotations never participate: changing an annotation updates the same alert in place.
2. **Label change = new alert.** Same series with a different label set → different fingerprint → a *new* incident; the old one expires via `EndsAt`.
3. **Same fingerprint merges** (AM `types.Alert.Merge`): earliest `StartsAt` wins, latest `EndsAt` wins, annotations from the latest copy.
4. **Resolve must reuse the identical label set** — Grafana rebuilds the resolved alert with the same labels (including the NoData/Error rewrite) so the fingerprint matches and expires the previous alert.
5. **NoData/Error rewrite `alertname`** (`DatasourceNoData`, original name backed up to `rulename`) — deliberately a *different* fingerprint = a separate incident.
6. **Empty label names/values are dropped** before sending (AM rejects invalid label sets).
7. **`EndsAt` on a firing alert is a rolling deadline**: each tick sets `EndsAt = evaluatedAt + max(resendDelay, interval)` (Grafana `Maintain`, ≥ 2 eval cycles, synced with Prometheus) so the AM does not auto-resolve between ticks; AM treats `now > EndsAt` as resolved.

### Persistence: in-memory by design (no DB)

Neither side of the real stack puts this state in a database by default:

| State | Prometheus | Grafana | If lost |
|---|---|---|---|
| Engine instance state (pending/firing, `for` progress) | memory only (`ALERTS_FOR_STATE` series as a later restore hack) | memory cache + `alert_instance` SQL table (for UI + multi-instance) | `for` restarts, pending resets — Prometheus accepts this |
| AM alerts store / groups / marker | memory only (marker GC'd periodically) | same | alerts re-sent after restart |
| nflog | memory + periodic **snapshot file** (+ gossip in HA) | same | **duplicate** notifications after restart |
| silences | memory + snapshot file (+ gossip) | same | noise |

Delivery state is **ephemeral by design**: the worst case of losing it is a duplicate notification (at-least-once), never a lost one. No transactions, no relational queries — a DB buys nothing. The right question is “can I accept the consequence of losing this state”, not “should I persist it”.

MVP: **all in-memory**. Self-healing comes from the engine re-sending firing alerts with a rolling `endsAt` every tick — if one side loses state, the next tick converges it. Optional PR7 stretch: JSON **snapshot file** (learn AM's snapshot, not Grafana's SQL) for instances + nflog + silences.

### Deliberately skip (same spirit as marchimetrics)

- Full PromQL / Grafana expressions / multi-datasource eval
- HA nflog gossip, peer wait, exactly-once across replicas (single-process: `GossipSettle` and the nflog timestamp guard are no-ops — note this in LEARNING.md)
- Mute/active time-interval calendar UI (the pipeline gate itself is an optional PR7 item), notification templates (Go `template` beyond a fixed JSON body)
- Every Grafana receiver (Slack, email, JSM, …)
- Grafana AM config protobuf / UI resource model
- Exact Grafana fingerprint / nflog on-disk compatibility
- Any database — state is in-memory (see Persistence above); a JSON snapshot file is a PR7 stretch at most

**Keep:** instance state + `for`, protocol boundary (pending never notifies), fingerprint, grouping + three timers + group teardown, **dedup semantics over nflog**, **retry with backoff**, silences, at least two contact-point adapters, tests that encode the Grafana hypotheses.

---

## 2. marchialerts vs Grafana (mental map)

| Concept | Grafana | marchialerts MVP |
|---|---|---|
| Eval input | datasource query + expressions | **in-memory samples** and/or `GET` Prometheus text / JSON scrape |
| Alert rule | Grafana-managed rule (interval, `for`, `keep_firing_for`, no-data) | YAML/JSON: selector + threshold + `for` |
| Alert instance | rule UID × label set | rule id × label set |
| Leaves engine | firing / resolved (not pending) | same — **acceptance test** |
| Fingerprint | **receiver-side** FNV-1a 64 over sorted `name 0xff value 0xff` of the alert labels (engine has its own FNV-1 CacheID; nflog uses xxhash — three hashes, §1) | one canonicalize + FNV-1a helper in `am/`; computed on ingest, never sent by the engine (same canonicalize lesson as marchimetrics SeriesID) |
| Notification policy | matcher tree + `continue` | **one default route** first; matcher tree in a later PR |
| Aggregation group | `group_by` + `aggrGroup` | map `groupKey → group`; wait / interval / repeat / delete; `group_by: [...]` = GroupByAll (aggregation off) |
| Notify pipeline | stage chain: settle → inhibit → active/mute time → silence → wait → dedup → retry → set-notifies | thin `Stage` interface: silence gate → dedup → retry → contact point |
| nflog / dedup | “should this flush actually send?” — fire / fire-subset / resolve / repeat, keyed by (receiver, groupKey) | in-memory entry per (receiver, groupKey); port of `needsUpdate` |
| Retry | exponential backoff per integration until ctx done | bounded backoff wrapper around the webhook adapter |
| Contact point | named receiver, N integrations | `stdout` + `webhook`; stretch `file` or `kafka` |
| Incident vs message | AM holds a **living fingerprint set**; payload is a batch | group notify sends `alerts[]`, not one engine tick |
| Persistence | AM: memory + snapshot file (+gossip); Grafana engine: SQL `alert_instance` | **all in-memory** — delivery state is ephemeral by design (§1) |

Recommendation: **reuse marchimetrics engineering habits** (Go module, one binary, HTTP, docker, tests per PR, `docs/LEARNING.md`), but **change the data model to instances + groups**, not LSM parts.

---

## 3. MVP product goals

**In:**

1. One binary: eval loop + in-process AM + contact points.
2. Rules: threshold on a metric+labels selector (`cpu_idle{job="api"} < 10`, or equivalent JSON).
3. Instance states: `pending` → `firing` after `for`; `resolved` when the condition clears (optional `keep_firing_for` later).
4. Engine → AM via an explicit `PutAlerts` (or identical internal call). Pending instances **must not** appear there.
5. Grouping: `group_by` (e.g. `alertname` + `team`; `group_by: [...]` = GroupByAll) with configurable group wait / group interval / repeat interval (repeat coerced to a multiple of group interval).
6. Dedup: a flush only sends when nflog says so (first fire / fire-subset / resolve / repeat); resolved alerts leave the group after a **successful** notify, retained ≤ `3 * group_interval` on failure.
7. Silences: matcher + time window; webhook stops; instances keep evaluating.
8. Contact points: **stdout/log** + **HTTP webhook** (Grafana-ish JSON: `status`, `labels`, `fingerprint`, `startsAt`/`endsAt`, `alerts[]`), wrapped in a **retry-with-backoff** stage.
9. Inspectable HTTP: healthz, current instances (status filters like `?silenced=true`), active groups, last notifications.

**Out:**

- Alert Gateway product, Kafka mesh, JSM/Slack production routing
- Grafana UI, HA, inhibition UI, full policy tree on day one
- PromQL engine, recording rules, Mimir ruler
- Wire-format compatibility with Grafana AM

**Pass bar:** a rule with 2+ series; pending never hits the webhook; first notify waits group wait and can include A+B; later C waits group interval; an unchanged firing set does **not** re-notify until repeat; resolve deletes the group so the next fire pays group wait again; silence mutes notify but not eval. That **is** the Grafana study.

---

## 4. Proposed runtime layout (MVP)

Single process, no cluster:

```
marchialerts/
  cmd/marchialerts/          # flags, HTTP, process lifecycle
  engine/
    rule.go                  # spec: selector, op, threshold, for, interval
    eval.go                  # tick → vector of (labels, value)
    state.go                 # instance map + pending/firing/resolved
    sender.go                # statesToSend → PutAlerts
  metrics/                   # fake append API and/or scrape
  am/
    alert.go                 # postable alert + receiver-side fingerprint (FNV-1a over canonical labels) + merge
    route.go                 # default route; later matcher tree
    group.go                 # aggrGroup: birth wait, interval, repeat, teardown
    pipeline.go              # Stage interface + chain: gates → dedup → retry → notify
    silence.go
    inhibit.go               # optional PR
    timeinterval.go          # optional PR: mute/active intervals
    nflog.go                 # dedup state per (receiver, groupKey): firing/resolved sets + timestamp
  notify/
    contact.go               # ContactPoint interface
    retry.go                 # backoff wrapper around a ContactPoint
    stdout.go
    webhook.go
    file.go / kafka.go       # stretch
  testdata/                  # golden webhook payloads, timer traces
  docs/LEARNING.md           # Grafana mapping + non-goals (create with PR0)
  PLAN.md
```

v0 simplification (fastest running loop): **no AM**. Eval tick writes firing lines to stdout. Upgrade to `PutAlerts` + groups when the state machine is tested.

Config (YAML is enough):

```
listen: :9094
eval_interval: 2s
group_wait: 3s
group_interval: 6s
repeat_interval: 12s          # must be multiple of group_interval; coerce if not
group_by: [alertname, team]
contact_points:
  - name: logger
    stdout: {}
  - name: hook
    webhook:
      url: http://127.0.0.1:9999/alerts
rules:
  - alert: HighCPU
    expr: { metric: cpu_usage, labels: {job: api}, op: ">", threshold: 0.8 }
    for: 4s
    labels: { team: o11y, severity: warning }
```

---

## 5. Implementation slices (PRs)

Theory is sequenced **as these PRs**. Do not add a “read Grafana docs for 2 hours” session; clone/source-read **the files needed for the current PR**.

### PR0 — Scaffold

- `go mod`, `cmd/marchialerts`, `Dockerfile`, `docker-compose.yml` (optional)
- Flags: `-httpListenAddr`, `-config`, eval interval
- `GET /healthz`
- `docs/LEARNING.md`: Grafana mapping table + non-goals (copy §2)

### PR1 — Metrics in + eval tick (smallest running loop)

- In-memory sample store: `POST /api/v1/import` JSON `{metric, labels, t, v}` (marchimetrics-shaped) **or** a test clock that injects values
- Load one threshold rule
- Loop: every eval interval, select series, compare threshold, log `firing` / `ok`
- Tests: two series, only the bad one logs

**Learned:** a “rule” is a spec; a “problem” is per label set (instance), not per rule.

### PR2 — Instance state machine + `for`

- Instance key = rule id × canonical labels
- States: `normal` → `pending` (condition true, `for` not elapsed) → `firing` → `resolved`/`normal`
- Deterministic time: inject a `now` clock in tests (do not sleep 30s in CI)
- Tests: condition true for `< for` stays pending; after `for`, firing; clear → resolved

**Acceptance (H2):** pending never queued for notify.

### PR3 — Protocol boundary `PutAlerts` + fingerprint

- `engine/sender.go`: only `firing` and `resolved` (not pending) call AM
- Alert payload: labels, annotations, startsAt, endsAt — **no fingerprint field**; labels are the identity
- Fingerprint lives **in the AM, computed on ingest** (`am/alert.go`): canonicalize = sort by name, feed `name 0xff value 0xff` into **FNV-1a 64**, render as `%016x`
- Drop empty label names/values before hashing/sending (AM rejects invalid label sets)
- **Merge:** re-posting the same fingerprint updates in place — earliest `startsAt` wins, latest `endsAt` wins, annotations from the latest copy
- **Resolve** re-sends the **identical label set** with `endsAt` set — same fingerprint expires the previous alert
- Firing alerts carry a **rolling `endsAt`** = `now + max(resendDelay, eval_interval)`, refreshed every tick (Grafana `Maintain`); AM treats `now > endsAt` as resolved
- In-memory AM stub: record received alerts for tests
- Tests: pending tick → 0 PutAlerts; firing → 1; resolved → 1 with endsAt
- Fingerprint tests: label-order independent; annotation change → **same** fingerprint; label change → **new** fingerprint (old alert expires, not updated); resolve reuses labels → same fingerprint; empty-valued label dropped before hash

**Acceptance (H1):** there is one egress from the engine; contact points are not called from `eval.go`.

### PR4 — Aggregation groups + three timers + dedup (H5)

- `group_by` → `groupKey`; `aggrGroup` with: create time, next flush, alerts map
- `group_by: [...]` (GroupByAll) = group by the full label set — aggregation off, one group per instance
- **Birth:** first alert in an empty group starts **group wait**; flush sends the whole set
- After first send, next flush is **group interval**
- **Dedup (nflog):** at each flush decide *whether* to send — port AM `DedupStage.needsUpdate` (see §1 table): first fire / fire-subset / resolve / resolve-subset (`send_resolved`) / repeat
- **Repeat** is only evaluated at flush cadence → coerce to a multiple of group interval (like Grafana)
- Resolved alerts: deleted from the group after a **successful** notify; on failure kept up to `3 * group_interval` (bounds memory, mirrors AM `DeleteIfStale`)
- Resolve-all + notified → **delete group**; next fire is a new birth (group wait again)
- Tests with fake clock (the important ones):

  1. A at t=0, no notify until group wait
  2. B before wait elapses → **one** first notify containing A+B
  3. After first send, C before group interval → no immediate send; next send has A+B+C
  4. Steady past repeat → reminder, same firing set
  5. Resolve all, interval notifies resolve, group gone; fire A → group wait again
  6. `repeat=5s`, `interval=3s` → coerced to `6s`
  7. Flush with unchanged firing set before repeat → **no send** (dedup gates, not just timers)
  8. Resolve without a prior notified fire → no `resolve` notification (receiver never knew)
  9. `group_by: [...]` → A and B with different labels land in **different** groups
  10. Notify fails → resolved alert retained; later success → deleted (no re-notify of that resolve)

**Learned:** AM is **incident-centric** (living fingerprint set), not “one eval tick = one message”. Timers decide *when* to flush; nflog dedup decides *whether* the flush sends.

### PR5 — Contact points + retry (H4)

- Interface: `Notify(ctx, groupKey, alerts)`
- Notify pipeline as composable stages (thin port of AM `notify.Stage`): `silence gate → dedup → retry → contact point`; stages pass `groupKey` / firing+resolved sets / `now` along (ctx or a struct)
- `stdout`: log JSON
- `webhook`: POST Grafana-ish body (`alerts[]` with status/labels/fingerprint/startsAt/endsAt)
- **Retry:** backoff (exponential or fixed-interval) with max attempts / deadline around the webhook; success writes nflog, exhaustion counts a failure and leaves the group to retry at next flush
- Tests: httptest server; payload is a **group**, not a single instance (unless `group_by` is unique per instance)
- Retry tests: 500 → retried → 200 → exactly one nflog entry; permanent 400 (non-retryable) → give up, no nflog write
- Optional same contact point with two integrations (stdout + webhook) — one group, two adapters (fan-out)

### PR6 — Silences (H6)

- Silence: matchers + starts/ends
- Applied **after** grouping, **before** contact point (a pipeline gate); engine still ticks
- Tests: firing instance stays firing; webhook silent; expire silence → notify resumes
- `GET /api/v2/alerts?silenced=true|false` style filter: silenced alerts still exist with status `suppressed` (proves silence ≠ deletion)
- Contrast test: “pause rule” (disable rule) **stops eval** — different from silence

### PR7 (optional) — Inhibition + time intervals + no-data + Kafka/file

- Inhibition: source/target matchers; drop **notifications** of target while source fires; instance still exists
- Mute/active **time intervals** on the route (calendar gates in the pipeline — distinct from matcher silences)
- No-data: synthetic instance labels (Grafana uses extra labels like `DatasourceNoData`) — only if it teaches H2 without a real datasource
- `keep_firing_for` / recovering
- JSON **snapshot file** for instances + nflog + silences (learn AM's snapshot model, not Grafana's SQL); startup restores, fake-clock tests stay pure
- File or Kafka contact point: **adapter only** — no grouping inside the producer (`[AG]` step-1: delivery pipe)

Stop before building Alert Gateway policy ownership (Grafana H8 step-2). If you want that, it is a **different repo**.

---

## 6. Grafana hypotheses = implementation acceptance tests

Encode these as tests (and as `docs/LEARNING.md` claims). They are **not** a reading checklist.

| # | Claim | How the MVP must exhibit it |
|---|---|---|
| H1 | Alerting = engine **glued at a protocol** to AM. You do not call Slack from the evaluator. | `eval` / `state` never import `notify/webhook`. Only `sender` → `PutAlerts`. |
| H2 | Engine owns rules, tick, `for`, instances. **Only firing and resolved leave.** | PR2–PR3 tests; webhook fixtures contain no `pending`. |
| H3 | AM owns contact points, grouping + timers, the notify pipeline (gates → dedup → retry), silences, active notification state. | Groups + pipeline + silences + `/am/groups` live under `am/`, not `engine/`. |
| H4 | Contact point = ingress adapter to a channel. Kafka/file would sit here, not in the engine. | PR5 (+ PR7 stretch) — grouping tests still pass if the adapter is a no-op recorder. |
| H5 | Group wait on **birth** only; then group interval; empty+notified **deletes**; repeat **multiple** of interval; nflog dedup gates every flush. | PR4 fake-clock tests 1–10. |
| H6 | Inhibition ≠ grouping ≠ silence. Silence/inhibit drop **notify**; eval continues. | PR6; PR7 inhibit. Pause-rule test proves the opposite. |
| H7 | AM is **incident-centric** (fingerprint set). Resolved is first-class. Payload = group mutation, not a chat message id. | Webhook: one POST can contain firing+resolved; repeat reminder is the same incident set. |
| H8 `[AG]` | End state people want: steal AM policy, keep a thin `PutAlerts` shell. Land in two steps: (1) Kafka contact point (2) take policy. | **Not implemented here.** After PR5, write 10 lines in LEARNING.md. Do not start a Gateway. |

**Falsifiers (if a test would fail, the design is wrong):**

- Pending instances in webhook JSON
- Group wait firing again without group deletion
- Notify from `eval.go` with no AM group
- Silence stopping evaluation
- A flush sending when the firing set is unchanged and repeat has not elapsed (dedup bypass)
- A resolve re-notified after its resolve notification already succeeded
- An annotation-only change producing a **new** fingerprint (annotations are not identity)
- A resolve sent with different labels than the fire (fingerprint mismatch → the firing alert never expires)

---

## 7. Grafana source study list (read while building the matching PR)

Do **not** vendor Grafana. Reimplement a thin subset.

| Topic | When | Path |
|---|---|---|
| Package map | PR0 | `grafana/grafana/pkg/services/ngalert/README.md` |
| Tick loop | PR1 | `pkg/services/ngalert/schedule/` |
| Eval results | PR1 | `pkg/services/ngalert/eval/` |
| Instance state / `send()` | PR2–PR3 | `pkg/services/ngalert/state/manager.go` |
| Protocol entry | PR3 | `pkg/services/ngalert/sender/router.go` (`PutAlerts`) |
| State → PostableAlert | PR3 | `pkg/services/ngalert/state/compat.go` (`StateToPostableAlert`: label dropping, resolve reuses labels, NoData/Error `alertname` rewrite) |
| Fingerprint algorithms | PR3 | `prometheus/common/model/signature.go` + `fnv.go` (FNV-1a); contrast `grafana-plugin-sdk-go/data/labels.go` (FNV-1) and AM `notify.hashAlert` (xxhash) |
| Alert merge / expiry | PR3 | `prometheus/alertmanager/types/types.go` (`Merge`, `ResolvedAt`) |
| AM wiring | PR4 | `grafana/alerting/notify/grafana_alertmanager.go` |
| Group timers / teardown | PR4 | Grafana fork of AM `dispatch` / `aggrGroup`; Prometheus `dispatch/dispatch.go` (note `flush`: `DeleteIfNotModified` on success, `DeleteIfStale` = 3 × group_interval on failure) |
| Dedup + retry + pipeline stages | PR4–PR5 | `prometheus/alertmanager/notify/notify.go` (`DedupStage.needsUpdate`, `RetryStage`, `MultiStage` / `FanoutStage`) |
| Route tree / `continue` / GroupByAll | PR4+ | `prometheus/alertmanager/dispatch/route.go` |
| nflog | PR4–PR5 | `grafana/alerting/nflog/` |
| Silences | PR6 | `grafana/alerting/silence/` |
| Inhibit | PR7 | `grafana/alerting/inhibit/` |
| Webhook adapter | PR5 | `grafana/alerting/receivers/webhook/` |

Docs (skim the page that matches the PR, then write the test):

- [Alert rule evaluation](https://grafana.com/docs/grafana/latest/alerting/fundamentals/alert-rule-evaluation/) — `for`, recovering
- [Group alert notifications](https://grafana.com/docs/grafana/latest/alerting/fundamentals/notifications/group-alert-notifications/) — timer bible
- [Silences](https://grafana.com/docs/grafana/latest/alerting/configure-notifications/create-silence/) / [inhibition](https://grafana.com/docs/grafana/latest/alerting/configure-notifications/inhibition-rules/)
- [Webhook](https://grafana.com/docs/grafana/latest/alerting/configure-notifications/manage-contact-points/integrations/webhook-notifier/)

---

## 8. First week checklist

1. Init module + HTTP hello (PR0)
2. Import a sample; one threshold rule; stdout “firing” on tick (PR1)
3. `for` + pending; tests with fake clock (PR2)
4. `PutAlerts` only on firing/resolved; receiver-side fingerprint + merge tests (PR3)
5. Group wait / interval / delete / repeat-multiple + dedup tests (PR4)
6. Webhook JSON + retry + silence (PR5–PR6)

Stop before Alert Gateway, Slack, HA, or a full policy tree.

---

## 9. Success criteria

- [ ] Two-dimensional rule: one instance pending, one firing; **only firing** hits webhook
- [ ] `for` respected under a fake clock
- [ ] Group wait coalesces A+B into the first POST
- [ ] Group interval batches C; repeat reminder does not recreate the group
- [ ] Resolve-all deletes the group; next fire pays group wait (new birth)
- [ ] Repeat coerced to a multiple of group interval
- [ ] Dedup: unchanged firing set before repeat → no send; resolve without prior notified fire → no resolve notification
- [ ] `group_by: [...]` (GroupByAll) puts different label sets in different groups
- [ ] Resolved alert deleted after successful notify; retained (≤ 3 × group interval) on failure
- [ ] Webhook failure retried with backoff; success written to nflog exactly once
- [ ] Fingerprint is receiver-side FNV-1a over canonical labels; annotation change ≠ new alert; label change = new alert; resolve reuses identical labels
- [ ] Silenced alerts still listed (status `suppressed`) via the inspect API
- [ ] Silence: notify off, instance still `firing`
- [ ] Contact point is a dumb adapter (stdout + webhook); no grouping inside it
- [ ] `docs/LEARNING.md` lists Grafana features omitted and restates H1–H7 from the code you wrote

**Out of scope for this plan:** implementing Alert Gateway, Canvas/Confluence write-ups, Grafana HA, Mimir ruler vs Grafana-managed rules.
