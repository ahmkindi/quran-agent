// quran-agent LiveKit client: connects to the self-hosted LiveKit room; the Go bot
// participant bridges audio to the same agent. Uses LiveKit's own media stack.
// Transcripts + tool events arrive on the room data channel (same JSON shapes
// as the pion signaling socket); shared visuals live in star.js.
const { Room, RoomEvent, ConnectionState, Track } = LivekitClient;

const $ = (id) => document.getElementById(id);
const card = $("card"), statusEl = $("status"), btn = $("callBtn");

let room = null;

const STATES = {
  idle:       { status: "Tap to talk",    btn: "Call" },
  connecting: { status: "Connecting…",    btn: "Connecting…" },
  "in-call":  { status: "Listening…",     btn: "Hang up" },
  ended:      { status: "Call ended",     btn: "Call" },
  error:      { status: "Something broke", btn: "Retry" },
};

function setState(name) {
  card.dataset.state = name;
  const s = STATES[name] || STATES.idle;
  statusEl.textContent = s.status;
  btn.textContent = s.btn;
  btn.disabled = name === "connecting";
}

btn.onclick = () => {
  const st = card.dataset.state;
  if (st === "in-call" || st === "connecting") hangup();
  else call();
};

async function call() {
  Transcript.reset();
  Panel.reset();
  setState("connecting");
  try {
    const res = await fetch("/livekit/token");
    if (!res.ok) throw new Error("token " + res.status);
    const { url, token } = await res.json();

    // Shared ICE config (STUN/TURN + ephemeral creds) from the server; lets us
    // add TURN for strict NATs and force relay for validation.
    let rtcCfg;
    try {
      const rc = await fetch("/rtc-config?transport=livekit");
      if (rc.ok) {
        const { iceServers, forceRelay } = await rc.json();
        rtcCfg = { iceServers, iceTransportPolicy: forceRelay ? "relay" : "all" };
      }
    } catch {}

    room = new Room({ adaptiveStream: true, dynacast: true });

    room.on(RoomEvent.ConnectionStateChanged, (state) => {
      if (state === ConnectionState.Connected) setState("in-call");
      else if (state === ConnectionState.Disconnected) {
        if (card.dataset.state === "in-call") setState("ended");
      }
    });

    room.on(RoomEvent.TrackSubscribed, (track) => {
      if (track.kind === Track.Kind.Audio) {
        const el = track.attach();
        el.autoplay = true;
        document.body.appendChild(el);
        HUD.remote(track.mediaStreamTrack);
        Star.remote(track.mediaStreamTrack);
      }
    });

    // Bot -> browser UI events (transcript lines, tool + recitation updates).
    room.on(RoomEvent.DataReceived, (payload) => {
      let m;
      try { m = JSON.parse(new TextDecoder().decode(payload)); } catch { return; }
      if (m.type === "transcript") Transcript.render(m);
      else if (m.type === "tool") Panel.tool(m);
    });

    // Autoplay may be blocked until a user gesture; this click is one.
    room.on(RoomEvent.AudioPlaybackStatusChanged, () => {
      if (!room.canPlaybackAudio) room.startAudio().catch(() => {});
    });

    await room.connect(url, token, rtcCfg ? { rtcConfig: rtcCfg } : undefined);
    await room.startAudio().catch(() => {});
    await room.localParticipant.setMicrophoneEnabled(true, {
      echoCancellation: true,
      noiseSuppression: true,
      autoGainControl: true,
    });
    const micPub = room.localParticipant.getTrackPublication(Track.Source.Microphone);
    const micTrack = micPub && micPub.track && micPub.track.mediaStreamTrack;
    HUD.mic(micTrack);
    Star.mic(micTrack);
  } catch (e) {
    console.error(e);
    setState("error");
    if (String(e).includes("token")) statusEl.textContent = "LiveKit not configured";
  }
}

async function hangup() {
  try { if (room) await room.disconnect(); } catch {}
  room = null;
  HUD.reset();
  Star.reset();
  Panel.reset();
  setState("ended");
}

// Tappable recitation controls: same actions as the voice keywords, sent to
// the bot over the reliable data channel.
Panel.bindControls((action) => {
  if (!room || !room.localParticipant) return;
  const data = new TextEncoder().encode(JSON.stringify({ type: "control", status: action }));
  room.localParticipant.publishData(data, { reliable: true }).catch(() => {});
});

setState("idle");
