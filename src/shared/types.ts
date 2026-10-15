// Shapes the Go backend sends to the views. Keep in sync with src/go/core.

export type Provider = "claude" | "codex";

export interface LimitWindow {
  /** true while a window is running (it has a reset time in the future) */
  active: boolean;
  usedPct: number;
  /** epoch ms, null when idle or unknown */
  resetsAt: number | null;
}

export interface Config {
  /** master switch for sending start messages; checks keep running when off */
  autoStart: boolean;
  intervalMin: number;
  /** e.g. { start: "08:00", end: "24:00" }; null = always */
  activeHours: { start: string; end: string } | null;
  /** opening the panel checks providers whose last check is older than this; 0 = never */
  refreshOnOpenSec: number;
  /** show time left in the menu bar next to the icon */
  trayShowTimes: boolean;
  /** keep the panel's web view (~40 MB) while hidden, so it opens at once */
  keepPanelLoaded: boolean;
  claude: { enabled: boolean; bin: string; model: string };
  codex: { enabled: boolean; bin: string; model: string; reasoningEffort: string };
}

export interface ModelOption {
  id: string;
  label: string;
  isDefault?: boolean;
}

export type UpdatePhase = "idle" | "checking" | "downloading" | "ready" | "installing" | "error";
