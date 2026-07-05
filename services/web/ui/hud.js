// hud.js — on-screen latency HUD, shared by /pion and /livekit so the two are
// measured identically. It taps the mic + agent MediaStreamTracks with Web Audio
// AnalyserNodes and measures *perceived* response latency: the gap from when you
// stop speaking to when the agent's audio starts. Shows last / avg / min / max.
(function () {
  const SPEAK = 0.015; // mic RMS above this = speech
  const SILENCE = 0.008; // mic RMS below this = quiet
  const AGENT_ON = 0.015; // agent RMS above this = agent started talking
  const END_MS = 200; // quiet this long after speech = "you stopped"
  const STALE_MS = 8000; // give up waiting for a response after this

  let actx, micA, remA, micBuf, remBuf, raf;
  const s = { speaking: false, lastSpeech: 0, armed: false, userStop: 0, last: 0, n: 0, sum: 0, min: Infinity, max: 0 };

  let box;
  function ui() {
    if (box) return box;
    box = document.createElement("div");
    box.id = "hud";
    box.innerHTML = `<div class="hud-t">latency</div><div class="hud-v" id="hud-last">—</div>
      <div class="hud-s" id="hud-stats">talk to measure</div>`;
    document.body.appendChild(box);
    return box;
  }
  function render() {
    ui();
    const last = document.getElementById("hud-last");
    const stats = document.getElementById("hud-stats");
    if (!s.n) { last.textContent = "—"; return; }
    last.textContent = s.last + " ms";
    last.className = "hud-v " + (s.last < 500 ? "good" : s.last < 800 ? "ok" : "bad");
    stats.textContent = `avg ${Math.round(s.sum / s.n)} · min ${s.min} · max ${s.max} · n ${s.n}`;
  }

  function rms(an, buf) {
    an.getFloatTimeDomainData(buf);
    let sum = 0;
    for (let i = 0; i < buf.length; i++) sum += buf[i] * buf[i];
    return Math.sqrt(sum / buf.length);
  }

  function loop() {
    const now = performance.now();
    if (micA) {
      const m = rms(micA, micBuf);
      if (m > SPEAK) { s.speaking = true; s.lastSpeech = now; }
      else if (s.speaking && m < SILENCE && now - s.lastSpeech > END_MS) {
        s.speaking = false; s.armed = true; s.userStop = now;
      }
    }
    if (remA && s.armed) {
      const r = rms(remA, remBuf);
      if (r > AGENT_ON) {
        const d = Math.round(now - s.userStop);
        s.armed = false;
        if (d > 50) { // ignore sub-50ms (echo/false trigger)
          s.last = d; s.n++; s.sum += d; s.min = Math.min(s.min, d); s.max = Math.max(s.max, d);
          render();
        }
      } else if (now - s.userStop > STALE_MS) {
        s.armed = false; // response never detected; drop it
      }
    }
    raf = requestAnimationFrame(loop);
  }

  function ensure() {
    if (!actx) actx = new (window.AudioContext || window.webkitAudioContext)();
    if (actx.state === "suspended") actx.resume();
    if (!raf) raf = requestAnimationFrame(loop);
    ui();
  }
  function analyser(track) {
    const src = actx.createMediaStreamSource(new MediaStream([track]));
    const an = actx.createAnalyser();
    an.fftSize = 1024;
    src.connect(an);
    return an;
  }

  window.HUD = {
    mic(track) { if (!track) return; ensure(); micA = analyser(track); micBuf = new Float32Array(micA.fftSize); },
    remote(track) { if (!track) return; ensure(); remA = analyser(track); remBuf = new Float32Array(remA.fftSize); },
    reset() {
      micA = remA = null; s.speaking = s.armed = false;
      // keep the accumulated stats visible after a call ends
      render();
    },
  };
})();
