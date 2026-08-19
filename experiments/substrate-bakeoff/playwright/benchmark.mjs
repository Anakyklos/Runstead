import { chromium } from 'playwright';
import { execFileSync } from 'node:child_process';
import { mkdir, rm, writeFile } from 'node:fs/promises';
import { join, resolve } from 'node:path';

const root = resolve(import.meta.dirname, '..');
const baseURL = process.env.RUNSTEAD_FIXTURE_URL || 'http://127.0.0.1:18765';
const executablePath = process.env.CHROMIUM_PATH || '/usr/bin/chromium';
const profileRoot = join(root, 'profiles', 'bench-playwright');
const outputPath = join(root, 'output', 'overhead-playwright.json');

function processes(profile) {
  const lines = execFileSync('ps', ['-eo', 'pid=,ppid=,rss=,args='], { encoding: 'utf8' }).trim().split('\n').filter(Boolean);
  return lines.filter(line => line.includes(`--user-data-dir=${profile}`)).map(line => {
    const match = line.trim().match(/^(\d+)\s+(\d+)\s+(\d+)\s+(.*)$/);
    return match ? { pid: Number(match[1]), ppid: Number(match[2]), rss_kb: Number(match[3]), args: match[4].replaceAll(profile, '<profile>') } : null;
  }).filter(Boolean);
}

const main = async () => {
  await mkdir(join(root, 'output'), { recursive: true });
  await rm(profileRoot, { recursive: true, force: true });
  const samples = [];
  for (let i = 0; i < 3; i += 1) {
    const profile = join(profileRoot, `sample-${i + 1}`);
    const start = performance.now();
    const context = await chromium.launchPersistentContext(profile, { headless: true, executablePath, timeout: 10000, args: ['--no-sandbox', '--disable-dev-shm-usage', '--disable-background-networking', '--disable-component-update'] });
    const launched = performance.now();
    const page = await context.newPage();
    await page.goto(`${baseURL}/page`, { waitUntil: 'domcontentloaded' });
    const navigated = performance.now();
    const tree = processes(profile);
    await context.close();
    const closed = performance.now();
    samples.push({ sample: i + 1, startup_ms: +(launched - start).toFixed(2), navigation_ms: +(navigated - launched).toFixed(2), shutdown_ms: +(closed - navigated).toFixed(2), process_count: tree.length, rss_total_kb: tree.reduce((sum, process) => sum + process.rss_kb, 0), process_tree: tree });
  }
  const artifact = { schema: 'runstead.substrate-bakeoff.overhead.v1', candidate: 'playwright', environment: { node: process.version, playwright: (await import('playwright/package.json', { with: { type: 'json' } })).default.version, chromium_path: executablePath }, samples, methodology: 'Three cold persistent-context launches against the local fixture; RSS is the sum of Chromium processes observed immediately after navigation.' };
  await writeFile(outputPath, JSON.stringify(artifact, null, 2));
  console.log(JSON.stringify({ candidate: artifact.candidate, output: outputPath, samples: samples.length }));
};
main().catch(error => { console.error(error); process.exitCode = 1; });
