# marchialerts

A learning MVP that re-implements the core of Grafana Alerting in one binary:
**evaluate → policy/group → notify**. Not a Grafana fork, not a production
notifier — see [PLAN.md](PLAN.md) for the full plan and
[docs/LEARNING.md](docs/LEARNING.md) for the Grafana ↔ marchialerts mapping.

## Quick start

```sh
go run ./cmd/marchialerts -config config.example.yaml
curl localhost:9094/healthz   # ok

# feed two series; the breaching one logs state=firing on the next tick
curl -X POST localhost:9094/api/v1/import -d '[
  {"metric":"cpu_usage","labels":{"job":"api","host":"a"},"v":0.95},
  {"metric":"cpu_usage","labels":{"job":"api","host":"b"},"v":0.10}
]'
```

Flags: `-httpListenAddr` (default `:9094`), `-config`, `-evalInterval`.

## Status

- [x] PR0 — scaffold: YAML config (with `repeat_interval` coercion), `/healthz`, Dockerfile
- [x] PR1 — metrics in (`POST /api/v1/import`) + eval tick: threshold rules log `firing`/`ok` per series
- [x] PR2 — instance state machine + `for`: `normal → pending → firing → resolved`, fake-clock tests
- [x] PR3 — `PutAlerts` boundary + receiver-side fingerprint (FNV-1a canonical labels; merge; rolling `endsAt`)
- [x] PR4 — aggregation groups + three timers + dedup (group wait/interval/repeat, GroupByAll, nflog gating, group teardown)
- [x] PR5 — contact points (stdout, webhook) + retry (bounded backoff; 5xx retryable, 4xx permanent; fan-out)
- [x] PR6 — silences (matcher gate in flush; eval keeps running, webhook stays silent, alert stays firing)
- [ ] PR7 — optional: inhibition, time intervals, snapshot file, Kafka/file adapter

## Docker

```sh
docker build -t marchialerts .
docker run --rm -p 9094:9094 marchialerts
```
