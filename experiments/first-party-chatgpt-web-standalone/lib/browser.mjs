// Browser ownership module: the spike itself locates the Chrome/Chromium
// binary, creates the dedicated profile directory, launches the browser
// process, discovers the CDP endpoint (DevToolsActivePort file), discovers
// targets, attaches to the ChatGPT page session, and finally terminates the
// browser. None of these steps involve Orca, JCode or any other agent
// runtime.

import { spawn } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";

import { Cdp } from "./cdp.mjs";

const BIN_CANDIDATES = [
  "google-chrome-stable",
  "google-chrome",
  "chromium",
  "chromium-browser",
];

export function findChrome() {
  // Explicit env override wins; otherwise discover via PATH candidates.
  const envBin = process.env.RUNSTEAD_SPIKE_CHROME;
  if (envBin && fs.existsSync(envBin)) {
    return { bin: envBin, discoveredBy: "RUNSTEAD_SPIKE_CHROME env" };
  }
  for (const name of BIN_CANDIDATES) {
    try {
      const out = execFileSync("which", [name], { encoding: "utf8" }).trim();
      if (out && fs.existsSync(out)) {
        return { bin: out, discoveredBy: `which ${name}` };
      }
    } catch {
      // not on PATH, try next candidate
    }
  }
  throw new Error(
    "no chrome/chromium binary found; install one or set RUNSTEAD_SPIKE_CHROME"
  );
}

function waitFor(fn, timeoutMs, intervalMs) {
  return new Promise((resolve, reject) => {
    const start = Date.now();
    const tick = () => {
      let value;
      try {
        value = fn();
      } catch (err) {
        reject(err);
        return;
      }
      if (value) {
        resolve(value);
        return;
      }
      if (Date.now() - start > timeoutMs) {
        reject(new Error("waitFor timeout"));
        return;
      }
      setTimeout(tick, intervalMs);
    };
    tick();
  });
}

// Kill any leftover browser process that still owns THIS dedicated profile
// (from a previous crashed run). Matches only our exact user-data-dir flag;
// never touches any other profile or process.
function killStaleOwners(profileDir) {
  const marker = `--user-data-dir=${profileDir}`;
  let psOut = "";
  try {
    psOut = execFileSync("ps", ["-eo", "pid=,args="], { encoding: "utf8" });
  } catch {
    return;
  }
  for (const line of psOut.split("\n")) {
    if (!line.includes(marker)) continue;
    if (line.includes("grep") || line.includes("ps -eo")) continue;
    const pid = Number.parseInt(line.trim().split(/\s+/)[0], 10);
    if (Number.isInteger(pid) && pid > 0) {
      try {
        process.kill(pid, "SIGTERM");
      } catch {
        // already gone
      }
    }
  }
}

// Launch Chrome with a profile directory owned exclusively by this spike.
// Returns process handle, the CDP port discovered from DevToolsActivePort,
// and whether the profile directory was created fresh by us.
//
// `remoteDebugging: false` launches a clean browser (no debugging flags) for
// the enrollment/login phase: measured transport property, plain launches
// pass OpenAI's Cloudflare/Auth0 login gate while debugging-flagged launches
// are challenged. The runtime phase always uses the debugging launch.
export async function launchBrowser(profileDir, { remoteDebugging = true, url = "about:blank" } = {}) {
  const info = findChrome();
  const existedBefore = fs.existsSync(profileDir);
  fs.mkdirSync(profileDir, { recursive: true });
  const fresh = !existedBefore || fs.readdirSync(profileDir).length === 0;
  killStaleOwners(profileDir);
  // Let the previous owner finish shutting down, then drop its stale
  // DevToolsActivePort so the port we read is guaranteed to be the new
  // browser's.
  await new Promise((r) => setTimeout(r, 1500));
  try {
    fs.rmSync(path.join(profileDir, "DevToolsActivePort"), { force: true });
  } catch {
    // ignore
  }

  const args = [
    `--user-data-dir=${profileDir}`,
    "--no-first-run",
    "--no-default-browser-check",
  ];
  if (remoteDebugging) {
    args.push("--remote-debugging-port=0"); // OS-assigned port, discovered via DevToolsActivePort
  }
  args.push(url);

  const proc = spawn(info.bin, args, { stdio: "ignore" });

  let port = null;
  if (remoteDebugging) {
    const devtoolsFile = path.join(profileDir, "DevToolsActivePort");
    port = await waitFor(
      () => {
        if (proc.exitCode !== null) {
          throw new Error(`chrome exited early with code ${proc.exitCode}`);
        }
        try {
          const content = fs.readFileSync(devtoolsFile, "utf8").split("\n");
          const p = Number.parseInt(content[0], 10);
          if (Number.isInteger(p) && p > 0) return p;
        } catch {
          // file not written yet
        }
        return null;
      },
      30000,
      200
    );
  }

  return { proc, port, info, fresh };
}

async function fetchJson(url) {
  // The DevToolsActivePort file appears before the HTTP endpoint answers;
  // retry briefly.
  let lastErr = null;
  for (let i = 0; i < 10; i++) {
    try {
      const res = await fetch(url);
      if (res.ok) return res.json();
      lastErr = new Error(`HTTP ${res.status}`);
    } catch (err) {
      lastErr = err;
    }
    await new Promise((r) => setTimeout(r, 300));
  }
  throw lastErr || new Error(`GET ${url} failed`);
}

// Browser-level CDP connection + version (sanitized to browser name only).
// The websocket endpoint URL is obtained by the spike itself from the
// browser's /json/version and is never logged.
export async function connectBrowser(port) {
  const version = await fetchJson(`http://127.0.0.1:${port}/json/version`);
  const targets = await fetchJson(`http://127.0.0.1:${port}/json/list`);
  const cdp = new Cdp(version.webSocketDebuggerUrl);
  await cdp.connect();
  await cdp.send("Target.setDiscoverTargets", { discover: true });
  return {
    cdp,
    meta: {
      browserProduct: String(version.Browser || "").split("/")[0] || "unknown",
      browserVersion: String(version.Browser || "").split("/")[1] || "",
      targetCountAtConnect: targets.length,
      cdpEndpointObtained: "DevToolsActivePort + /json/version",
    },
  };
}

export function findPageTarget(targets, urlPrefix) {
  return targets.find(
    (t) => t.type === "page" && (t.url || "").startsWith(urlPrefix)
  );
}

export async function openOrFindChatGptTarget(cdp, targets) {
  const existing = findPageTarget(targets, "https://chatgpt.com");
  if (existing) return { targetId: existing.id, alreadyOpen: true };
  const { targetId } = await cdp.send("Target.createTarget", {
    url: "https://chatgpt.com",
  });
  return { targetId, alreadyOpen: false };
}

export async function attachTarget(cdp, targetId) {
  const { sessionId } = await cdp.send("Target.attachToTarget", {
    targetId,
    flatten: true,
  });
  return sessionId;
}

export async function closeTarget(cdp, targetId) {
  await cdp.send("Target.closeTarget", { targetId });
}
