import type { ElectrobunConfig } from "electrobun";

export default {
  app: {
    name: "Usage Window Starter",
    identifier: "dev.foksa.usage-window-starter",
    version: "0.3.7",
  },
  runtime: {
    // It's a menu bar app: closing the settings window must not quit it.
    exitOnLastWindowClosed: false,
  },
  build: {
    mainProcess: "go",
    go: {
      package: "./src/go",
    },
    views: {
      settings: {
        entrypoint: "src/settings/index.ts",
      },
      panel: {
        entrypoint: "src/panel/index.ts",
      },
    },
    copy: {
      "src/settings/index.html": "views/settings/index.html",
      "src/settings/index.css": "views/settings/index.css",
      "src/panel/index.html": "views/panel/index.html",
      "src/panel/index.css": "views/panel/index.css",
      "src/panel/logos/claude.svg": "views/panel/logos/claude.svg",
      "src/panel/logos/openai.svg": "views/panel/logos/openai.svg",
      "assets/tray-template.png": "views/assets/tray-template.png",
    },
    mac: {
      bundleCEF: false,
      createDmg: true,
    },
  },
  release: {
    // Where the app looks for updates; scripts/release.sh uploads each build here.
    baseUrl: "https://github.com/foksa/usage-window-starter/releases/latest/download",
  },
} satisfies ElectrobunConfig;
