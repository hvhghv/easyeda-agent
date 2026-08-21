// Bump the connector version in extension.json + package.json + package-lock.json
// in lock-step. The Windows CI checks all three before building.
//
// EasyEDA dedups installed extensions by (uuid, version): re-importing an .eext
// whose version equals the installed one is a no-op. So every connector change
// that the user needs to test must ship a NEW, higher version. Run this before
// building the .eext you hand to the user.
//
//   node scripts/bump.mjs            # patch: 0.2.0 -> 0.2.1
//   node scripts/bump.mjs minor      # minor: 0.2.1 -> 0.3.0
//   node scripts/bump.mjs major      # major: 0.3.0 -> 1.0.0
//   node scripts/bump.mjs 0.5.0      # set an explicit version

import crypto from 'node:crypto';
import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const extPath = path.join(here, '..', 'extension.json');
const pkgPath = path.join(here, '..', 'package.json');
const lockPath = path.join(here, '..', 'package-lock.json');
const changelogPath = path.join(here, '..', 'CHANGELOG.md');

// Does CHANGELOG.md carry a `## [<version>]` heading for this version?
function changelogHasEntry(version) {
	let text;
	try {
		text = fs.readFileSync(changelogPath, 'utf-8');
	}
	catch {
		return false;
	}
	// `## [0.5.4]` at the start of a line, version bracketed.
	const re = new RegExp(`^##\\s*\\[${version.replace(/\./g, '\\.')}\\]`, 'm');
	return re.test(text);
}

function parse(v) {
	const m = /^(\d+)\.(\d+)\.(\d+)/.exec(v);
	if (!m) throw new Error(`unparseable version: ${v}`);
	return [Number(m[1]), Number(m[2]), Number(m[3])];
}

function nextVersion(current, mode) {
	if (/^\d+\.\d+\.\d+$/.test(mode)) return mode; // explicit version
	const [maj, min, pat] = parse(current);
	switch (mode) {
		case 'major': return `${maj + 1}.0.0`;
		case 'minor': return `${maj}.${min + 1}.0`;
		case 'patch':
		default: return `${maj}.${min}.${pat + 1}`;
	}
}

function writeJsonTabs(file, obj) {
	fs.writeFileSync(file, `${JSON.stringify(obj, null, '\t')}\n`, 'utf-8');
}

const args = process.argv.slice(2);
const mode = args.find((a) => !a.startsWith('--')) ?? 'patch';
const freshUuid = args.includes('--uuid'); // opt-in fallback: mint a new uuid
// --require-changelog: hard-fail when the new version has no CHANGELOG entry
// (used by `make release`); otherwise a missing entry is just a warning.
const requireChangelog = args.includes('--require-changelog');

const ext = JSON.parse(fs.readFileSync(extPath, 'utf-8'));
const pkg = JSON.parse(fs.readFileSync(pkgPath, 'utf-8'));
const lock = JSON.parse(fs.readFileSync(lockPath, 'utf-8'));

if (!lock || typeof lock !== 'object' || Array.isArray(lock)
	|| !lock.packages || typeof lock.packages !== 'object' || Array.isArray(lock.packages)
	|| !lock.packages[''] || typeof lock.packages[''] !== 'object' || Array.isArray(lock.packages[''])) {
	throw new Error('package-lock.json has no npm root package entry (packages[""])');
}
if (lock.name !== pkg.name || lock.packages[''].name !== pkg.name) {
	throw new Error(`package-lock.json package name does not match package.json: ${lock.name ?? '<missing>'} / ${lock.packages[''].name ?? '<missing>'} vs ${pkg.name}`);
}

const from = ext.version;
const to = nextVersion(from, mode);

// UUID policy: keep it STABLE by default so the build is the SAME extension you
// update in place (uninstall the old one in EasyEDA's 已安装 tab → import). Only
// the fallback path (--uuid) mints a fresh uuid, which imports as a NEW extension
// entry (no uninstall needed, but you must delete the stale one). EasyEDA dedups
// installed extensions by uuid, so a same-uuid re-import requires that uninstall.
// A minted uuid must match the connector's testUuid: 32 lowercase hex chars.
const fromUuid = ext.uuid;
const toUuid = freshUuid ? crypto.randomUUID().replaceAll('-', '') : fromUuid;

ext.version = to;
ext.uuid = toUuid;
pkg.version = to;
lock.version = to;
lock.packages[''].version = to;

writeJsonTabs(extPath, ext);
writeJsonTabs(pkgPath, pkg);
writeJsonTabs(lockPath, lock);

console.log(`version ${from} -> ${to}  (extension.json + package.json + package-lock.json)`);
console.log(freshUuid
	? `uuid    ${fromUuid} -> ${toUuid}  (FRESH uuid — imports as a new extension; delete the old one)`
	: `uuid    ${toUuid}  (unchanged — update in place: uninstall old in 已安装, then import)`);

// Changelog gate: a release MUST document the version; the dev loop only nags.
if (!changelogHasEntry(to)) {
	const msg = `CHANGELOG.md has no "## [${to}]" entry — add one in extension/CHANGELOG.md`;
	if (requireChangelog) {
		console.error(`\n❌ ${msg}\n   (release requires a changelog entry for this version)`);
		process.exit(1);
	}
	console.warn(`\n⚠️  ${msg} before release.`);
}
