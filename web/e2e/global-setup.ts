import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import { request, type FullConfig } from '@playwright/test';
import { DATABASE_URL } from '../playwright.config';
import { PASSWORD, stateFile, USERS, type Who } from './users';

/**
 * Runs after the server migrated a fresh database and installed its bundled apps. Creates users with the real CLI.
 *
 * Sessions are saved once and reused, so the suite doesn't trip the login rate limit. Tests that sign out or
 * refresh tokens must use their own login (see helpers.freshLogin), because doing so would invalidate a
 * shared session for everyone else.
 */
export default async function globalSetup(config: FullConfig) {
	const bin = path.resolve(import.meta.dirname, '../../backend/bin/servicelater');
	for (const u of Object.values(USERS)) {
		execFileSync(bin, ['user', 'create', '-email', u.email, '-name', u.name, '-role', u.role], {
			env: { ...process.env, SL_DATABASE_URL: DATABASE_URL, SL_USER_PASSWORD: PASSWORD },
			stdio: ['ignore', 'ignore', 'inherit']
		});
	}

	fs.mkdirSync(path.dirname(stateFile('agent')), { recursive: true });
	const baseURL = config.projects[0].use.baseURL!;
	for (const who of Object.keys(USERS) as Who[]) {
		const ctx = await request.newContext({ baseURL });
		const res = await ctx.post('/api/v1/auth/login', { data: { email: USERS[who].email, password: PASSWORD } });
		if (!res.ok()) throw new Error(`login failed for ${who}: ${res.status()} ${await res.text()}`);
		await ctx.storageState({ path: stateFile(who) });
		await ctx.dispose();
	}

	// The server installed its bundled apps while starting (see start-server.sh); wait until it serves the lifecycle.
	const agent = await request.newContext({ baseURL, storageState: stateFile('agent') });
	for (let i = 0; i < 50; i++) {
		const res = await agent.get('/api/v1/lifecycles/incident');
		if (res.ok() && (await res.json()).app === 'incident-management') break;
		if (i === 49) throw new Error('the server never picked up the installed apps');
		await new Promise((r) => setTimeout(r, 100));
	}
	await agent.dispose();

	// The suite drives a few shared accounts far faster than a person could, so the API limit for people and keys
	// is off here (limits.spec.ts puts back what it finds). Every other rate limit keeps its default.
	const admin = await request.newContext({ baseURL, storageState: stateFile('admin') });
	const lifted = await admin.put('/api/v1/settings/limits', { data: { api_people_per_minute: 0, api_keys_per_minute: 0 } });
	if (!lifted.ok()) throw new Error(`lifting the API rate limit: ${lifted.status()} ${await lifted.text()}`);
	await admin.dispose();
}
