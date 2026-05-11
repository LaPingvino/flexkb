// flexkb live preview frontend. Talks to /api/layouts + /api/compose,
// renders a 4-row physical keyboard with each cell color-coded by which
// composition stage produced it.

const fileSelect = document.getElementById("file");
const variantSelect = document.getElementById("variant");
const metaEl = document.getElementById("meta");
const kbEl = document.getElementById("keyboard");
const warnEl = document.getElementById("warnings");

// XKB key rows. AE = top digit row, AD = QWERTYUIOP row, AC = home row,
// AB = bottom row. We render in that visual order, with TLDE and BKSL
// tucked at the ends. LSGT is the ISO extra key between LShift and Z.
const ROWS = [
  ["TLDE", "AE01", "AE02", "AE03", "AE04", "AE05", "AE06", "AE07", "AE08", "AE09", "AE10", "AE11", "AE12", "AE13"],
  ["AD01", "AD02", "AD03", "AD04", "AD05", "AD06", "AD07", "AD08", "AD09", "AD10", "AD11", "AD12", "BKSL"],
  ["AC01", "AC02", "AC03", "AC04", "AC05", "AC06", "AC07", "AC08", "AC09", "AC10", "AC11"],
  ["LSGT", "AB01", "AB02", "AB03", "AB04", "AB05", "AB06", "AB07", "AB08", "AB09", "AB10", "AB11"],
];

let layouts = [];

async function loadLayouts() {
  const res = await fetch("/api/layouts");
  layouts = await res.json();
  fileSelect.innerHTML = "";
  for (const lf of layouts) {
    const opt = document.createElement("option");
    opt.value = lf.file;
    const defVar = lf.variants.find(v => v.default) || lf.variants[0];
    opt.textContent = `${lf.file}${defVar ? " — " + defVar.description : ""}`;
    fileSelect.appendChild(opt);
  }
  fileSelect.value = layouts[0]?.file || "";
  populateVariants();
}

function populateVariants() {
  const lf = layouts.find(l => l.file === fileSelect.value);
  variantSelect.innerHTML = "";
  if (!lf) return;
  for (const v of lf.variants) {
    const opt = document.createElement("option");
    opt.value = v.name;
    opt.textContent = `${v.name}${v.passthrough ? " (passthrough)" : ""} — ${v.description}`;
    variantSelect.appendChild(opt);
  }
  const def = lf.variants.find(v => v.default) || lf.variants[0];
  if (def) variantSelect.value = def.name;
  loadCompose();
}

async function loadCompose() {
  const file = fileSelect.value;
  const variant = variantSelect.value;
  if (!file || !variant) return;
  const res = await fetch(`/api/compose?file=${encodeURIComponent(file)}&variant=${encodeURIComponent(variant)}`);
  if (!res.ok) {
    metaEl.textContent = `error: ${await res.text()}`;
    return;
  }
  const data = await res.json();
  render(data);
}

function render(data) {
  // Meta panel
  const recipeBits = [];
  if (data.physical) recipeBits.push(`physical: ${data.physical}`);
  if (data.transformation) recipeBits.push(`transformation: ${data.transformation}`);
  if (data.additions && data.additions.length) recipeBits.push(`additions: [${data.additions.join(", ")}]`);
  if (data.substitutions && data.substitutions.length) recipeBits.push(`substitutions: [${data.substitutions.join(", ")}]`);
  if (data.passthrough) recipeBits.push("passthrough: true");
  metaEl.innerHTML = `
    <div class="desc">${escapeHTML(data.description || data.variant)}</div>
    <div class="recipe">${recipeBits.map(escapeHTML).join("  •  ")}</div>
  `;

  // Warnings
  if (data.warnings && data.warnings.length) {
    warnEl.classList.add("show");
    warnEl.innerHTML = `<strong>${data.warnings.length} warning(s):</strong><ul>${
      data.warnings.map(w => `<li>${escapeHTML(w)}</li>`).join("")
    }</ul>`;
  } else {
    warnEl.classList.remove("show");
    warnEl.innerHTML = "";
  }

  // Keyboard grid
  kbEl.innerHTML = "";
  const presentKeys = new Set(data.keys || []);
  for (const row of ROWS) {
    const rowEl = document.createElement("div");
    rowEl.className = "kbrow";
    for (const code of row) {
      if (!presentKeys.has(code)) continue;
      rowEl.appendChild(renderKey(code, data.symbols[code] || []));
    }
    if (rowEl.children.length) kbEl.appendChild(rowEl);
  }
}

function renderKey(code, levels) {
  const el = document.createElement("div");
  el.className = "key";
  const codeEl = document.createElement("span");
  codeEl.className = "code";
  codeEl.textContent = code;
  el.appendChild(codeEl);
  // Render up to 4 levels in fixed grid positions; missing levels render
  // as empty placeholder cells so the layout stays stable.
  for (let i = 0; i < 4; i++) {
    const lvl = levels[i] || { value: "", source: "" };
    const c = document.createElement("span");
    c.className = `cell l${i} ${lvl.source ? "src-" + lvl.source : "src-empty"}`;
    c.textContent = displayValue(lvl.value);
    c.title = lvl.value ? `level ${i+1} (${lvl.source || "—"}): ${lvl.value}` : `level ${i+1} (empty)`;
    el.appendChild(c);
  }
  return el;
}

// displayValue converts xkb symbol tokens to a more readable form for
// the grid: U+XXXX codepoints to the actual glyph, well-known single-
// letter tokens unchanged. Long descriptive names get truncated so they
// don't break the cell grid.
function displayValue(v) {
  if (!v) return "";
  const m = /^U([0-9A-Fa-f]{4,6})$/.exec(v);
  if (m) {
    try { return String.fromCodePoint(parseInt(m[1], 16)); } catch (e) { return v; }
  }
  // Latin letter tokens: "a", "B", "comma", etc. — keep short ones,
  // shorten long descriptive ones.
  if (v.length <= 3) return v;
  // Common token shortcuts.
  const tokens = {
    "space": "␣", "Tab": "↹", "BackSpace": "⌫", "Return": "⏎",
    "Escape": "⎋", "comma": ",", "period": ".", "slash": "/",
    "semicolon": ";", "apostrophe": "'", "minus": "-", "equal": "=",
    "bracketleft": "[", "bracketright": "]", "backslash": "\\",
    "grave": "`", "asciitilde": "~", "exclam": "!", "at": "@",
    "numbersign": "#", "dollar": "$", "percent": "%", "asciicircum": "^",
    "ampersand": "&", "asterisk": "*", "parenleft": "(", "parenright": ")",
    "underscore": "_", "plus": "+", "braceleft": "{", "braceright": "}",
    "bar": "|", "colon": ":", "quotedbl": "\"", "less": "<", "greater": ">",
    "question": "?",
  };
  if (tokens[v]) return tokens[v];
  // Truncate long names so the grid stays usable.
  return v.length > 7 ? v.slice(0, 6) + "…" : v;
}

function escapeHTML(s) {
  return String(s).replace(/[&<>"']/g, c => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;"
  }[c]));
}

fileSelect.addEventListener("change", populateVariants);
variantSelect.addEventListener("change", loadCompose);
loadLayouts();
