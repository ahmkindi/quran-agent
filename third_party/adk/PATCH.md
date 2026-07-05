# Patched copy of google.golang.org/adk v1.4.0

This is a verbatim copy of the upstream module (testdata dirs stripped) with two
small patches, wired in via a `replace` directive in the repo root `go.mod`:

```
replace google.golang.org/adk => ./third_party/adk
```

## Why

Upstream v1.4.0 (and v1.5.0) accepts `RealtimeInputConfig` in
`agent.LiveRunConfig` but never copies it into the `genai.LiveConnectConfig`
it sends to the Gemini Live API. Result: all VAD / endpointing tuning
(`SilenceDurationMs`, start/end-of-speech sensitivity) is silently ignored and
the session runs at the server default (~800 ms end-of-speech silence) — which
alone blows the 300–500 ms voice-to-voice latency budget. Upstream also does
not expose `ContextWindowCompression`, so audio-only Live sessions are
hard-terminated at ~15 minutes.

## The patches (grep for "quran-agent patch")

1. `internal/llminternal/base_flow.go` — forward `RealtimeInputConfig` and
   `ContextWindowCompression` from `LiveRunConfig` into `LiveConnectConfig`,
   plus a one-line "live connect: vad silence_ms=..." log at connect time so
   the forwarding is verifiable in agent logs.
2. `agent/live.go` — add the `ContextWindowCompression` field to
   `LiveRunConfig`.

Verified live 2026-07-02: agent log shows
`live connect: vad silence_ms=3000 start="START_SENSITIVITY_LOW"
end="END_SENSITIVITY_HIGH" compression=true` and the session works. Note:
gemini-3.1-flash-live-preview's endpointing is model-based — for a clean,
complete utterance it responds ~1 s after speech end regardless of
silence_ms, so treat the knob as a hint, not a hard floor.

## Maintenance

- When bumping ADK: re-copy the new version here, re-apply the two patches
  (check whether upstream fixed the forwarding first — if so, drop this copy
  and the `replace` directive).
- Upstream issue: TODO file at https://github.com/google/adk-go/issues
