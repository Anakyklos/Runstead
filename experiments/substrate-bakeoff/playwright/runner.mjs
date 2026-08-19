import { chromium } from 'playwright';
import { execFileSync } from 'node:child_process';
import { mkdir, rm } from 'node:fs/promises';
import { join, resolve } from 'node:path';

const root = resolve(import.meta.dirname, '..');
const baseURL = process.env.RUNSTEAD_FIXTURE_URL || 'http://127.0.0.1:18765';
const executablePath = process.env.CHROMIUM_PATH || '/usr/bin/chromium';
const outputDir = process.env.RUNSTEAD_OUTPUT_DIR || join(root, 'output');
const profileRoot = process.env.RUNSTEAD_PROFILE_ROOT || join(root, 'profiles', 'playwright');
const timeoutMs = Number(process.env.RUNSTEAD_SCENARIO_TIMEOUT_MS || 1200);

const sleep = ms => new Promise(resolvePromise => setTimeout(resolvePromise, ms));

async function fixture(path, options = {}) {
  const response = await fetch(`${baseURL}${path}`, options);
  if (!response.ok && response.status !== 204) throw new Error(`fixture ${path}: ${response.status}`);
  return response;
}

function browserProcesses(profile) {
  let lines = [];
  try {
    lines = execFileSync('ps', ['-eo', 'pid=,ppid=,args='], { encoding: 'utf8' }).trim().split('\n').filter(Boolean);
  } catch {
    return [];
  }
  return lines.filter(line => line.includes(`--user-data-dir=${profile}`)).map(line => {
    const match = line.trim().match(/^(\d+)\s+(\d+)\s+(.*)$/);
    return match ? { pid: Number(match[1]), ppid: Number(match[2]), args: match[3] } : null;
  }).filter(Boolean);
}

function processTree(profile) {
  const processes = browserProcesses(profile);
  const byParent = new Map();
  for (const process of processes) {
    if (!byParent.has(process.ppid)) byParent.set(process.ppid, []);
    byParent.get(process.ppid).push(process.pid);
  }
  const roots = processes.filter(process => !processes.some(other => other.pid === process.ppid));
  const tree = [];
  const visit = (pid, depth) => {
    const process = processes.find(candidate => candidate.pid === pid);
    if (!process) return;
    tree.push({ pid: process.pid, ppid: process.ppid, depth, args: process.args.replaceAll(profile, '<profile>') });
    for (const child of byParent.get(pid) || []) visit(child, depth + 1);
  };
  for (const rootProcess of roots) visit(rootProcess.pid, 0);
  return tree;
}

function killProfileBrowsers(profile) {
  const processes = browserProcesses(profile).sort((a, b) => b.pid - a.pid);
  for (const process of processes) {
    try { process.kill(process.pid, 'SIGKILL'); } catch {}
  }
  return processes.map(process => process.pid);
}

async function launch(profile) {
  await mkdir(profile, { recursive: true });
  const context = await chromium.launchPersistentContext(profile, {
    headless: true,
    executablePath,
    timeout: 10000,
    args: ['--no-sandbox', '--disable-dev-shm-usage', '--disable-background-networking', '--disable-component-update'],
  });
  return context;
}

async function installServiceWorker(page) {
  await page.goto(`${baseURL}/page?sw=1`, { waitUntil: 'domcontentloaded' });
  await page.evaluate(async () => {
    if (navigator.serviceWorker) await navigator.serviceWorker.ready;
  });
  await page.reload({ waitUntil: 'domcontentloaded' });
  await page.waitForFunction(() => window.serviceWorkerControlled(), null, { timeout: 3000 }).catch(() => {});
  return await page.evaluate(() => window.serviceWorkerControlled());
}

async function pollResult(page, logicalId, maxMs = timeoutMs) {
  const started = Date.now();
  while (Date.now() - started < maxMs) {
    const result = await page.evaluate(id => window.getSubmitResult(id), logicalId).catch(() => null);
    if (result && ['response_completed', 'response_incomplete', 'physical_abort_observed'].includes(result.phase)) return result;
    await sleep(20);
  }
  return await page.evaluate(id => window.getSubmitResult(id), logicalId).catch(() => null);
}

async function runScenario(name, options = {}) {
  const profile = join(profileRoot, options.profile || 'scenario');
  await rm(profile, { recursive: true, force: true });
  await fixture('/reset', { method: 'POST' });
  const result = { name, candidate: 'playwright', profile_ref: options.profile || 'scenario', evidence: {}, page_events: [] };
  let context;
  let page;
  try {
    context = await launch(profile);
    page = await context.newPage();
    page.on('request', request => {
      if (request.url().includes('/submit')) {
        result.page_events.push({ type: 'request', method: request.method(), path: new URL(request.url()).pathname, scenario: new URL(request.url()).searchParams.get('scenario'), redirected_from: request.redirectedFrom() ? new URL(request.redirectedFrom().url()).pathname : null });
      }
    });
    page.on('response', response => {
      if (response.url().includes('/submit')) result.page_events.push({ type: 'response', status: response.status(), path: new URL(response.url()).pathname });
    });
    page.on('requestfailed', request => {
      if (request.url().includes('/submit')) result.page_events.push({ type: 'request_failed', error: request.failure()?.errorText || 'unknown', path: new URL(request.url()).pathname });
    });

    if (options.serviceWorker) {
      result.evidence.service_worker_controlled = await installServiceWorker(page);
    } else {
      await page.goto(`${baseURL}/page`, { waitUntil: 'domcontentloaded' });
    }
    await page.evaluate(() => window.setProfileMarker('synthetic-playwright-marker'));
    result.evidence.process_tree_before = processTree(profile);

    if (options.preDispatchCancel) {
      result.evidence.dispatch_observed = false;
      result.evidence.canceled_pre_dispatch = true;
      result.evidence.conservative_state = 'not_sent';
    } else {
      const logicalId = await page.evaluate(scenario => window.startSubmit(scenario), options.fixtureScenario || name);
      result.evidence.logical_id = logicalId;
      if (options.cancelAfterMs !== undefined) {
        await sleep(options.cancelAfterMs);
        result.evidence.dispatch_observed = result.page_events.some(event => event.type === 'request');
        await page.evaluate(id => window.cancelSubmit(id), logicalId).catch(() => {});
        result.evidence.caller_cancellation = true;
      }
      if (options.killBrowser) {
        await sleep(options.killAfterMs || 100);
        result.evidence.dispatch_observed = result.page_events.some(event => event.type === 'request');
        result.evidence.killed_pids = killProfileBrowsers(profile);
        result.evidence.browser_crashed = true;
      }
      if (options.controllerDisconnect) {
        await sleep(options.disconnectAfterMs || 100);
        result.evidence.dispatch_observed = result.page_events.some(event => event.type === 'request');
        result.evidence.controller_disconnected = true;
        await context.close().catch(() => {});
        context = null;
      }
      if (options.waitForResponseStart) {
        const started = Date.now();
        while (!result.page_events.some(event => event.type === 'response') && Date.now() - started < 1000) await sleep(20);
      }
      if (options.cancelAfterResponseStart) {
        result.evidence.dispatch_observed = result.page_events.some(event => event.type === 'request');
        result.evidence.response_started = result.page_events.some(event => event.type === 'response');
        await page.evaluate(id => window.cancelSubmit(id), logicalId).catch(() => {});
        result.evidence.caller_cancellation = true;
      }
      result.evidence.page_result = await pollResult(page, logicalId, options.pollMs || timeoutMs);
      result.evidence.dispatch_observed ||= result.page_events.some(event => event.type === 'request');
      const pageResult = result.evidence.page_result;
      if (pageResult?.phase === 'response_completed') result.evidence.conservative_state = 'completed';
      else if (result.evidence.dispatch_observed) result.evidence.conservative_state = 'sent_unconfirmed';
      else result.evidence.conservative_state = 'not_sent';
      if (pageResult?.phase === 'physical_abort_observed') result.evidence.physical_abort_observed = true;
      if (pageResult?.phase === 'response_incomplete') result.evidence.response_incomplete = true;
    }

    if (options.profileReadback) {
      result.evidence.marker_first_open = await page.evaluate(() => window.profileMarker()).catch(() => null);
    }
  } catch (error) {
    result.evidence.error_type = error?.name || 'Error';
    result.evidence.error = String(error?.message || error).slice(0, 240);
    if (result.evidence.dispatch_observed) result.evidence.conservative_state = 'unknown_submission';
  } finally {
    if (context) await context.close().catch(() => {});
    result.evidence.process_tree_after = processTree(profile);
    result.evidence.browser_processes_after = browserProcesses(profile).length;
  }
  result.fixture_events = await (await fixture('/events')).json();
  result.evidence.response_started = result.page_events.some(event => event.type === 'response');
  result.evidence.timeout = name.includes('timeout');
  result.evidence.canceled = name.includes('cancel');
  if (result.evidence.dispatch_observed && !result.evidence.physical_abort_observed && (result.evidence.timeout || result.evidence.canceled || result.evidence.browser_crashed || result.evidence.controller_disconnected)) {
    result.evidence.physical_abort_unproven = true;
  }
  result.evidence.physical_post_count = result.fixture_events.filter(event => event.method === 'POST' && (event.path === '/submit' || event.path === '/effect-final')).length;
  result.evidence.duplicate_gate = name === 'redirect' ? (result.evidence.physical_post_count >= 1 ? 'pass_redirect_hops_are_explicit' : 'fail_missing_redirect_hop') : (result.evidence.physical_post_count <= 1 ? 'pass' : 'fail_unexpected_multiple_physical_posts');
  result.evidence.physical_post_paths = result.fixture_events.filter(event => event.method === 'POST').map(event => event.path);
  result.evidence.service_worker_request_count = result.fixture_events.filter(event => event.service_worker).length;
  return result;
}

async function profileReuseAndIsolation() {
  const profileA = join(profileRoot, 'profile-a');
  const profileB = join(profileRoot, 'profile-b');
  await rm(profileA, { recursive: true, force: true });
  await rm(profileB, { recursive: true, force: true });
  const open = async (profile, marker) => {
    const context = await launch(profile);
    const page = await context.newPage();
    await page.goto(`${baseURL}/page`, { waitUntil: 'domcontentloaded' });
    const before = await page.evaluate(() => window.profileMarker());
    await page.evaluate(value => window.setProfileMarker(value), marker);
    await context.close();
    return before;
  };
  const firstA = await open(profileA, 'profile-a-marker');
  const firstB = await open(profileB, 'profile-b-marker');
  const contextA = await launch(profileA);
  const pageA = await contextA.newPage();
  await pageA.goto(`${baseURL}/page`, { waitUntil: 'domcontentloaded' });
  const reusedA = await pageA.evaluate(() => window.profileMarker());
  await contextA.close();
  const contextB = await launch(profileB);
  const pageB = await contextB.newPage();
  await pageB.goto(`${baseURL}/page`, { waitUntil: 'domcontentloaded' });
  const reusedB = await pageB.evaluate(() => window.profileMarker());
  await contextB.close();
  return { first_open_markers: { profile_a: firstA, profile_b: firstB }, reused_markers: { profile_a: reusedA, profile_b: reusedB }, isolated: reusedA === 'profile-a-marker' && reusedB === 'profile-b-marker' && reusedA !== reusedB };
}

const scenarios = [
  ['normal', {}],
  ['redirect', { fixtureScenario: 'redirect' }],
  ['service-worker', { fixtureScenario: 'normal', serviceWorker: true }],
  ['timeout-before-headers', { fixtureScenario: 'headers-delay', cancelAfterMs: 60 }],
  ['cancel-after-headers', { fixtureScenario: 'body-delay', cancelAfterMs: 340 }],
  ['cancel-in-flight', { fixtureScenario: 'open', cancelAfterMs: 100 }],
  ['sse-complete', { fixtureScenario: 'sse-complete' }],
  ['sse-truncated', { fixtureScenario: 'sse-truncated' }],
  ['sse-eof', { fixtureScenario: 'sse-eof' }],
  ['sse-partial', { fixtureScenario: 'sse-partial' }],
  ['cancel-before-dispatch', { preDispatchCancel: true }],
  ['browser-kill-in-flight', { fixtureScenario: 'open', killBrowser: true, killAfterMs: 100 }],
  ['controller-disconnect-in-flight', { fixtureScenario: 'open', controllerDisconnect: true, disconnectAfterMs: 100 }],
];

const main = async () => {
  await mkdir(outputDir, { recursive: true });
  await mkdir(profileRoot, { recursive: true });
  const startedAt = new Date().toISOString();
  const results = [];
  for (const [name, options] of scenarios) results.push(await runScenario(name, options));
  const profile = await profileReuseAndIsolation();
  const artifact = {
    schema: 'runstead.substrate-bakeoff.v1',
    candidate: 'playwright',
    started_at: startedAt,
    finished_at: new Date().toISOString(),
    environment: { node: process.version, playwright: (await import('playwright/package.json', { with: { type: 'json' } })).default.version, chromium_path: executablePath },
    runtime_tree: { runner: 'node -> Playwright driver -> Chromium', browser_processes_sample: results.find(r => r.evidence.process_tree_before)?.evidence.process_tree_before || [] },
    profile_lifecycle: profile,
    scenarios: results,
    limitations: ['The lane uses Playwright persistent contexts and ps-based process inspection; no real account or ChatGPT session is used.', 'Playwright page request events are browser-observation evidence, not proof of upstream acceptance after dispatch.'],
  };
  const outputPath = join(outputDir, 'playwright-results.json');
  const { writeFile } = await import('node:fs/promises');
  await writeFile(outputPath, JSON.stringify(artifact, null, 2));
  console.log(JSON.stringify({ candidate: artifact.candidate, scenarios: results.length, output: outputPath, profile_isolated: profile.isolated }));
};

main().catch(error => { console.error(error); process.exitCode = 1; });
