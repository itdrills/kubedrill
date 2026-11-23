"use strict";
// KubeDrill browser UI – no framework, no external requests. Talks only to the
// local `kubedrill ui` server (same origin).

const out = document.getElementById("output");
const form = document.getElementById("prompt");
const input = document.getElementById("line");
const history = [];
let histPos = 0;
let busy = false;

function print(text, cls) {
  const span = document.createElement("span");
  if (cls) span.className = cls;
  span.textContent = text; // never innerHTML: output comes from kubectl and scenarios
  out.appendChild(span);
  out.scrollTop = out.scrollHeight;
}

async function api(path, body) {
  const opts = body === undefined
    ? { method: "GET", credentials: "same-origin" }
    : { method: "POST", credentials: "same-origin", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) };
  const res = await fetch(path, opts);
  if (res.status === 401 || res.status === 403) throw new Error("Session expired – open the link printed by `kubedrill ui` again.");
  return res.json();
}

async function run(line) {
  line = line.trim();
  if (!line || busy) return;
  busy = true;
  input.disabled = true;
  if (history[history.length - 1] !== line) history.push(line);
  histPos = history.length;
  print("$ " + line + "\n", "cmd");
  try {
    const r = await api("/api/run", { line });
    print(r.output, r.rejected ? "rejected" : (r.exitCode !== 0 ? "error" : ""));
  } catch (e) {
    print(e.message + "\n", "error");
  } finally {
    busy = false;
    input.disabled = false;
    input.focus();
    refresh();
  }
}

form.addEventListener("submit", (ev) => {
  ev.preventDefault();
  const line = input.value;
  input.value = "";
  run(line);
});

input.addEventListener("keydown", (ev) => {
  if (ev.key === "ArrowUp" && histPos > 0) {
    histPos--;
    input.value = history[histPos];
    ev.preventDefault();
  } else if (ev.key === "ArrowDown") {
    histPos = Math.min(histPos + 1, history.length);
    input.value = history[histPos] || "";
    ev.preventDefault();
  } else if (ev.key === "l" && ev.ctrlKey) {
    out.textContent = "";
    ev.preventDefault();
  }
});

document.querySelectorAll("button[data-run]").forEach((b) => {
  b.addEventListener("click", () => {
    const msg = b.dataset.confirm;
    if (msg && !window.confirm(msg)) return;
    run(b.dataset.run);
  });
});

function el(tag, text, cls) {
  const e = document.createElement(tag);
  if (text !== undefined) e.textContent = text;
  if (cls) e.className = cls;
  return e;
}

function render(state) {
  const status = document.getElementById("status");
  if (!state.running) {
    status.textContent = "simulator stopped";
    status.className = "pill bad";
    document.getElementById("challenge").hidden = true;
    document.getElementById("no-challenge").hidden = false;
    document.getElementById("catalog").replaceChildren(el("li", state.error || "Run `kubedrill start` in your terminal."));
    return;
  }
  status.textContent = "simulator running";
  status.className = "pill ok";
  const ch = state.challenge;
  document.getElementById("challenge").hidden = !ch;
  document.getElementById("no-challenge").hidden = !!ch;
  if (ch) {
    document.getElementById("ch-title").textContent = ch.title;
    document.getElementById("ch-difficulty").textContent = ch.difficulty;
    document.getElementById("ch-hints").textContent = ch.hintsRevealed + " of " + ch.hintsTotal + " hints used" + (ch.solved ? " · solved" : "");
    document.getElementById("ch-description").textContent = (ch.description || "").trim();
    const crit = (ch.check && ch.check.criteria) || [];
    document.getElementById("ch-criteria").replaceChildren(...crit.map((c) => {
      const li = el("li", (c.passed ? "✓ " : "✗ ") + c.description, c.passed ? "ok" : "");
      if (c.detail) li.appendChild(el("small", " (" + c.detail + ")"));
      return li;
    }));
    return;
  }
  const list = (state.catalog && state.catalog.scenarios) || [];
  document.getElementById("catalog").replaceChildren(...list.map((s) => {
    const li = el("li");
    const b = el("button", s.title);
    b.type = "button";
    b.addEventListener("click", () => run("kubedrill challenge start " + s.id));
    li.append(b, el("small", " " + s.difficulty + " · ~" + s.estimatedMinutes + " min" + (s.solved ? " · solved" : "")));
    return li;
  }));
}

async function refresh() {
  try {
    render(await api("/api/state"));
  } catch (e) {
    render({ running: false, error: e.message });
  }
}

print("KubeDrill browser terminal. Try: kubectl get nodes\n", "muted");
refresh();
setInterval(() => { if (!busy && !document.hidden) refresh(); }, 5000);
