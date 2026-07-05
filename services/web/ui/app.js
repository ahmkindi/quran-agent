// quran-agent browser voice client: WebRTC mic <-> server, WebSocket signaling.
// Shared visuals (Star / Panel / Transcript) live in star.js.
const $ = (id) => document.getElementById(id);
const card = $("card"), statusEl = $("status"), btn = $("callBtn"), remote = $("remote");

// ICE config (STUN/TURN + ephemeral creds) comes from the server so both
// transports share one source of truth; falls back to public STUN.
async function rtcConfig() {
  try {
    const r = await fetch("/rtc-config");
    if (!r.ok) throw new Error(r.status);
    const { iceServers, forceRelay } = await r.json();
    return { iceServers, iceTransportPolicy: forceRelay ? "relay" : "all" };
  } catch {
    return { iceServers: [{ urls: "stun:stun.l.google.com:19302" }] };
  }
}

let pc = null, ws = null, micStream = null;
let remoteReady = false, pendingCandidates = [];

const STATES = {
  idle:       { status: "Tap to talk",     btn: "Call" },
  connecting: { status: "Connecting…",     btn: "Connecting…" },
  "in-call":  { status: "Listening…",      btn: "Hang up" },
  ended:      { status: "Call ended",      btn: "Call" },
  error:      { status: "Something broke",  btn: "Retry" },
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
    micStream = await navigator.mediaDevices.getUserMedia({
      audio: { echoCancellation: true, noiseSuppression: true, autoGainControl: true },
      video: false,
    });
  } catch (e) {
    console.error(e); setState("error"); statusEl.textContent = "Mic permission needed"; return;
  }
  const micTrack = micStream.getAudioTracks()[0];
  HUD.mic(micTrack);
  Star.mic(micTrack);

  pc = new RTCPeerConnection(await rtcConfig());
  micStream.getTracks().forEach((t) => pc.addTrack(t, micStream));

  pc.ontrack = (ev) => {
    remote.srcObject = ev.streams[0];
    const t = ev.streams[0].getAudioTracks()[0];
    HUD.remote(t);
    Star.remote(t);
  };
  pc.onicecandidate = (ev) => {
    if (ev.candidate) sendWS({ type: "ice", candidate: ev.candidate });
  };
  pc.onconnectionstatechange = () => {
    const cs = pc.connectionState;
    if (cs === "connected") setState("in-call");
    else if (cs === "failed" || cs === "disconnected") hangup();
  };

  ws = new WebSocket(wsURL());
  ws.onopen = async () => {
    const offer = await pc.createOffer({ offerToReceiveAudio: true });
    await pc.setLocalDescription(offer);
    sendWS({ type: "offer", sdp: offer.sdp });
  };
  ws.onmessage = (ev) => onSignal(JSON.parse(ev.data));
  ws.onclose = () => { if (card.dataset.state === "in-call") setState("ended"); };
  ws.onerror = () => setState("error");
}

async function onSignal(m) {
  switch (m.type) {
    case "answer":
      await pc.setRemoteDescription({ type: "answer", sdp: m.sdp });
      remoteReady = true;
      for (const c of pendingCandidates) await pc.addIceCandidate(c).catch(() => {});
      pendingCandidates = [];
      break;
    case "ice":
      if (!m.candidate) break;
      if (remoteReady) await pc.addIceCandidate(m.candidate).catch(() => {});
      else pendingCandidates.push(m.candidate);
      break;
    case "transcript":
      Transcript.render(m);
      break;
    case "tool":
      Panel.tool(m);
      break;
    case "error":
      console.error("server:", m.text); setState("error");
      break;
  }
}

function hangup() {
  try { sendWS({ type: "bye" }); } catch {}
  if (ws) { ws.close(); ws = null; }
  if (pc) { pc.close(); pc = null; }
  if (micStream) { micStream.getTracks().forEach((t) => t.stop()); micStream = null; }
  remoteReady = false; pendingCandidates = [];
  HUD.reset();
  Star.reset();
  Panel.reset();
  setState("ended");
}

function sendWS(obj) {
  if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify(obj));
}

function wsURL() {
  const proto = location.protocol === "https:" ? "wss" : "ws";
  return `${proto}://${location.host}/ws`;
}

// Tappable recitation controls: same actions as the voice keywords.
Panel.bindControls((action) => sendWS({ type: "control", status: action }));

setState("idle");
