import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { APPS_LOCAL_DIR, DATABASE_URL } from '../playwright.config';

const root = path.resolve(import.meta.dirname, '../..');
export const BINARY = path.join(root, 'backend/bin/servicelater');
export const appDir = (name: string) => path.join(root, 'apps', name);

/** Runs `servicelater app ...` against the scratch database, as an operator would. Returns its output. */
export function servicelater(...args: string[]) {
	return execFileSync(BINARY, ['app', ...args], {
		env: { ...process.env, SL_DATABASE_URL: DATABASE_URL, SL_APPS_LOCAL_DIR: APPS_LOCAL_DIR },
		encoding: 'utf8',
		stdio: ['ignore', 'pipe', 'pipe']
	});
}

/** What test apps may do as their own principal (async rules, workflows): see, edit and annotate tickets. Installing
 * them approves it, as an operator would after reviewing the plan. */
export const APP_PERMISSIONS = 'permissions: [records:read:*, records:update:*, comments:internal]\n';

/** Installs an app from a folder, approving the permissions it asks for. */
export const installApp = (dir: string) => servicelater('install', '-approve-permissions', dir);

/** Puts the bundled apps back to what the binary ships (plus any local files present), as a restart would. */
export const installFirstParty = () => servicelater('sync');

/** Writes a local override for a bundled app, the way an operator would. Returns a function that removes it. */
export function writeLocalOverride(app: string, file: string, content: string) {
	const full = path.join(APPS_LOCAL_DIR, app, file);
	fs.mkdirSync(path.dirname(full), { recursive: true });
	fs.writeFileSync(full, content);
	return () => fs.rmSync(path.join(APPS_LOCAL_DIR, app), { recursive: true, force: true });
}

/**
 * A copy of the incident-management app with edits applied to its files, in a temporary folder.
 * `edits` maps a file (relative to the app) to a function that rewrites its text.
 */
export function modifiedIncidentApp(edits: Record<string, (text: string) => string>) {
	const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'sl-app-'));
	fs.cpSync(appDir('incident-management'), dir, { recursive: true });
	for (const [file, edit] of Object.entries(edits)) {
		const full = path.join(dir, file);
		fs.writeFileSync(full, edit(fs.readFileSync(full, 'utf8')));
	}
	return { dir, cleanup: () => fs.rmSync(dir, { recursive: true, force: true }) };
}

/** Bumps the version so the change is recorded as a new release of the app. */
export const bumpVersion = (to: string) => (text: string) => text.replace(/^version: .*$/m, `version: ${to}`);

/** A small app made of the given files (paths relative to the app, e.g. `default/workflows/x.yaml`), in a temp folder. */
export function tempApp(name: string, files: Record<string, string>) {
	const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'sl-app-'));
	fs.writeFileSync(
		path.join(dir, 'app.yaml'),
		`apiVersion: 1\nname: ${name}\nversion: 1.0.0\nauthor: e2e\ndescription: "Made by an end-to-end test."\n${APP_PERMISSIONS}`
	);
	for (const [file, content] of Object.entries(files)) {
		const full = path.join(dir, file);
		fs.mkdirSync(path.dirname(full), { recursive: true });
		fs.writeFileSync(full, content);
	}
	return { dir, cleanup: () => fs.rmSync(dir, { recursive: true, force: true }) };
}
