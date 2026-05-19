// flexkb live preview frontend. Three tabs: Browse, Compose, Data
// layers. Talks to /api/layouts, /api/modules, /api/paths,
// /api/compose, /api/compose-spec, /api/save.

// Accessibility hook: mirror the `hidden` CSS class to the actual
// `hidden` attribute on overlay/panel elements, plus manage focus
// trap and return-focus for modal overlays. Many call sites
// toggle .hidden via classList; without these mirrors, assistive
// tech would still treat the hidden overlay as live content and
// keyboard users would Tab out of the modal into the page behind.

// Track the element that had focus before each overlay opened,
// keyed by overlay id, so we can restore focus on close.
const _lastFocusByOverlay = new WeakMap();

// trapTabFocus keeps Tab and Shift+Tab inside an overlay. The
// implementation is deliberately simple: query focusable children
// at keydown time so dynamically-added controls work without
// re-registering anything.
function _trapTabFocus(overlay, event) {
  if (event.key !== "Tab") return;
  const focusables = overlay.querySelectorAll(
    'a[href], button:not([disabled]), textarea:not([disabled]), input:not([disabled]):not([type="hidden"]), select:not([disabled]), [tabindex]:not([tabindex="-1"])'
  );
  if (focusables.length === 0) return;
  const first = focusables[0];
  const last = focusables[focusables.length - 1];
  if (event.shiftKey && document.activeElement === first) {
    event.preventDefault();
    last.focus();
  } else if (!event.shiftKey && document.activeElement === last) {
    event.preventDefault();
    first.focus();
  }
}
document.addEventListener("DOMContentLoaded", () => {
  const selectors = ".inspect-overlay, section.tab";
  const sync = (el) => {
    const isHidden = el.classList.contains("hidden");
    if (isHidden) {
      el.setAttribute("hidden", "");
    } else {
      el.removeAttribute("hidden");
    }
    // Modal-overlay only: focus management.
    if (el.classList.contains("inspect-overlay")) {
      if (isHidden) {
        // Restore focus to where the user was before opening.
        const ret = _lastFocusByOverlay.get(el);
        if (ret && typeof ret.focus === "function") {
          ret.focus();
        }
        _lastFocusByOverlay.delete(el);
      } else {
        // Remember return target, then move focus into the
        // overlay. Heading first (so the screen reader
        // announces what dialog opened), then first focusable
        // control if no heading is reachable.
        _lastFocusByOverlay.set(el, document.activeElement);
        const target = el.querySelector("h3, h2, [autofocus], input, button");
        if (target) {
          // h3 isn't natively focusable — give it tabindex=-1
          // so .focus() works without making it Tab-reachable.
          if (target.tagName === "H3" || target.tagName === "H2") {
            target.setAttribute("tabindex", "-1");
          }
          target.focus();
        }
      }
    }
  };
  document.querySelectorAll(selectors).forEach(sync);
  const obs = new MutationObserver((mutations) => {
    for (const m of mutations) {
      if (m.type === "attributes" && m.attributeName === "class") {
        sync(m.target);
      }
    }
  });
  document.querySelectorAll(selectors).forEach((el) => {
    obs.observe(el, { attributes: true, attributeFilter: ["class"] });
  });

  // Wire Tab-focus-trap on every modal overlay. Listeners stay
  // installed; they no-op when the overlay isn't visible because
  // focus would be elsewhere.
  document.querySelectorAll(".inspect-overlay").forEach((overlay) => {
    overlay.addEventListener("keydown", (e) => _trapTabFocus(overlay, e));
  });
});

const ROWS = [
  ["TLDE", "AE01", "AE02", "AE03", "AE04", "AE05", "AE06", "AE07", "AE08", "AE09", "AE10", "AE11", "AE12", "AE13"],
  ["AD01", "AD02", "AD03", "AD04", "AD05", "AD06", "AD07", "AD08", "AD09", "AD10", "AD11", "AD12", "BKSL"],
  ["AC01", "AC02", "AC03", "AC04", "AC05", "AC06", "AC07", "AC08", "AC09", "AC10", "AC11"],
  ["LSGT", "AB01", "AB02", "AB03", "AB04", "AB05", "AB06", "AB07", "AB08", "AB09", "AB10", "AB11"],
];

// Shared state
let layouts = [];
let modules = { physicals: [], transformations: [], additions: [], substitutions: [] };
let activeLayout = null;
// Latest compose response per render target — used by the per-key
// popover so a click on a key can show the full level breakdown
// without re-fetching.
const lastCompose = new WeakMap();

// === Theme ===
const themeToggle = document.getElementById("themeToggle");
function applyTheme(name) {
  document.body.classList.toggle("theme-light", name === "light");
  themeToggle.textContent = name === "light" ? "☀" : "☾";
  try { localStorage.setItem("flexkb.theme", name); } catch (e) {}
}
themeToggle.addEventListener("click", () => {
  const next = document.body.classList.contains("theme-light") ? "dark" : "light";
  applyTheme(next);
});
try {
  const saved = localStorage.getItem("flexkb.theme");
  if (saved) applyTheme(saved);
} catch (e) {}

// === Unicode picker ===
const unicodeOverlay = document.getElementById("unicodeOverlay");
const unicodeSearchInput = document.getElementById("unicodeSearch");
const unicodeResults = document.getElementById("unicodeResults");
const unicodeStatus = document.getElementById("unicodeStatus");
document.getElementById("unicodeOpenBtn").addEventListener("click", () => openUnicodePicker());
document.getElementById("unicodeClose").addEventListener("click", closeUnicodePicker);
unicodeOverlay.addEventListener("click", (e) => {
  if (e.target === unicodeOverlay) closeUnicodePicker();
});
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape" && !unicodeOverlay.classList.contains("hidden")) closeUnicodePicker();
});

let unicodeSearchTimer = null;
unicodeSearchInput.addEventListener("input", () => {
  clearTimeout(unicodeSearchTimer);
  unicodeSearchTimer = setTimeout(runUnicodeSearch, 80);
});

async function openUnicodePicker() {
  pickerTargetCell = null;
  unicodeOverlay.classList.remove("hidden");
  unicodeSearchInput.value = "";
  unicodeStatus.textContent = "";
  unicodeStatus.className = "status";
  await runUnicodeSearch();
  unicodeSearchInput.focus();
}

// openUnicodePickerForCell opens the picker in "assign to a cell" mode.
// When the user picks a glyph, it lands in the composeOverrides map at
// the given (key, level) and the live preview re-renders to show it.
async function openUnicodePickerForCell(key, level) {
  pickerTargetCell = { key, level };
  unicodeOverlay.classList.remove("hidden");
  unicodeSearchInput.value = "";
  unicodeStatus.textContent = `Picking a glyph for ${key} level ${level + 1} — click any character below to assign, or × to cancel.`;
  unicodeStatus.className = "status ok";
  await runUnicodeSearch();
  unicodeSearchInput.focus();
}

function closeUnicodePicker() {
  unicodeOverlay.classList.add("hidden");
  pickerTargetCell = null;
}

async function runUnicodeSearch() {
  const q = unicodeSearchInput.value;
  const res = await fetch(`/api/unicode?q=${encodeURIComponent(q)}&limit=200`);
  if (!res.ok) {
    unicodeResults.innerHTML = `<div class="empty">error: ${res.status}</div>`;
    return;
  }
  const chars = await res.json();
  unicodeResults.innerHTML = "";
  if (!chars || chars.length === 0) {
    unicodeResults.innerHTML = '<div class="empty">no matches</div>';
    return;
  }
  for (const c of chars) {
    const cell = document.createElement("div");
    cell.className = "glyph-cell";
    cell.textContent = c.glyph;
    cell.title = `U+${c.hex} ${c.name}\n${c.block} · ${c.category}\nClick to copy glyph + xkb token`;
    const hex = document.createElement("span");
    hex.className = "hex";
    hex.textContent = c.hex;
    cell.appendChild(hex);
    cell.addEventListener("click", () => copyUnicodeChar(c));
    unicodeResults.appendChild(cell);
  }
}

async function copyUnicodeChar(c) {
  const token = `U${c.hex.padStart(4, "0")}`;
  rememberRecentUnicode(c);
  // Cell-assign mode — write into composeOverrides and close.
  if (pickerTargetCell) {
    const coord = `${pickerTargetCell.key}:${pickerTargetCell.level}`;
    composeOverrides.set(coord, c.glyph);
    unicodeStatus.textContent = `assigned ${c.glyph} (${token}) to ${pickerTargetCell.key} level ${pickerTargetCell.level + 1}`;
    unicodeStatus.className = "status ok";
    closeUnicodePicker();
    composeLivePreview();
    switchTab("compose");
    return;
  }
  // Default mode — copy to clipboard.
  try {
    await navigator.clipboard.writeText(c.glyph);
    unicodeStatus.textContent = `copied ${c.glyph} to clipboard · xkb token: ${token}`;
    unicodeStatus.className = "status ok";
  } catch (e) {
    unicodeStatus.textContent = `copy failed: ${e.message}. Token: ${token}`;
    unicodeStatus.className = "status err";
  }
}

function rememberRecentUnicode(c) {
  try {
    const key = "flexkb.unicode.recent";
    let recent = JSON.parse(localStorage.getItem(key) || "[]");
    recent = recent.filter(r => r.hex !== c.hex);
    recent.unshift({ hex: c.hex, glyph: c.glyph, name: c.name });
    if (recent.length > 24) recent = recent.slice(0, 24);
    localStorage.setItem(key, JSON.stringify(recent));
  } catch (e) {}
}

// === Tab switching ===
document.querySelectorAll("nav.tabs button").forEach(btn => {
  btn.addEventListener("click", () => switchTab(btn.dataset.tab));
});

// === Browse tab ===
const fileSelect = document.getElementById("file");
const fileFilter = document.getElementById("fileFilter");
const variantSelect = document.getElementById("variant");
const metaEl = document.getElementById("meta");
const kbEl = document.getElementById("keyboard");
const warnEl = document.getElementById("warnings");
const browseSource = document.getElementById("browseSource");
const cmpFile = document.getElementById("cmpFile");
const cmpVariant = document.getElementById("cmpVariant");
const diffSummary = document.getElementById("diffSummary");
const bEditBtn = document.getElementById("bEdit");
const bActivateBtn = document.getElementById("bActivate");
const bShowXKBBtn = document.getElementById("bShowXKB");
const bYAMLBtn = document.getElementById("bYAML");
const bDeleteBtn = document.getElementById("bDelete");
const bStatus = document.getElementById("bStatus");
const bXkb = document.getElementById("bXkb");

async function loadLayouts() {
  const res = await fetch("/api/layouts");
  layouts = await res.json();
  cmpFile.innerHTML = '<option value="">— none —</option>';
  for (const lf of layouts) {
    const opt = document.createElement("option");
    opt.value = lf.file;
    const defVar = lf.variants.find(v => v.default) || lf.variants[0];
    const tag = lf.sourceKind ? ` [${lf.sourceKind}]` : "";
    opt.textContent = `${lf.file}${tag}${defVar ? " — " + defVar.description : ""}`;
    cmpFile.appendChild(opt);
  }
  renderFilePicker();
  populateVariants();
  populateCmpVariants();
}

// renderFilePicker rebuilds the Browse-tab layout-file picker honouring
// the live fileFilter query (matches against file name, variant names,
// and variant descriptions so "dvorak" finds us/de/etc., "polish"
// finds layouts mentioning Polish, and so on).
function renderFilePicker() {
  const q = (fileFilter.value || "").toLowerCase().trim();
  const prev = fileSelect.value;
  fileSelect.innerHTML = "";
  for (const lf of layouts) {
    if (q && !matchesQuery(lf, q)) continue;
    const opt = document.createElement("option");
    opt.value = lf.file;
    const defVar = lf.variants.find(v => v.default) || lf.variants[0];
    const tag = lf.sourceKind ? ` [${lf.sourceKind}]` : "";
    opt.textContent = `${lf.file}${tag}${defVar ? " — " + defVar.description : ""}`;
    fileSelect.appendChild(opt);
  }
  if (fileSelect.options.length === 0) {
    const opt = document.createElement("option");
    opt.value = "";
    opt.disabled = true;
    opt.textContent = "(no layouts match)";
    fileSelect.appendChild(opt);
    return;
  }
  // Restore previous selection if still visible, else pick first.
  if ([...fileSelect.options].some(o => o.value === prev)) {
    fileSelect.value = prev;
  } else {
    fileSelect.value = fileSelect.options[0].value;
  }
}

function matchesQuery(lf, q) {
  if (lf.file.toLowerCase().includes(q)) return true;
  for (const v of lf.variants) {
    if (v.name.toLowerCase().includes(q)) return true;
    if ((v.description || "").toLowerCase().includes(q)) return true;
  }
  return false;
}

fileFilter.addEventListener("input", () => {
  renderFilePicker();
  populateVariants();
});

function populateVariants() {
  const lf = layouts.find(l => l.file === fileSelect.value);
  variantSelect.innerHTML = "";
  browseSource.textContent = "";
  browseSource.className = "layer-tag";
  if (!lf) return;
  for (const v of lf.variants) {
    const opt = document.createElement("option");
    opt.value = v.name;
    opt.textContent = `${v.name}${v.passthrough ? " (passthrough)" : ""} — ${v.description}`;
    variantSelect.appendChild(opt);
  }
  if (lf.sourceKind) {
    browseSource.textContent = `${lf.sourceKind}: ${lf.sourcePath}`;
    browseSource.className = `layer-tag ${lf.sourceKind}`;
  }
  const def = lf.variants.find(v => v.default) || lf.variants[0];
  if (def) variantSelect.value = def.name;
  decorateActiveInPicker();
  updateBrowseControls();
  loadCompose();
}

// updateBrowseControls toggles the Delete button visibility — only
// user-owned layout files are deletable from the GUI.
function updateBrowseControls() {
  const lf = layouts.find(l => l.file === fileSelect.value);
  bDeleteBtn.classList.toggle("hidden", !lf || lf.sourceKind !== "user");
}

async function loadCompose() {
  const file = fileSelect.value;
  const variant = variantSelect.value;
  if (!file || !variant) return;
  bStatus.textContent = "";
  bXkb.classList.add("hidden");
  const res = await fetch(`/api/compose?file=${encodeURIComponent(file)}&variant=${encodeURIComponent(variant)}`);
  if (!res.ok) {
    metaEl.textContent = `error: ${await res.text()}`;
    return;
  }
  const data = await res.json();
  const cmp = await loadCompareCompose();
  renderInto({ meta: metaEl, kb: kbEl, warn: warnEl }, data, cmp);
  updateDiffSummary(data, cmp);
  loadCoverage(file, variant, false);
}

const coverageEl = document.getElementById("coverage");
let coverageShowingAll = false;

async function loadCoverage(file, variant, all) {
  coverageShowingAll = !!all;
  const q = all ? "&all=1" : "";
  const res = await fetch(`/api/coverage?file=${encodeURIComponent(file)}&variant=${encodeURIComponent(variant)}${q}`);
  if (!res.ok) {
    coverageEl.innerHTML = "";
    return;
  }
  const data = await res.json();
  renderCoverage(data);
}

function renderCoverage(data) {
  coverageEl.innerHTML = "";
  if (!data.reports || data.reports.length === 0) {
    return;
  }
  const label = document.createElement("span");
  label.className = "label";
  label.textContent = coverageShowingAll ? "all locales:" : "coverage:";
  coverageEl.appendChild(label);
  for (const r of data.reports) {
    const pct = Math.round(r.coverage * 100);
    const chip = document.createElement("span");
    let cls = "low";
    if (pct === 100) cls = "full";
    else if (pct >= 90) cls = "high";
    else if (pct >= 60) cls = "medium";
    chip.className = `chip ${cls}`;
    chip.innerHTML = `${escapeHTML(r.code)} <span class="pct">${pct}%</span>`;
    let title = `${r.name}: ${r.covered}/${r.total} covered`;
    if (r.missing && r.missing.length) {
      title += `\nmissing: ${r.missing.join(" ")}`;
    }
    chip.title = title;
    coverageEl.appendChild(chip);
  }
  const btn = document.createElement("button");
  btn.className = "toggle-more";
  btn.textContent = coverageShowingAll ? "hint only" : "all locales →";
  btn.addEventListener("click", () => {
    loadCoverage(fileSelect.value, variantSelect.value, !coverageShowingAll);
  });
  coverageEl.appendChild(btn);
}

async function loadCompareCompose() {
  const f = cmpFile.value;
  const v = cmpVariant.value;
  if (!f || !v) return null;
  const res = await fetch(`/api/compose?file=${encodeURIComponent(f)}&variant=${encodeURIComponent(v)}`);
  if (!res.ok) return null;
  return await res.json();
}

function populateCmpVariants() {
  const lf = layouts.find(l => l.file === cmpFile.value);
  cmpVariant.innerHTML = "";
  if (!lf) {
    diffSummary.textContent = "";
    return;
  }
  for (const v of lf.variants) {
    const opt = document.createElement("option");
    opt.value = v.name;
    opt.textContent = `${v.name} — ${v.description}`;
    cmpVariant.appendChild(opt);
  }
  const def = lf.variants.find(v => v.default) || lf.variants[0];
  if (def) cmpVariant.value = def.name;
}

cmpFile.addEventListener("change", () => { populateCmpVariants(); loadCompose(); });
cmpVariant.addEventListener("change", loadCompose);

function updateDiffSummary(a, b) {
  if (!b) { diffSummary.textContent = ""; return; }
  let differing = 0, total = 0, onlyA = 0, onlyB = 0;
  const aKeys = new Set(a.keys || []);
  const bKeys = new Set(b.keys || []);
  for (const k of aKeys) {
    total++;
    if (!bKeys.has(k)) { onlyA++; continue; }
    if (!sameLevels(a.symbols[k], b.symbols[k])) differing++;
  }
  for (const k of bKeys) if (!aKeys.has(k)) onlyB++;
  const parts = [];
  parts.push(`<span class="num">${differing}</span> / ${total} keys differ`);
  if (onlyA) parts.push(`<span class="num">${onlyA}</span> only in A`);
  if (onlyB) parts.push(`<span class="num">${onlyB}</span> only in B`);
  diffSummary.innerHTML = parts.join(" · ");
}

function sameLevels(a, b) {
  if (!a && !b) return true;
  if (!a || !b) return false;
  const n = Math.max(a.length, b.length);
  for (let i = 0; i < n; i++) {
    const av = (a[i] && a[i].value) || "";
    const bv = (b[i] && b[i].value) || "";
    if (av !== bv) return false;
  }
  return true;
}

// === Browse-tab actions ===
bEditBtn.addEventListener("click", async () => {
  const file = fileSelect.value;
  const variant = variantSelect.value;
  const lf = layouts.find(l => l.file === file);
  if (!lf) return;
  const v = lf.variants.find(x => x.name === variant);
  if (!v) return;
  await ensureComposeReady();
  // Prefill the Compose form with this variant's spec.
  cFile.value = file;
  cName.value = v.name;
  cDesc.value = v.description || "";
  if (v.physical) cPhysical.value = v.physical;
  if (v.transformation) cTransformation.value = v.transformation;
  composeAdditions = (v.additions || []).slice();
  composeSubs = (v.substitutions || []).slice();
  cDefault.checked = !!v.default;
  cPassthrough.checked = !!v.passthrough;
  selectedAutofillCats = new Set();
  cAutofillAll.checked = false;
  cAutofillSmart.checked = false;
  if (v.autofill && v.autofill.length) {
    if (v.autofill.includes("smart")) cAutofillSmart.checked = true;
    else if (v.autofill.includes("*")) cAutofillAll.checked = true;
    else for (const c of v.autofill) selectedAutofillCats.add(c);
  }
  renderAutofillCats();
  renderChips(cAddChips, composeAdditions, modules.additions, composeAdditions);
  renderChips(cSubChips, composeSubs, modules.substitutions, composeSubs);
  // Switch to Compose tab.
  switchTab("compose");
  updateComposeControls();
  composeLivePreview();
});

bActivateBtn.addEventListener("click", async () => {
  const file = fileSelect.value;
  const variant = variantSelect.value;
  if (!file || !variant) return;
  if (!confirm(activatePrompt(file, variant))) return;
  bStatus.textContent = "activating…";
  bStatus.className = "status";
  await activateRequest(file, variant, bStatus);
});

// activatePrompt builds the confirm-dialog text and tailors the
// warning for Wayland users — under Wayland, flexkb activate only
// affects Xwayland apps, not the compositor's input subsystem.
function activatePrompt(file, variant) {
  const base = `Activate ${file}(${variant}) in the current session?\n\nRuns flexkb activate (setxkbmap + xkbcomp).`;
  if (activeLayout && activeLayout.sessionType === "wayland") {
    return base + "\n\n⚠ You're on Wayland — this only affects Xwayland apps. Your real keyboard layout is controlled by the Wayland compositor (mutter / kwin / sway / niri / hyprland / …) and won't change. Configure it in your compositor's input settings instead.";
  }
  return base + "\n\nReplaces your current X11 layout until logout.";
}

bYAMLBtn.addEventListener("click", () => openYAMLEditor(fileSelect.value));

const bOpenSettingsBtn = document.getElementById("bOpenSettings");
bOpenSettingsBtn.addEventListener("click", async () => {
  bStatus.textContent = "launching OS settings…";
  bStatus.className = "status";
  const res = await fetch("/api/open-settings", { method: "POST" });
  const data = await res.json();
  if (data.ok) {
    bStatus.textContent = `launched: ${data.used}`;
    bStatus.className = "status ok";
  } else {
    bStatus.textContent = `couldn't launch (compositor=${data.compositor || "?"}): ${data.error}`;
    bStatus.className = "status err";
    bStatus.title = "tried: " + (data.tried || []).join(" / ");
  }
});

// === YAML edit-as-text overlay ===
const yamlOverlay = document.getElementById("yamlOverlay");
const yamlEditor = document.getElementById("yamlEditor");
const yamlTitle = document.getElementById("yamlTitle");
const yamlMeta = document.getElementById("yamlMeta");
const yamlStatus = document.getElementById("yamlStatus");
const yamlSave = document.getElementById("yamlSave");
const yamlReload = document.getElementById("yamlReload");
document.getElementById("yamlClose").addEventListener("click", closeYAMLEditor);
yamlOverlay.addEventListener("click", (e) => {
  if (e.target === yamlOverlay) closeYAMLEditor();
});
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape" && !yamlOverlay.classList.contains("hidden")) closeYAMLEditor();
});

let yamlEditingFile = "";

async function openYAMLEditor(file) {
  yamlEditingFile = file;
  yamlTitle.textContent = `YAML: ${file}.yaml`;
  yamlMeta.textContent = "loading…";
  yamlStatus.textContent = "";
  yamlEditor.value = "";
  const res = await fetch(`/api/file?file=${encodeURIComponent(file)}`);
  if (!res.ok) {
    yamlMeta.textContent = `error: ${await res.text()}`;
    yamlOverlay.classList.remove("hidden");
    return;
  }
  const src = res.headers.get("X-Source-Path") || "";
  const kind = res.headers.get("X-Source-Kind") || "";
  yamlEditor.value = await res.text();
  let m = `${kind}: ${src}`;
  if (kind !== "user") {
    m += "  ·  saving will create a user override at ~/.config/flexkb/data/layouts/" + file + ".yaml";
  }
  yamlMeta.textContent = m;
  yamlOverlay.classList.remove("hidden");
}

function closeYAMLEditor() { yamlOverlay.classList.add("hidden"); }

yamlSave.addEventListener("click", async () => {
  yamlStatus.textContent = "saving…";
  yamlStatus.className = "status";
  const res = await fetch(`/api/file?file=${encodeURIComponent(yamlEditingFile)}`, {
    method: "PUT",
    headers: { "Content-Type": "text/yaml" },
    body: yamlEditor.value,
  });
  if (!res.ok) {
    yamlStatus.textContent = "error: " + await res.text();
    yamlStatus.className = "status err";
    return;
  }
  const data = await res.json();
  yamlStatus.textContent = "saved to " + data.path;
  yamlStatus.className = "status ok";
  await loadLayouts();
});

yamlReload.addEventListener("click", () => openYAMLEditor(yamlEditingFile));

bDeleteBtn.addEventListener("click", async () => {
  const file = fileSelect.value;
  const variant = variantSelect.value;
  if (!file || !variant) return;
  if (!confirm(`Delete ${file}(${variant}) from your user data?\n\nThis edits ~/.config/flexkb/data/layouts/${file}.yaml; the system version is untouched.`)) return;
  bStatus.textContent = "deleting…";
  bStatus.className = "status";
  const res = await fetch(`/api/variant?file=${encodeURIComponent(file)}&variant=${encodeURIComponent(variant)}`, {
    method: "DELETE",
  });
  if (!res.ok) {
    bStatus.textContent = "delete failed: " + await res.text();
    bStatus.className = "status err";
    return;
  }
  const data = await res.json();
  bStatus.textContent = data.fileRemoved ? "deleted (file removed)" : "deleted from " + data.path;
  bStatus.className = "status ok";
  await loadLayouts();
});

bShowXKBBtn.addEventListener("click", async () => {
  const file = fileSelect.value;
  const variant = variantSelect.value;
  if (!file || !variant) return;
  if (!bXkb.classList.contains("hidden") && bXkb.dataset.key === file + "/" + variant) {
    bXkb.classList.add("hidden");
    return;
  }
  const res = await fetch(`/api/xkb?file=${encodeURIComponent(file)}&variant=${encodeURIComponent(variant)}`);
  bXkb.textContent = await res.text();
  bXkb.dataset.key = file + "/" + variant;
  bXkb.classList.remove("hidden");
});

async function activateRequest(file, variant, statusEl) {
  try {
    const res = await fetch("/api/activate", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ file, variant }),
    });
    if (!res.ok) {
      statusEl.textContent = "HTTP " + res.status + ": " + await res.text();
      statusEl.className = "status err";
      return false;
    }
    const data = await res.json();
    if (data.ok) {
      statusEl.textContent = "activated — " + (data.stdout.split("\n")[0] || "");
      statusEl.className = "status ok";
      loadActive();
      return true;
    }
    statusEl.textContent = "activate failed: " + (data.stderr.split("\n")[0] || data.stdout || "(no output)");
    statusEl.className = "status err";
    statusEl.title = data.stderr + "\n---\n" + data.stdout;
    return false;
  } catch (e) {
    statusEl.textContent = "activate failed: " + e.message;
    statusEl.className = "status err";
    return false;
  }
}

function switchTab(name) {
  for (const b of document.querySelectorAll("nav.tabs button")) {
    const active = b.dataset.tab === name;
    b.classList.toggle("active", active);
    // ARIA: tab role consumers (screen readers, AT) rely on
    // aria-selected and tabindex to know which tab is current
    // and which to skip during keyboard navigation. Without
    // these the tablist behaves as a generic button group.
    b.setAttribute("aria-selected", active ? "true" : "false");
    b.setAttribute("tabindex", active ? "0" : "-1");
  }
  for (const s of document.querySelectorAll("section.tab")) {
    const active = s.dataset.tab === name;
    s.classList.toggle("hidden", !active);
    // hidden attribute (vs. just the class) is what assistive
    // tech actually honours — CSS display:none would hide it
    // visually but a screen reader following sequential reading
    // order could still descend into it.
    if (active) {
      s.removeAttribute("hidden");
    } else {
      s.setAttribute("hidden", "");
    }
  }
  if (name === "paths") renderPathsTab();
  if (name === "compose") ensureComposeReady();
}

// Tablist keyboard navigation: Left/Right arrow keys move focus
// between tabs, Home/End jump to first/last. Matches the
// W3C ARIA Authoring Practices for tabs widgets — most desktop
// users expect this behaviour from screen-reader friendly UIs.
document.addEventListener("DOMContentLoaded", () => {
  const tablist = document.querySelector('[role="tablist"]');
  if (!tablist) return;
  tablist.addEventListener("keydown", (e) => {
    const tabs = Array.from(tablist.querySelectorAll('[role="tab"]'));
    const idx = tabs.indexOf(document.activeElement);
    if (idx === -1) return;
    let next = null;
    switch (e.key) {
      case "ArrowRight": next = tabs[(idx + 1) % tabs.length]; break;
      case "ArrowLeft":  next = tabs[(idx - 1 + tabs.length) % tabs.length]; break;
      case "Home":       next = tabs[0]; break;
      case "End":        next = tabs[tabs.length - 1]; break;
      default: return;
    }
    e.preventDefault();
    next.focus();
    next.click();
  });
});

// === Compose tab ===
const cFile = document.getElementById("cFile");
const cName = document.getElementById("cName");
const cDesc = document.getElementById("cDesc");
const cPhysical = document.getElementById("cPhysical");
const cTransformation = document.getElementById("cTransformation");
const cAddPicker = document.getElementById("cAddPicker");
const cAddChips = document.getElementById("cAddChips");
const cAddFilter = document.getElementById("cAddFilter");
const cSubPicker = document.getElementById("cSubPicker");
const cSubChips = document.getElementById("cSubChips");
const cSubFilter = document.getElementById("cSubFilter");
const cSubInverse = document.getElementById("cSubInverse");
const cPreviewBtn = document.getElementById("cPreview");
const cSaveBtn = document.getElementById("cSave");
const cActivateBtn = document.getElementById("cActivate");
const cShowXKBBtn = document.getElementById("cShowXKB");
const cDeleteBtn = document.getElementById("cDelete");
const cStatus = document.getElementById("cStatus");
const cDefault = document.getElementById("cDefault");
const cPassthrough = document.getElementById("cPassthrough");
const cAutofillSmart = document.getElementById("cAutofillSmart");
const cAutofillAll = document.getElementById("cAutofillAll");
const cAutofillCatList = document.getElementById("cAutofillCatList");
const cFillStats = document.getElementById("cFillStats");
let autofillCategories = []; // [{name, fillers}]
let selectedAutofillCats = new Set();
const cMeta = document.getElementById("cMeta");
const cOverrideStats = document.getElementById("cOverrideStats");
const cKb = document.getElementById("cKeyboard");
const cWarn = document.getElementById("cWarnings");
const cXkb = document.getElementById("cXkb");

let composeAdditions = [];
let composeSubs = [];
let modulesLoaded = false;
// composeOverrides: per-cell user assignments accumulated by clicking
// a cell in the Compose grid and picking a glyph in the Unicode picker.
// Keyed as "AB01:2" → "♠"; applied on top of the server response by
// applyComposeOverrides so the live preview reflects the edits without
// requiring a server-side spec change. Save flow can later promote
// these to a synthetic addition.
let composeOverrides = new Map();
let pickerTargetCell = null; // {key, level} when the picker is opened from a cell

async function ensureComposeReady() {
  if (modulesLoaded) {
    updateComposeControls();
    return;
  }
  const res = await fetch("/api/modules");
  modules = await res.json();
  const catRes = await fetch("/api/autofill-categories");
  autofillCategories = await catRes.json();
  renderAutofillCats();
  modulesLoaded = true;
  populateModuleDropdowns();
  updateComposeControls();
  composeLivePreview();
}

function renderAutofillCats() {
  cAutofillCatList.innerHTML = "";
  for (const cat of autofillCategories) {
    const label = document.createElement("label");
    label.className = "inline-check";
    const cb = document.createElement("input");
    cb.type = "checkbox";
    cb.value = cat.name;
    cb.checked = selectedAutofillCats.has(cat.name);
    cb.addEventListener("change", () => {
      if (cb.checked) selectedAutofillCats.add(cat.name);
      else selectedAutofillCats.delete(cat.name);
      cAutofillAll.checked = false;
      composeLivePreview();
    });
    const span = document.createElement("span");
    span.textContent = `${cat.name} (${cat.fillers.length})`;
    span.title = "fillers: " + cat.fillers.join(", ");
    label.appendChild(cb);
    label.appendChild(span);
    cAutofillCatList.appendChild(label);
  }
}

cAutofillAll.addEventListener("change", () => {
  if (cAutofillAll.checked) {
    cAutofillSmart.checked = false;
    selectedAutofillCats.clear();
    for (const cb of cAutofillCatList.querySelectorAll("input")) cb.checked = false;
  }
  composeLivePreview();
});

cAutofillSmart.addEventListener("change", () => {
  if (cAutofillSmart.checked) {
    cAutofillAll.checked = false;
    selectedAutofillCats.clear();
    for (const cb of cAutofillCatList.querySelectorAll("input")) cb.checked = false;
  }
  composeLivePreview();
});

function populateModuleDropdowns() {
  fillModuleSelect(cPhysical, modules.physicals, "ansi");
  fillModuleSelect(cTransformation, modules.transformations, "qwerty");
  fillPicker(cAddPicker, modules.additions);
  fillPicker(cSubPicker, modules.substitutions);
}

function fillModuleSelect(sel, mods, preferred) {
  sel.innerHTML = "";
  for (const m of mods) {
    const opt = document.createElement("option");
    opt.value = m.name;
    opt.textContent = `${m.name} [${m.sourceKind}]${m.description ? " — " + truncate(m.description, 60) : ""}`;
    sel.appendChild(opt);
  }
  if (preferred && mods.some(m => m.name === preferred)) sel.value = preferred;
}

function fillPicker(sel, mods, filterText) {
  const q = (filterText || "").toLowerCase().trim();
  sel.innerHTML = '<option value="">— add —</option>';
  for (const m of mods) {
    if (q && !(m.name.toLowerCase().includes(q) || (m.description || "").toLowerCase().includes(q))) {
      continue;
    }
    const opt = document.createElement("option");
    opt.value = m.name;
    opt.textContent = `${m.name} [${m.sourceKind}]${m.description ? " — " + truncate(m.description, 60) : ""}`;
    sel.appendChild(opt);
  }
}

cAddFilter.addEventListener("input", () => fillPicker(cAddPicker, modules.additions, cAddFilter.value));
cSubFilter.addEventListener("input", () => fillPicker(cSubPicker, modules.substitutions, cSubFilter.value));

cAddPicker.addEventListener("change", () => {
  if (cAddPicker.value) {
    composeAdditions.push(cAddPicker.value);
    cAddPicker.value = "";
    renderChips(cAddChips, composeAdditions, modules.additions, composeAdditions);
    composeLivePreview();
  }
});
cSubPicker.addEventListener("change", () => {
  if (cSubPicker.value) {
    const name = cSubInverse.checked ? "~" + cSubPicker.value : cSubPicker.value;
    composeSubs.push(name);
    cSubPicker.value = "";
    renderChips(cSubChips, composeSubs, modules.substitutions, composeSubs);
    composeLivePreview();
  }
});

function renderChips(container, selected, allModules, listRef) {
  container.innerHTML = "";
  selected.forEach((name, idx) => {
    const mod = allModules.find(m => m.name === name.replace(/^~/, ""));
    const chip = document.createElement("span");
    chip.className = "chip";
    chip.title = "double-click to inspect";
    const label = document.createElement("span");
    label.textContent = name;
    chip.appendChild(label);
    if (mod) {
      const km = document.createElement("span");
      km.className = "layer-mark";
      km.textContent = "[" + mod.sourceKind + "]";
      chip.appendChild(km);
    }
    const x = document.createElement("button");
    x.textContent = "×";
    x.title = "remove";
    x.addEventListener("click", (e) => {
      e.stopPropagation();
      listRef.splice(idx, 1);
      renderChips(container, listRef, allModules, listRef);
      composeLivePreview();
    });
    chip.appendChild(x);
    chip.addEventListener("dblclick", () => {
      const kind = container === cAddChips ? "addition" : "substitution";
      openInspect(kind, name);
    });
    container.appendChild(chip);
  });
}

// === Module inspect overlay ===
const inspectOverlay = document.getElementById("inspectOverlay");
const inspectTitle = document.getElementById("inspectTitle");
const inspectMeta = document.getElementById("inspectMeta");
const inspectDesc = document.getElementById("inspectDesc");
const inspectSummary = document.getElementById("inspectSummary");
const inspectRaw = document.getElementById("inspectRaw");
document.getElementById("inspectClose").addEventListener("click", closeInspect);
inspectOverlay.addEventListener("click", (e) => {
  if (e.target === inspectOverlay) closeInspect();
});
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape" && !inspectOverlay.classList.contains("hidden")) closeInspect();
});

async function openInspect(kind, name) {
  const url = `/api/module?kind=${encodeURIComponent(kind)}&name=${encodeURIComponent(name)}`;
  const res = await fetch(url);
  if (!res.ok) {
    alert("inspect failed: " + await res.text());
    return;
  }
  const data = await res.json();
  inspectTitle.textContent = `${kind}: ${name}`;
  inspectMeta.textContent = `${data.sourceKind} · ${data.sourcePath}`;
  inspectDesc.textContent = data.description || "";
  inspectSummary.innerHTML = "";
  for (const e of (data.summary || []).slice(0, 200)) {
    const tr = document.createElement("tr");
    const tdK = document.createElement("td");
    tdK.className = "k";
    tdK.textContent = e.key;
    const tdV = document.createElement("td");
    tdV.className = "v";
    tdV.textContent = (e.value || []).map(displayValue).join("  ");
    tdV.title = (e.value || []).join("  ");
    const tdN = document.createElement("td");
    tdN.className = "n";
    tdN.textContent = e.note || "";
    tr.appendChild(tdK);
    tr.appendChild(tdV);
    tr.appendChild(tdN);
    inspectSummary.appendChild(tr);
  }
  if ((data.summary || []).length > 200) {
    const tr = document.createElement("tr");
    const td = document.createElement("td");
    td.colSpan = 3;
    td.style.color = "#768390";
    td.textContent = `… ${data.summary.length - 200} more rows in raw YAML below`;
    tr.appendChild(td);
    inspectSummary.appendChild(tr);
  }
  inspectRaw.textContent = data.raw || "";
  inspectOverlay.classList.remove("hidden");
}

function closeInspect() { inspectOverlay.classList.add("hidden"); }

// Picker list-box: double-click an entry to inspect without adding it.
cAddPicker.addEventListener("dblclick", () => {
  if (cAddPicker.value) openInspect("addition", cAddPicker.value);
});
cSubPicker.addEventListener("dblclick", () => {
  if (cSubPicker.value) openInspect("substitution", cSubPicker.value);
});
cPhysical.addEventListener("dblclick", () => {
  if (cPhysical.value) openInspect("physical", cPhysical.value);
});
cTransformation.addEventListener("dblclick", () => {
  if (cTransformation.value) openInspect("transformation", cTransformation.value);
});

cPhysical.addEventListener("change", composeLivePreview);
cTransformation.addEventListener("change", composeLivePreview);
cName.addEventListener("input", composeLivePreview);
cDesc.addEventListener("input", composeLivePreview);
cDefault.addEventListener("change", composeLivePreview);
cPassthrough.addEventListener("change", composeLivePreview);
cPreviewBtn.addEventListener("click", composeLivePreview);

cSaveBtn.addEventListener("click", () => doSave(false));
cActivateBtn.addEventListener("click", () => {
  const file = cFile.value.trim();
  const name = cName.value.trim() || "basic";
  if (!confirm("Save & " + activatePrompt(file, name))) return;
  doSave(true);
});

async function doSave(thenActivate) {
  cStatus.textContent = "saving…";
  cStatus.className = "status";
  const file = cFile.value.trim();
  const spec = composeSpec();
  const body = { file, merge: true, variants: [spec] };
  try {
    const res = await fetch("/api/save", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    if (!res.ok) {
      cStatus.textContent = "save error: " + await res.text();
      cStatus.className = "status err";
      return;
    }
    const data = await res.json();
    cStatus.textContent = "saved to " + data.path;
    cStatus.className = "status ok";
    await loadLayouts();
    if (thenActivate) {
      cStatus.textContent = "activating…";
      cStatus.className = "status";
      await activateRequest(file, spec.name, cStatus);
    }
  } catch (e) {
    cStatus.textContent = "save failed: " + e.message;
    cStatus.className = "status err";
  }
}

cDeleteBtn.addEventListener("click", async () => {
  const file = cFile.value.trim();
  const variant = cName.value.trim() || "basic";
  if (!confirm(`Delete ${file}(${variant}) from your user data?`)) return;
  cStatus.textContent = "deleting…";
  cStatus.className = "status";
  const res = await fetch(`/api/variant?file=${encodeURIComponent(file)}&variant=${encodeURIComponent(variant)}`, {
    method: "DELETE",
  });
  if (!res.ok) {
    cStatus.textContent = "delete failed: " + await res.text();
    cStatus.className = "status err";
    return;
  }
  const data = await res.json();
  cStatus.textContent = data.fileRemoved ? "deleted (file removed)" : "deleted";
  cStatus.className = "status ok";
  await loadLayouts();
  updateComposeControls();
});

// updateComposeControls toggles the Delete button: only shows when
// the current file is a user-layer file AND the variant name matches
// one that exists in it.
function updateComposeControls() {
  const file = cFile.value.trim();
  const name = cName.value.trim();
  const lf = layouts.find(l => l.file === file);
  const present = lf && lf.sourceKind === "user" && lf.variants.some(v => v.name === name);
  cDeleteBtn.classList.toggle("hidden", !present);
  // Save-target hint: where does the YAML land, and is there a
  // system layer being overridden?
  const hint = document.getElementById("cSaveHint");
  if (!file) {
    hint.innerHTML = "";
    return;
  }
  let msg = `Save writes to <code>~/.config/flexkb/data/layouts/${escapeHTML(file)}.yaml</code>`;
  if (lf && lf.sourceKind !== "user") {
    msg += ` — this will <em>shadow</em> the ${lf.sourceKind} version at <code>${escapeHTML(lf.sourcePath)}</code>`;
  } else if (lf && lf.sourceKind === "user") {
    msg += ` — merging into existing user file (${lf.variants.length} variant${lf.variants.length === 1 ? "" : "s"})`;
  }
  hint.innerHTML = msg;
}
cFile.addEventListener("input", updateComposeControls);
cName.addEventListener("input", updateComposeControls);

cShowXKBBtn.addEventListener("click", async () => {
  if (!cXkb.classList.contains("hidden")) {
    cXkb.classList.add("hidden");
    return;
  }
  const res = await fetch("/api/xkb", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(composeSpec()),
  });
  cXkb.textContent = await res.text();
  cXkb.classList.remove("hidden");
});

function composeSpec() {
  let autofill = [];
  if (cAutofillSmart.checked) autofill = ["smart"];
  else if (cAutofillAll.checked) autofill = ["*"];
  else if (selectedAutofillCats.size > 0) autofill = Array.from(selectedAutofillCats);
  return {
    name: cName.value.trim() || "basic",
    description: cDesc.value.trim(),
    physical: cPhysical.value,
    transformation: cTransformation.value,
    additions: composeAdditions.slice(),
    substitutions: composeSubs.slice(),
    default: cDefault.checked,
    passthrough: cPassthrough.checked,
    autofill,
  };
}

async function composeLivePreview() {
  if (!cPhysical.value || !cTransformation.value) return;
  const res = await fetch("/api/compose-spec", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(composeSpec()),
  });
  if (!res.ok) {
    cMeta.textContent = "error: " + await res.text();
    return;
  }
  const data = await res.json();
  applyComposeOverrides(data);
  renderInto({ meta: cMeta, kb: cKb, warn: cWarn }, data);
  updateFillStats(data);
  updateOverrideStats();
}

// applyComposeOverrides mutates the API response in-place so that
// user-clicked cells override the server-computed levels. Stamps the
// source as "override" so the popover/legend show clearly where the
// content came from.
function applyComposeOverrides(data) {
  if (composeOverrides.size === 0) return;
  for (const [coord, glyph] of composeOverrides.entries()) {
    const [key, levelStr] = coord.split(":");
    const lvl = parseInt(levelStr, 10);
    if (!data.symbols[key]) data.symbols[key] = [];
    while (data.symbols[key].length < lvl + 1) {
      data.symbols[key].push({ value: "", source: "" });
    }
    // Encode picked glyph as a single Unicode codepoint string. The
    // xkb writer will turn it into a U<hex> token when saved.
    data.symbols[key][lvl] = { value: glyph, source: "override", module: "you" };
  }
}

function updateOverrideStats() {
  const n = composeOverrides.size;
  if (n === 0) {
    cOverrideStats.classList.add("hidden");
    return;
  }
  cOverrideStats.classList.remove("hidden");
  cOverrideStats.innerHTML = `<strong>${n}</strong> custom cell${n === 1 ? "" : "s"} <button id="cSaveAddition" title="Save these as a reusable addition you can stack on any layout">Save as addition…</button> <button id="cClearOverrides" title="Clear all custom cell assignments">clear</button>`;
  document.getElementById("cClearOverrides").addEventListener("click", () => {
    composeOverrides.clear();
    composeLivePreview();
  });
  document.getElementById("cSaveAddition").addEventListener("click", openSaveAdditionModal);
}

// === Save-as-addition modal ===
const saveAdditionOverlay = document.getElementById("saveAdditionOverlay");
const saName = document.getElementById("saName");
const saDisplay = document.getElementById("saDisplay");
const saDesc = document.getElementById("saDesc");
const saCats = document.getElementById("saCats");
const saFiller = document.getElementById("saFiller");
const saPreview = document.getElementById("saPreview");
const saStatus = document.getElementById("saStatus");
const saSave = document.getElementById("saSave");
document.getElementById("saveAdditionClose").addEventListener("click", closeSaveAdditionModal);
saveAdditionOverlay.addEventListener("click", (e) => {
  if (e.target === saveAdditionOverlay) closeSaveAdditionModal();
});
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape" && !saveAdditionOverlay.classList.contains("hidden")) closeSaveAdditionModal();
});

function openSaveAdditionModal() {
  saveAdditionOverlay.classList.remove("hidden");
  saName.value = "";
  saDisplay.value = "";
  saDesc.value = "";
  saCats.value = "";
  saFiller.checked = false;
  saStatus.textContent = "";
  saStatus.className = "status";
  // Build the preview from composeOverrides — group by key, sort.
  const byKey = new Map();
  for (const [coord, glyph] of composeOverrides.entries()) {
    const [key, levelStr] = coord.split(":");
    const lvl = parseInt(levelStr, 10);
    if (!byKey.has(key)) byKey.set(key, []);
    byKey.get(key).push({ lvl, glyph });
  }
  saPreview.innerHTML = "";
  if (byKey.size === 0) {
    saPreview.innerHTML = '<div class="empty">no overrides to save</div>';
  } else {
    const keys = [...byKey.keys()].sort();
    for (const k of keys) {
      const slots = byKey.get(k).sort((a, b) => a.lvl - b.lvl);
      // Build a "L1=♠ L3=♣" style summary.
      const parts = slots.map(s => `L${s.lvl + 1}=${s.glyph}`).join("  ");
      const row = document.createElement("div");
      row.className = "row";
      row.textContent = `${k}: ${parts}`;
      saPreview.appendChild(row);
    }
  }
  saName.focus();
}

function closeSaveAdditionModal() { saveAdditionOverlay.classList.add("hidden"); }

saSave.addEventListener("click", async () => {
  const name = saName.value.trim().toLowerCase();
  if (!name) {
    saStatus.textContent = "slug required";
    saStatus.className = "status err";
    return;
  }
  if (!/^[a-z0-9_.-]+$/.test(name)) {
    saStatus.textContent = "slug must be a-z 0-9 _-.";
    saStatus.className = "status err";
    return;
  }
  // Build overlays: { KEY: { levels: [glyph_at_L1, glyph_at_L2, …] } }.
  // Missing levels filled with "" so the addition merges cleanly.
  const byKey = new Map();
  for (const [coord, glyph] of composeOverrides.entries()) {
    const [key, levelStr] = coord.split(":");
    const lvl = parseInt(levelStr, 10);
    if (!byKey.has(key)) byKey.set(key, []);
    const arr = byKey.get(key);
    while (arr.length < lvl + 1) arr.push("");
    arr[lvl] = toXKBToken(glyph);
  }
  const overlays = {};
  for (const [k, arr] of byKey.entries()) {
    overlays[k] = { levels: arr };
  }
  const body = {
    name,
    displayName: saDisplay.value.trim() || name,
    description: saDesc.value.trim(),
    categories: saCats.value.split(",").map(s => s.trim()).filter(Boolean),
    filler: saFiller.checked,
    scripts: ["any"],
    overlays,
    letterOverlays: {},
  };
  saStatus.textContent = "saving…";
  saStatus.className = "status";
  try {
    const res = await fetch("/api/save-addition", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    if (!res.ok) {
      saStatus.textContent = "save failed: " + await res.text();
      saStatus.className = "status err";
      return;
    }
    const data = await res.json();
    saStatus.textContent = `saved to ${data.path}`;
    saStatus.className = "status ok";
    // Refresh module list, add the new addition to the current
    // recipe, and clear the in-flight overrides (they're now baked
    // into the addition).
    const modRes = await fetch("/api/modules");
    modules = await modRes.json();
    populateModuleDropdowns();
    if (!composeAdditions.includes(data.slug)) {
      composeAdditions.push(data.slug);
      renderChips(cAddChips, composeAdditions, modules.additions, composeAdditions);
    }
    composeOverrides.clear();
    composeLivePreview();
    setTimeout(closeSaveAdditionModal, 800);
  } catch (e) {
    saStatus.textContent = "save failed: " + e.message;
    saStatus.className = "status err";
  }
});

// toXKBToken converts a JS string of one Unicode character to the
// xkb keysym form (U<hex>) so saved YAML matches the project's
// convention. ASCII letters / digits / single-byte punctuation
// pass through unchanged.
function toXKBToken(glyph) {
  if (!glyph) return "";
  const cp = glyph.codePointAt(0);
  if (cp < 0x80) {
    // ASCII letters/digits/symbols — keep as-is; xkb accepts them.
    if ((cp >= 0x30 && cp <= 0x39) || (cp >= 0x41 && cp <= 0x5A) || (cp >= 0x61 && cp <= 0x7A)) {
      return glyph;
    }
  }
  return "U" + cp.toString(16).toUpperCase().padStart(4, "0");
}

function updateFillStats(data) {
  // Count autofill-sourced cells per filler slug.
  const byFiller = {};
  let total = 0;
  for (const v of Object.values(data.symbols || {})) {
    for (const c of v) {
      if (c.source === "autofill") {
        total++;
        const m = c.module || "(unknown)";
        byFiller[m] = (byFiller[m] || 0) + 1;
      }
    }
  }
  if (total === 0) {
    cFillStats.textContent = "";
    cFillStats.classList.remove("has-fills");
    return;
  }
  cFillStats.classList.add("has-fills");
  const detail = Object.entries(byFiller)
    .sort((a, b) => b[1] - a[1])
    .map(([m, n]) => `${m}: ${n}`)
    .join(", ");
  cFillStats.innerHTML = `${total} levels autofilled <span class="by-cat">(${escapeHTML(detail)})</span>`;
}

// === Paths tab ===
async function renderPathsTab() {
  const pathsRes = await fetch("/api/paths");
  const paths = await pathsRes.json();
  const list = document.getElementById("pathsList");
  list.innerHTML = "";
  for (const p of paths) {
    const li = document.createElement("li");
    li.textContent = p.path;
    const k = document.createElement("span");
    k.className = "kind layer-tag " + p.kind;
    k.textContent = p.kind;
    li.appendChild(k);
    list.appendChild(li);
  }
  await ensureComposeReady();
  const panel = document.getElementById("modulesPanel");
  panel.innerHTML = "";
  for (const [title, items] of [
    ["Physicals", modules.physicals],
    ["Transformations", modules.transformations],
    ["Additions", modules.additions],
    ["Substitutions", modules.substitutions],
  ]) {
    panel.appendChild(renderModulesCard(title, items));
  }
}

function renderModulesCard(title, items) {
  // Map title to the kind=… query the inspect API expects.
  const kindByTitle = {
    "Physicals": "physical",
    "Transformations": "transformation",
    "Additions": "addition",
    "Substitutions": "substitution",
  };
  const kind = kindByTitle[title];
  const card = document.createElement("div");
  card.className = "modules-card";
  const h = document.createElement("h4");
  h.textContent = `${title} (${items.length})`;
  card.appendChild(h);
  const ul = document.createElement("ul");
  for (const m of items) {
    const li = document.createElement("li");
    const k = document.createElement("span");
    k.className = `mod-kind ${m.sourceKind}`;
    k.textContent = m.sourceKind;
    li.appendChild(k);
    const name = document.createElement("a");
    name.href = "#";
    name.className = "name-link";
    name.textContent = m.name;
    name.addEventListener("click", (e) => {
      e.preventDefault();
      openInspect(kind, m.name);
    });
    li.appendChild(name);
    if (m.description) {
      const d = document.createElement("span");
      d.style.color = "#768390";
      d.textContent = " — " + truncate(m.description, 80);
      li.appendChild(d);
    }
    ul.appendChild(li);
  }
  card.appendChild(ul);
  return card;
}

// === Shared rendering ===
function renderInto(targets, data, compareData) {
  const { meta, kb, warn } = targets;
  const recipeBits = [];
  if (data.physical) recipeBits.push(`physical: ${data.physical}`);
  if (data.transformation) recipeBits.push(`transformation: ${data.transformation}`);
  if (data.additions && data.additions.length) recipeBits.push(`additions: [${data.additions.join(", ")}]`);
  if (data.substitutions && data.substitutions.length) recipeBits.push(`substitutions: [${data.substitutions.join(", ")}]`);
  if (data.passthrough) recipeBits.push("passthrough: true");
  meta.innerHTML = `
    <div class="desc">${escapeHTML(data.description || data.variant || "")}</div>
    <div class="recipe">${recipeBits.map(escapeHTML).join("  •  ")}</div>
  `;

  if (data.warnings && data.warnings.length) {
    warn.classList.add("show");
    warn.innerHTML = `<strong>${data.warnings.length} warning(s):</strong><ul>${
      data.warnings.map(w => `<li>${escapeHTML(w)}</li>`).join("")
    }</ul>`;
  } else {
    warn.classList.remove("show");
    warn.innerHTML = "";
  }

  kb.innerHTML = "";
  lastCompose.set(kb, data);
  // Store the compare side too so the click popover can use it.
  if (compareData) lastCompose.set(kb, { ...data, _compare: compareData });
  const presentKeys = new Set(data.keys || []);
  for (const row of ROWS) {
    const rowEl = document.createElement("div");
    rowEl.className = "kbrow";
    for (const code of row) {
      if (!presentKeys.has(code)) continue;
      const cmpLevels = compareData ? (compareData.symbols[code] || null) : null;
      rowEl.appendChild(renderKey(code, data.symbols[code] || [], cmpLevels));
    }
    if (rowEl.children.length) kb.appendChild(rowEl);
  }
}

function renderKey(code, levels, cmpLevels) {
  const el = document.createElement("div");
  el.className = "key";
  el.dataset.code = code;
  // Diff outline if a compare-side is supplied and any level differs
  // (or the compare side is missing this key entirely).
  if (cmpLevels === undefined) {
    // no compare mode — no outline
  } else if (cmpLevels === null) {
    el.classList.add("diff-add"); // present on A, absent on B
  } else if (!sameLevels(levels, cmpLevels)) {
    el.classList.add("diff");
  }
  const codeEl = document.createElement("span");
  codeEl.className = "code";
  codeEl.textContent = code;
  el.appendChild(codeEl);
  for (let i = 0; i < 4; i++) {
    const lvl = levels[i] || { value: "", source: "" };
    const c = document.createElement("span");
    c.className = `cell l${i} ${lvl.source ? "src-" + lvl.source : "src-empty"}`;
    c.textContent = displayValue(lvl.value);
    c.title = lvl.value
      ? `level ${i+1} (${lvl.source || "—"}${lvl.module ? " · " + lvl.module : ""}): ${lvl.value}`
      : `level ${i+1} (empty)`;
    el.appendChild(c);
  }
  el.addEventListener("click", (e) => {
    e.stopPropagation();
    const kbContainer = el.closest("section.tab")?.querySelector(".keyboard");
    const data = kbContainer ? lastCompose.get(kbContainer) : null;
    const cmp = data && data._compare ? data._compare : null;
    openKeyPopover(el, code, levels, data, cmpLevels, cmp);
  });
  // Per-cell click (not the whole key) opens the Unicode picker for
  // that exact (key, level) — but only on the Compose tab where the
  // user authoring flow lives. Browse-tab cells go through the
  // provenance popover only.
  for (let i = 0; i < el.children.length; i++) {
    const child = el.children[i];
    if (!child.classList || !child.classList.contains("cell")) continue;
    const levelIdx = Array.from(child.classList).map(c => /^l(\d+)$/.exec(c)).find(Boolean);
    if (!levelIdx) continue;
    const lvl = parseInt(levelIdx[1], 10);
    child.addEventListener("dblclick", (e) => {
      e.stopPropagation();
      // Only fire in the Compose tab.
      const tab = el.closest("section.tab");
      if (!tab || tab.dataset.tab !== "compose") return;
      openUnicodePickerForCell(code, lvl);
    });
  }
  return el;
}

// === Per-key provenance popover ===
const keyPopover = document.getElementById("keyPopover");

function openKeyPopover(anchorEl, code, levels, data, cmpLevels, cmp) {
  if (!data) return;
  keyPopover.innerHTML = "";
  const h = document.createElement("h4");
  h.textContent = code;
  keyPopover.appendChild(h);
  for (let i = 0; i < 4; i++) {
    const lvl = levels[i] || { value: "", source: "" };
    const row = document.createElement("div");
    row.className = "lvl";
    const num = document.createElement("span");
    num.className = "num";
    num.textContent = `L${i+1}`;
    row.appendChild(num);
    const body = document.createElement("div");
    body.className = "body";
    if (!lvl.value) {
      const p = document.createElement("span");
      p.style.color = "var(--fg-faint)";
      p.textContent = "(empty — pass-through)";
      body.appendChild(p);
    } else {
      const g = document.createElement("span");
      g.className = "glyph";
      g.textContent = displayValue(lvl.value);
      body.appendChild(g);
      const t = document.createElement("span");
      t.className = "token";
      t.textContent = lvl.value;
      body.appendChild(t);
      if (lvl.source) {
        const s = document.createElement("div");
        s.className = "src src-" + lvl.source;
        s.textContent = sourceLabel(lvl.source) + (lvl.module ? ` · ${lvl.module}` : "");
        body.appendChild(s);
      }
    }
    row.appendChild(body);
    keyPopover.appendChild(row);
  }
  // Recipe footer — show which additions/substitutions are in play.
  const recipe = document.createElement("div");
  recipe.className = "recipe";
  const bits = [];
  if (data.transformation) bits.push(`transformation: <span class="dim">${escapeHTML(data.transformation)}</span>`);
  if (data.additions && data.additions.length) bits.push(`additions: <span class="dim">[${data.additions.map(escapeHTML).join(", ")}]</span>`);
  if (data.substitutions && data.substitutions.length) bits.push(`substitutions: <span class="dim">[${data.substitutions.map(escapeHTML).join(", ")}]</span>`);
  recipe.innerHTML = bits.join("<br>");
  keyPopover.appendChild(recipe);

  // Compare side (if active) — show what the other variant has for the same key.
  if (cmpLevels !== undefined) {
    const cmpDiv = document.createElement("div");
    cmpDiv.className = "compare-side";
    const h5 = document.createElement("h5");
    if (cmpLevels === null) {
      h5.textContent = `Compare: ${cmp?.file || cmpFile.value}(${cmp?.variant || cmpVariant.value}) — key not present`;
      cmpDiv.appendChild(h5);
    } else {
      h5.textContent = `Compare: ${cmp?.file || cmpFile.value}(${cmp?.variant || cmpVariant.value})`;
      cmpDiv.appendChild(h5);
      for (let i = 0; i < 4; i++) {
        const av = (levels[i] && levels[i].value) || "";
        const bv = (cmpLevels[i] && cmpLevels[i].value) || "";
        const row = document.createElement("div");
        row.className = "lvl";
        const num = document.createElement("span");
        num.className = "num";
        num.textContent = `L${i+1}`;
        row.appendChild(num);
        const body = document.createElement("div");
        body.className = "body";
        const txt = av === bv ? "≡ same" : (bv ? `${displayValue(bv)}  ${bv}` : "(empty)");
        body.textContent = txt;
        if (av !== bv) body.style.color = "var(--warn-fg)";
        row.appendChild(body);
        cmpDiv.appendChild(row);
      }
    }
    keyPopover.appendChild(cmpDiv);
  }

  // Position the popover near the clicked key, clamping to viewport.
  const rect = anchorEl.getBoundingClientRect();
  const scrollY = window.scrollY || document.documentElement.scrollTop;
  const scrollX = window.scrollX || document.documentElement.scrollLeft;
  keyPopover.classList.remove("hidden");
  // Render to measure.
  const ph = keyPopover.offsetHeight;
  const pw = keyPopover.offsetWidth;
  let top = rect.bottom + scrollY + 4;
  let left = rect.left + scrollX;
  // If it would overflow bottom, place above the key instead.
  if (rect.bottom + ph + 12 > window.innerHeight) {
    top = rect.top + scrollY - ph - 4;
  }
  if (left + pw + 12 > window.innerWidth + scrollX) {
    left = window.innerWidth + scrollX - pw - 12;
  }
  keyPopover.style.top = top + "px";
  keyPopover.style.left = left + "px";
}

function closeKeyPopover() { keyPopover.classList.add("hidden"); }

function sourceLabel(s) {
  return { transformation: "transformation",
           position: "positional overlay",
           letter: "letter overlay",
           substitution: "substitution" }[s] || s;
}

document.addEventListener("click", (e) => {
  if (keyPopover.classList.contains("hidden")) return;
  if (!keyPopover.contains(e.target)) closeKeyPopover();
});
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape" && !keyPopover.classList.contains("hidden")) closeKeyPopover();
});

function displayValue(v) {
  if (!v) return "";
  const m = /^U([0-9A-Fa-f]{4,6})$/.exec(v);
  if (m) {
    try { return String.fromCodePoint(parseInt(m[1], 16)); } catch (e) { return v; }
  }
  if (v.length <= 3) return v;
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
  return v.length > 7 ? v.slice(0, 6) + "…" : v;
}

function escapeHTML(s) {
  return String(s).replace(/[&<>"']/g, c => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;"
  }[c]));
}

function truncate(s, n) {
  if (!s) return "";
  return s.length > n ? s.slice(0, n - 1) + "…" : s;
}

fileSelect.addEventListener("change", populateVariants);
variantSelect.addEventListener("change", loadCompose);
loadLayouts();
loadActive();

const activeChip = document.getElementById("activeChip");

async function loadActive() {
  try {
    const res = await fetch("/api/active");
    if (!res.ok) return;
    const a = await res.json();
    if (a.ok && a.layout) {
      activeLayout = a;
      const v = a.variant || "basic";
      const sess = a.sessionType ? ` · ${a.sessionType}` : "";
      activeChip.textContent = `live: ${a.layout}(${v})${sess}`;
      activeChip.title = `source: ${a.source}` + (a.model ? ` · model: ${a.model}` : "") + (a.sessionType === "wayland" ? "\nWayland session — Activate affects Xwayland apps only" : "");
      activeChip.classList.toggle("warn", a.sessionType === "wayland");
      activeChip.classList.remove("hidden");
    } else {
      activeLayout = a;
      activeChip.classList.add("hidden");
    }
    // Show the "OS keyboard settings" button only on Wayland — that's
    // the session type where Activate genuinely can't change the real
    // input layout, so we redirect users to the compositor's own panel.
    if (a.sessionType === "wayland" && a.compositor) {
      bOpenSettingsBtn.classList.remove("hidden");
      bOpenSettingsBtn.title = `Open the ${a.compositor} keyboard panel (Wayland's real layout selection lives there, not in xkb)`;
    } else {
      bOpenSettingsBtn.classList.add("hidden");
    }
    // Re-decorate the variant picker if it's already rendered.
    decorateActiveInPicker();
  } catch (e) {}
}

function decorateActiveInPicker() {
  if (!activeLayout) return;
  for (const opt of variantSelect.options) {
    opt.textContent = opt.textContent.replace(/ \[live\]$/, "");
    if (fileSelect.value === activeLayout.layout && opt.value === (activeLayout.variant || "basic")) {
      opt.textContent = opt.textContent + " [live]";
    }
  }
}
