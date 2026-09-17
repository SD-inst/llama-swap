---
title: Automatic model unloading with ttl
summary: How ttl, globalTTL, unloadTimeout and Docker cmdStop interact, how to unload on demand, and how slotPersistence keeps the KV cache across swaps.
category: guides
tags: [ttl, unload, vram, memory, idle, docker, container, cmd-stop, slot-persistence, kv-cache]
config_keys: [globalTTL, unloadTimeout, models.*.ttl, models.*.unloadTimeout, models.*.cmdStop, models.*.slotPersistence]
updated: 2026-09-18
---

# Automatic model unloading with ttl

By default llama-swap keeps a model loaded until something else needs the GPU.
TTL frees the VRAM after a period of inactivity instead.

Docker needs one extra setting: use `cmdStop: docker stop ${MODEL_ID}` so an
unload stops the container, not only the local `docker run` client process.

## The two settings

```yaml
# global default, in seconds. 0 = never unload automatically
globalTTL: 0

models:
  qwen-coder:
    ttl: 300      # unload after 5 minutes idle
    cmd: llama-server --port ${PORT} -m /models/qwen.gguf
```

`models.*.ttl` values and what they mean:

| value | behaviour |
| --- | --- |
| `-1` (default) | inherit `globalTTL` |
| `0` | never unload automatically |
| `> 0` | unload after this many seconds of inactivity |

So `globalTTL: 600` with no per-model `ttl` unloads every model after 10
minutes idle. A model that should stay resident sets `ttl: 0` to opt out.

The timer measures **inactivity**, not lifetime — it resets on every request.

## `unloadTimeout` is a different thing

`ttl` decides *when* to unload. `unloadTimeout` decides *how long to wait* for
the process to exit once unloading starts.

```yaml
unloadTimeout: 10        # global default, seconds

models:
  docker-llama:
    unloadTimeout: 30    # docker stop is slow
```

It applies to every unload — TTL expiry, a manual unload, or a swap. A model
whose `unloadTimeout` is `0` uses the global value.

Raise it for anything slow to shut down: containers, vLLM, anything with a
`cmdStop` that talks to another daemon. Too low and llama-swap force-kills a
process mid-shutdown, which can leave a container running and its VRAM held.

For Docker, set `cmdStop: docker stop ${MODEL_ID}`. llama-swap otherwise stops
the local Docker client process, which can leave the container running. A
ten-minute idle timeout for a container therefore needs both settings:

```yaml
models:
  docker-llama:
    ttl: 600
    cmdStop: docker stop ${MODEL_ID}
    unloadTimeout: 30
```

## Unloading on demand

```console
$ curl http://localhost:8080/unload                        # everything
$ curl -X POST http://localhost:8080/api/models/unload      # everything
$ curl -X POST http://localhost:8080/api/models/unload/qwen-coder
```

The web UI's model list has an unload button per model that hits the same
endpoint.

## Keeping the KV cache across swaps (slotPersistence)

Unloading frees VRAM by dropping the model. With `slotPersistence`, llama-swap
can also save the model's prompt-cache slots to disk on unload and restore
them on the next load, so the next request reuses the KV cache instead of
recomputing it. It talks to llama-server's `/slots` API, so it only works with
`llama-server` started with `--slot-save-path`:

```yaml
models:
  qwen-coder:
    cmd: llama-server --port ${PORT} -np 2 --slot-save-path /kv-cache/qwen-coder -m /models/qwen.gguf
    slotPersistence:
      path: /kv-cache/qwen-coder   # must match --slot-save-path; setting it enables save/restore
      slots: 2                     # must match -np
      deleteAfterRestore: true     # optional; good with tmpfs
```

Omit the `slotPersistence` block (or leave `path` empty) to disable it, the
same way other per-model blocks are switched off.

- `path` **must** match llama-server's `--slot-save-path`; `slots` **must**
  match its `-np`. A mismatch makes saves/restores target the wrong slots.
- Restore is **non-fatal**: a bad or interrupted cache is dropped and the slot
  serves cold. The save/restore markers ensure an interrupted save is never
  mistaken for a complete one.
- `deleteAfterRestore` removes the files after a successful restore, so on a
  tmpfs the KV cache is not held in RAM twice (once as files, once in the live
  slot).

## Picking a value

- **Single GPU, one model at a time.** TTL buys you little — swapping already
  evicts the old model. `globalTTL: 0` is fine.
- **Several models resident together** (see `guides/routing/groups-and-matrix`). TTL is how
  you reclaim VRAM from the ones that have gone quiet. Something in the 300–900
  second range is a reasonable start.
- **Slow-loading models** (large weights, vLLM). A short TTL is expensive — you
  pay a long cold start on the next request. Give them a long TTL or `ttl: 0`
  and let the router evict them only when it must.
- **Keeping a small model always warm.** `ttl: 0` plus a `persistent` group.

## Related

- `reference/config/globalTTL` and `reference/config/unloadTimeout`
- `guides/routing/groups-and-matrix` — controlling what runs together
