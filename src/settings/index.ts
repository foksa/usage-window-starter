import Electrobun, { Electroview } from "electrobun/view";
import type { SettingsRPC } from "../shared/rpc";
import type { Config, ModelOption, Provider } from "../shared/types";

const rpc = Electroview.defineRPC<SettingsRPC>({
  maxRequestTime: 180_000,
  handlers: { requests: {}, messages: {} },
});
const electrobun = new Electrobun.Electroview({ rpc });
const api = () => electrobun.rpc!;

const CUSTOM = "__custom__";
const $ = <T extends HTMLElement>(id: string) => document.getElementById(id) as T;
const input = (id: string) => $<HTMLInputElement>(id);
const select = (id: string) => $<HTMLSelectElement>(id);

let config: Config;

function setStatus(text: string, cls = "") {
  const el = $("status");
  el.textContent = text;
  el.className = cls;
}

/** Fill a model <select>; a saved model the CLI no longer offers stays selectable but flagged. */
function fillModels(p: Provider, models: ModelOption[], current: string): boolean {
  const sel = select(`${p}-model`);
  sel.innerHTML = "";
  const known = models.some((m) => m.id === current);
  for (const m of models) {
    sel.add(new Option(m.label + (m.isDefault ? " (default)" : ""), m.id, false, m.id === current));
  }
  if (!known && current && models.length) {
    const opt = new Option(`${current} — not offered anymore`, current, false, true);
    sel.add(opt, 0);
  }
  sel.add(new Option("Custom…", CUSTOM));
  if (!models.length) sel.value = CUSTOM;
  input(`${p}-custom`).value = sel.value === CUSTOM ? current : "";
  syncCustomRow(p);
  return known;
}

function syncCustomRow(p: Provider) {
  $(`${p}-custom-row`).hidden = select(`${p}-model`).value !== CUSTOM;
}

function modelValue(p: Provider): string {
  const v = select(`${p}-model`).value;
  return (v === CUSTOM ? input(`${p}-custom`).value : v).trim();
}

async function loadCodexModels() {
  const hint = $("codex-models-hint");
  hint.textContent = "Loading models from Codex…";
  hint.className = "";
  const current = config.codex.model;
  const res = await api().request.listCodexModels({ bin: input("codex-bin").value.trim() });
  if ("error" in res) {
    fillModels("codex", [], current);
    hint.textContent = `Couldn't list models: ${res.error.slice(0, 80)}`;
    hint.className = "err";
    return;
  }
  const known = fillModels("codex", res.models, current);
  hint.textContent = known ? "From your installed Codex" : `"${current}" isn't offered by your Codex anymore — pick another`;
  hint.className = known ? "" : "warn";
}

function syncDisabled() {
  for (const p of ["claude", "codex"] as Provider[]) {
    document.querySelector(`section[data-provider="${p}"]`)!.classList.toggle("disabled", !input(`${p}-enabled`).checked);
  }
  $("activeHoursRow").hidden = !input("activeHoursOn").checked;
}

function readForm(): Config {
  return {
    ...config,
    autoStart: input("autoStart").checked,
    intervalMin: Math.max(1, Math.min(120, Math.round(Number(input("intervalMin").value) || 10))),
    activeHours: input("activeHoursOn").checked
      ? { start: input("ahStart").value || "08:00", end: input("ahEnd").value || "23:59" }
      : null,
    refreshOnOpenSec: Math.max(0, Math.min(3600, Math.round(Number(input("refreshOnOpenSec").value) || 0))),
    trayShowTimes: input("trayShowTimes").checked,
    keepPanelLoaded: input("keepPanelLoaded").checked,
    claude: {
      ...config.claude,
      enabled: input("claude-enabled").checked,
      model: modelValue("claude"),
      bin: input("claude-bin").value.trim(),
    },
    codex: {
      ...config.codex,
      enabled: input("codex-enabled").checked,
      model: modelValue("codex"),
      bin: input("codex-bin").value.trim(),
      reasoningEffort: select("codex-effort").value,
    },
  };
}

function validate(c: Config): string | null {
  for (const p of ["claude", "codex"] as Provider[]) {
    if (c[p].enabled && !c[p].model) return `Pick a ${p === "claude" ? "Claude" : "Codex"} model`;
    if (c[p].enabled && !c[p].bin) return `Set the ${p} CLI path`;
  }
  return null;
}

async function save() {
  const next = readForm();
  const problem = validate(next);
  if (problem) return setStatus(problem, "err");
  $<HTMLButtonElement>("save").disabled = true;
  const res = await api().request.saveSettings({ config: next, launchAtLogin: input("launchAtLogin").checked });
  $<HTMLButtonElement>("save").disabled = false;
  if (!res.ok) return setStatus(res.error, "err");
  config = next;
  setStatus("Saved — checking now", "ok");
}

async function testModel(p: Provider) {
  const btn = document.querySelector<HTMLButtonElement>(`button[data-test="${p}"]`)!;
  const out = $(`${p}-test-result`);
  const cfg = readForm();
  if (!cfg[p].model) {
    out.textContent = "Pick a model first";
    out.className = "test-result err";
    return;
  }
  btn.disabled = true;
  out.textContent = `Sending to ${cfg[p].model}…`;
  out.className = "test-result";
  const res = await api().request.testModel({ provider: p, config: cfg });
  btn.disabled = false;
  out.textContent = res.ok ? `✓ ${cfg[p].model} replied "${res.reply}"` : `✗ ${res.error}`;
  out.className = `test-result ${res.ok ? "ok" : "err"}`;
}

async function init() {
  const s = await api().request.getSettings({});
  config = s.config;

  input("autoStart").checked = config.autoStart;
  input("intervalMin").value = String(config.intervalMin);
  input("refreshOnOpenSec").value = String(config.refreshOnOpenSec);
  input("trayShowTimes").checked = config.trayShowTimes;
  input("keepPanelLoaded").checked = config.keepPanelLoaded;
  input("activeHoursOn").checked = !!config.activeHours;
  input("ahStart").value = config.activeHours?.start ?? "08:00";
  input("ahEnd").value = config.activeHours?.end ?? "23:59";
  input("launchAtLogin").checked = s.launchAtLogin;
  $("version").textContent = `Version ${s.appVersion}`;
  input("launchAtLogin").disabled = !s.canLaunchAtLogin;
  $("loginHint").textContent = s.canLaunchAtLogin
    ? "Open the app in the background when you log in"
    : "Available when running the built app";

  input("claude-enabled").checked = config.claude.enabled;
  input("claude-bin").value = config.claude.bin;
  fillModels("claude", s.claudeModels, config.claude.model);

  input("codex-enabled").checked = config.codex.enabled;
  input("codex-bin").value = config.codex.bin;
  select("codex-effort").value = config.codex.reasoningEffort;
  fillModels("codex", [], config.codex.model);

  syncDisabled();
  $("app").hidden = false;
  void loadCodexModels();
}

document.addEventListener("change", (e) => {
  const id = (e.target as HTMLElement).id;
  if (id === "claude-model") syncCustomRow("claude");
  if (id === "codex-model") syncCustomRow("codex");
  if (id === "codex-bin") void loadCodexModels();
  syncDisabled();
  setStatus("Unsaved changes");
});
$("codex-refresh").addEventListener("click", () => void loadCodexModels());
$("save").addEventListener("click", () => void save());
$("close").addEventListener("click", () => api().send.closeSettings({}));
document.querySelectorAll<HTMLButtonElement>("button[data-test]").forEach((b) =>
  b.addEventListener("click", () => void testModel(b.dataset.test as Provider)),
);
document.addEventListener("keydown", (e) => {
  if ((e.metaKey || e.ctrlKey) && e.key === "s") {
    e.preventDefault();
    void save();
  }
  if (e.key === "Escape") api().send.closeSettings({});
});

void init();
