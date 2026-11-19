# Feature: Starlark timer schedules (cron)

| | |
|---|---|
| Status | Implemented: the `timer_schedule` declaration, parser, compile-time rules, timer loop integration, the read-only API field, and kammon awareness. |
| Scope | Timer declaration syntax, compile contract, the automation timer loop, the scripts API script object, docs, example and monitor. |
| Related | [FeatureStarlarkPeriodicPosts.md](FeatureStarlarkPeriodicPosts.md) (the interval timer this extends), [FeatureStarlark.md](FeatureStarlark.md) (runtime and scripts API), [FeatureUnifiedApi.md](FeatureUnifiedApi.md) (transport rules). |

## Summary

The interval timer (`timer_interval_minutes`, FeatureStarlarkPeriodicPosts)
fires *N minutes after enable/restart* — the schedule drifts with every
restart. Operators who want wall-clock-aligned runs ("daily at 07:00") now have
a second, mutually exclusive declaration:

```starlark
timer_schedule = "0 7 * * *"

def on_timer(now):
    room_post("lobby", body_text)
```

- Standard five cron fields (minute, hour, day-of-month, month, weekday),
  plus an **optional seconds field** in front: `*/1 * * * * *` is one per
  second. Seconds are also what tests use, so integration tests do not need
  the minute-warping `timerUnit` seam for cron scripts.
- Slots align with the clock. The first fire is the first slot at or after the
  script is saved/enabled/recovered or KAM restarts; a missed slot (KAM down)
  is skipped, exactly like the interval's no-catch-up rule. Editing the
  schedule of a running script replaces its pending fire at the save, so a new
  expression takes effect immediately (the timer loop keys each pending fire by
  the declaration it was computed from).
- A plain string literal, parsed and rejected at compile time by the same
  `Compile` path validate and save use. Nothing else about scripts changes:
  timeouts, freeze/recover, and the target checks apply identically.

Existing scripts keep compiling and running unchanged; `pinger` and the
shipped example still use the interval. Exactly one of the two timer lines may
be set.

## Cron in practice

Fields read left to right as **minute hour day-of-month month weekday**, with an
optional seconds field in front. Every expression below was checked with
`timerCronParser`; the "first runs" column assumes a Thursday 21:52 save.

| You want | Expression | First runs |
|---|---|---|
| Daily at 21:50 | `50 21 * * *` | Fri 21:50, Sat 21:50 |
| Every hour on the hour | `0 * * * *` | 22:00, 23:00, 00:00 |
| Every 15 minutes | `*/15 * * * *` | 22:00, 22:15, 22:30 |
| 21:50 on weekdays | `50 21 * * 1-5` | Fri 21:50, Mon 21:50 |
| Twice a day, 07:50 and 21:50 | `50 7,21 * * *` | Fri 07:50, Fri 21:50 |
| First of the month at 08:00 | `0 8 1 * *` | 1 Dec 08:00, 1 Jan 08:00 |
| Every 10 seconds (testing) | `*/10 * * * * *` | 21:52:10, 21:52:20 |

Weekdays number `0`–`6` from Sunday, so `1-5` is Monday to Friday. A comma
lists alternatives (`7,21` = those two hours); a slash means "every N"
(`*/15` = every 15 minutes).

**Limit worth stating plainly:** cron cannot express a different minute per
hour. `50 21,7 * * *` is *not* 21:50 plus 07:00 — it selects minute 50 in both
hours, i.e. 21:50 and 07:50. Two different minutes need two timer scripts.

## Contract

Same compile-time grammar style as the interval:

- `timer_schedule` must be a plain string literal assignment at the top level.
  Anything else is a compile error:
  `timer_schedule must be a string literal cron expression`.
- The text must parse as standard cron (optional seconds; no `@every` and no
  other `@` descriptors), else
  `timer_schedule "<text>" is not a valid cron expression: <reason>`.
- The parsed schedule is the timer floor: one slot is the smallest expressible
  period, so the smallest a script may run is once per second — a bad script
  cannot become a tight loop without a separate floor rule.
- Entry-point rules match the interval's:
  `timer_schedule` without callable `on_timer(now)` → error; `on_timer`
  without either timer line → error; both lines set →
  `script must not set both timer_interval_minutes and timer_schedule; keep one`.
- An empty string literal means "no timer", mirroring `timer_interval_minutes = 0`.
- The fire loop applies both forms through one `timerPeriod` seam
  (`nextAfter(t)`); interval adds its step, cron asks the parsed schedule.

## API

The script object gains one read-only field next to
`timer_interval_minutes`, filled by the node adapter from compiled state and
ignored on writes:

```go
TimerSchedule string `json:"timer_schedule,omitempty"`
```

`omitempty` keeps message-script responses unchanged. kammon's script model
and automations page mention both forms; its fixtures pin one of each.

## Dependencies

| Dependency | Why |
|---|---|
| `github.com/robfig/cron/v3` | The cron parser alone (`cron.NewParser` + `Schedule.Next`). Hand-rolling five fields, ranges, steps, lists and DST-correct "next fire" is exactly where a dependency pays off; it is dependency-free itself and used only for parsing and slot computation. |

## Decisions

| # | Decision | Rationale |
|---|---|---|
| D1 | Add a sibling declaration instead of replacing `timer_interval_minutes`. | Existing timer scripts keep working without migration and keep their tested behavior; the cost is a both-lines rejection rule, not a dual grammar forever — the runtime maps both onto one `timerPeriod` seam. |
| D2 | Cron parsing via `robfig/cron/v3`, `@`-descriptors excluded. | The library handles the "next slot ≥ 1 s" computation correctly across DST; excluding `@every` keeps the floor structural (cron's smallest unit is a second) instead of adding a duration check to police. |
| D3 | No seconds-less revalidation floor; one second is the floor. | The existing 1-minute floor applied to the interval literal; cron granularity is coarser than the guard, so the guard would be unreachable — and seconds are what tests and chatty operators legitimately want. |
| D4 | Schedule text lives in source; the API reports it read-only (`timer_schedule`). | Same reasoning as the interval (D1 there): source is the single definition, storage needs no column, and dashboards should not parse Starlark. |
| D5 | Missed cron slots are skipped, matching the interval's no-catch-up rule. | The fire loop computes "next after now"; a worker restart cannot produce a burst of stale fires on an airtime-capped radio. |

## Verification

1. `go test ./...` green in `kam` and `kammon`, including the new
   `internal/automation/cron_test.go`: fires on slots, no immediate fire,
   disabled stops firing, a schedule change replacing the pending fire,
   `TimerSchedules` reporting, the compile matrix
   (literal rules, invalid expressions, both/all/neither rules).
2. Interval tests stay untouched and green — the two declarations coexist.
3. API tests: `timer_schedule` serializes on list/get and a write's value is
   ignored (`internal/api`, `internal/node`); kammon fixture decodes with one
   interval script and one cron script.
4. `POST /scripts/validate` and `save` reject malformed `timer_schedule`
   sources with the compile error; `dry-run` is unaffected (timer-only
   scripts never had a dry-run entry).
