// Shared visuals for both transports (pion + LiveKit):
//   Star       — audio-reactive 8-fold shamsa (mic pulses the emerald core,
//                agent/reciter audio blooms the rosette + gold glow)
//   Panel      — the now-playing ayah panel + stop-word hint + tool chip
//   Transcript — bilingual transcript lines (RTL + Quran font for Arabic)
// All animation is transform/opacity writes; nothing here touches layout or
// the audio path.

const Star = (() => {
  const $ = (id) => document.getElementById(id);
  const reduced = matchMedia("(prefers-reduced-motion: reduce)").matches;

  let ctx = null; // shared AudioContext, created on first use (post-gesture)
  let mic = null, remote = null; // { analyser, buf, src, level }
  let raf = 0;

  function analyserFor(track) {
    if (!ctx) ctx = new (window.AudioContext || window.webkitAudioContext)();
    if (ctx.state === "suspended") ctx.resume();
    const src = ctx.createMediaStreamSource(new MediaStream([track]));
    const analyser = ctx.createAnalyser();
    analyser.fftSize = 512;
    src.connect(analyser);
    return { analyser, buf: new Uint8Array(analyser.fftSize), src, level: 0 };
  }

  // Time-domain RMS (0..~0.5 for speech), exponentially smoothed.
  function level(a) {
    a.analyser.getByteTimeDomainData(a.buf);
    let sum = 0;
    for (let i = 0; i < a.buf.length; i++) {
      const v = (a.buf[i] - 128) / 128;
      sum += v * v;
    }
    const rms = Math.sqrt(sum / a.buf.length);
    a.level += (rms - a.level) * (rms > a.level ? 0.4 : 0.12); // fast attack, slow decay
    return a.level;
  }

  function frame() {
    const rose = $("starRose"), glow = $("starGlow"), core = $("starCore");
    if (!rose) return;
    const m = mic ? level(mic) : 0;
    const r = remote ? level(remote) : 0;
    if (!reduced) {
      core.style.transform = `scale(${(1 + Math.min(m * 3.2, 1.4)).toFixed(3)})`;
      rose.style.transform = `scale(${(1 + Math.min(r * 0.9, 0.24)).toFixed(3)})`;
    }
    glow.style.opacity = Math.min(r * 4.5, 0.9).toFixed(3);
    raf = requestAnimationFrame(frame);
  }

  function start() {
    if (!raf) raf = requestAnimationFrame(frame);
  }

  function drop(a) {
    if (a) try { a.src.disconnect(); } catch {}
  }

  return {
    mic(track) { if (!track) return; drop(mic); mic = analyserFor(track); start(); },
    remote(track) { if (!track) return; drop(remote); remote = analyserFor(track); start(); },
    reset() {
      drop(mic); drop(remote);
      mic = remote = null;
      cancelAnimationFrame(raf); raf = 0;
      const rose = $("starRose"), glow = $("starGlow"), core = $("starCore");
      if (rose) rose.style.transform = "";
      if (glow) glow.style.opacity = "0";
      if (core) core.style.transform = "";
    },
  };
})();

const Panel = (() => {
  const $ = (id) => document.getElementById(id);
  const AR_DIGITS = "٠١٢٣٤٥٦٧٨٩";
  const arNum = (n) => String(n).replace(/\d/g, (d) => AR_DIGITS[d]);
  let curRef = null; // "2:255" currently shown, to detect verse changes
  let hideTimer = 0, swapTimer = 0;

  function fill(d) {
    curRef = d.ref;
    $("npSurahAr").textContent = d.surahNameAr || "";
    $("npSurahEn").textContent = [d.surahNameEn, d.surahTranslation]
      .filter(Boolean).join(" · ");
    // ۝ U+06DD encloses the following Arabic-Indic digits (Amiri Quran).
    $("npAyah").textContent = d.textAr ? `${d.textAr} ۝${arNum(d.ayah)}` : d.ref;
    $("npTranslation").textContent = d.translationEn || "";
    const chips = [];
    if (d.juz) chips.push(`Juz ${d.juz}`);
    if (d.revelationType) chips.push(d.revelationType);
    if (d.ayahCount) chips.push(`Ayah ${d.ayah} / ${d.ayahCount}`);
    if (d.verseCount > 1) chips.push(`Verse ${d.verseIndex} of ${d.verseCount}`);
    if (d.repeatCount === 0) chips.push(`Repeat ${d.repeatIndex} · ∞`);
    else if (d.repeatCount > 1) chips.push(`Repeat ${d.repeatIndex} of ${d.repeatCount}`);
    $("npMeta").innerHTML = "";
    for (const c of chips) {
      const s = document.createElement("span");
      s.className = "chip";
      s.textContent = c;
      $("npMeta").appendChild(s);
    }
  }

  function showVerse(d) {
    const np = $("nowplaying");
    if (!np) return;
    clearTimeout(hideTimer);
    if (np.hidden) {
      fill(d);
      np.hidden = false;
      // two frames so the transition runs from the hidden state
      requestAnimationFrame(() => requestAnimationFrame(() => np.classList.add("active")));
    } else if (d.ref !== curRef) {
      // verse-to-verse cross-fade
      const body = $("npBody");
      body.classList.add("swap");
      clearTimeout(swapTimer);
      swapTimer = setTimeout(() => {
        fill(d);
        body.classList.remove("swap");
      }, 170);
    } else {
      fill(d); // same verse (repeat) — just refresh the counters
    }
  }

  function setReciting(on) {
    const card = $("card"), hint = $("hint");
    if (card) card.classList.toggle("reciting", on);
    if (hint) hint.hidden = !on;
  }

  function hide() {
    const np = $("nowplaying");
    if (!np || np.hidden) return;
    np.classList.remove("active");
    clearTimeout(hideTimer);
    hideTimer = setTimeout(() => { np.hidden = true; curRef = null; }, 460);
  }

  // A control word was spotted (web-side, instant): flash its chip.
  function flashKeyword(action) {
    const chip = document.querySelector(`.hint .kw[data-kw="${action}"]`);
    if (!chip) return;
    chip.classList.remove("hit");
    void chip.offsetWidth; // restart the animation on rapid repeats
    chip.classList.add("hit");
    setTimeout(() => chip.classList.remove("hit"), 950);
  }

  // Entry point for every {type:"tool"} message from either transport.
  function tool(m) {
    if (m.name === "recitation") {
      if (m.status === "start") setReciting(true);
      else if (m.status === "verse") {
        setReciting(true);
        try { showVerse(JSON.parse(m.detail)); } catch {}
      } else if (m.status === "stop") {
        setReciting(false);
        hide();
      }
      return;
    }
    if (m.name === "keyword") {
      flashKeyword(m.status);
      return;
    }
    const toolEl = $("tool");
    if (toolEl) toolEl.textContent = m.status === "start" ? `⚙ ${m.name}…` : `✓ ${m.name}`;
  }

  function reset() {
    setReciting(false);
    hide();
    const toolEl = $("tool");
    if (toolEl) toolEl.textContent = "";
  }

  // Make the hint chips tappable: fallback for when the voice spotter misses a
  // control word (Gemini is muted during recitation, so nothing else hears it).
  // send(action) delivers the control over the transport (WS or data channel).
  function bindControls(send) {
    document.querySelectorAll(".hint .kw").forEach((chip) => {
      chip.addEventListener("click", () => {
        const action = chip.dataset.kw;
        if (!action) return;
        flashKeyword(action);
        send(action);
      });
    });
  }

  return { tool, reset, bindControls };
})();

const Transcript = (() => {
  const AR = /[؀-ۿ]/;
  const el = () => document.getElementById("transcript");
  let agentLine = null, userLine = null;

  function newLine(kind) {
    const div = document.createElement("div");
    div.className = `line ${kind} partial`;
    el().appendChild(div);
    return div;
  }

  // Arabic lines get RTL + the Quran face via [lang="ar"].
  function localize(div) {
    const arabic = AR.test(div.textContent);
    div.dir = arabic ? "rtl" : "ltr";
    if (arabic) div.lang = "ar";
    else div.removeAttribute("lang");
  }

  function render(m) {
    if (!el()) return;
    if (m.isUser) {
      if (!userLine) userLine = newLine("user");
      userLine.textContent = m.text;
      localize(userLine);
      if (m.final) { userLine.classList.remove("partial"); userLine = null; }
    } else {
      if (!agentLine) agentLine = newLine("agent");
      // Gemini streams incremental partials, then a full final.
      agentLine.textContent = m.final ? m.text : agentLine.textContent + m.text;
      localize(agentLine);
      if (m.final) { agentLine.classList.remove("partial"); agentLine = null; }
    }
    el().scrollTop = el().scrollHeight;
  }

  function reset() {
    if (el()) el().innerHTML = "";
    agentLine = userLine = null;
  }

  return { render, reset };
})();
