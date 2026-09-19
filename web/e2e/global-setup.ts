import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import { request, type FullConfig } from '@playwright/test';
import pg from 'pg';
import { DATABASE_URL } from '../playwright.config';
import { PASSWORD, stateFile, USERS, type Who } from './users';

/**
 * Runs after the server is up (so the schema is migrated). Resets the scratch database, creates the test users
 * with the real `servicelater user create` command, and saves a signed-in session per user.
 *
 * Sessions are saved once and reused, so the suite doesn't trip the login rate limit. Tests that sign out or
 * refresh tokens must use their own login (see helpers.freshLogin), because doing so would invalidate a
 * shared session for everyone else.
 */
export default async function globalSetup(config: FullConfig) {
	const client = new pg.Client({ connectionString: DATABASE_URL });
	await client.connect();
	try {
		await client.query('TRUNCATE tickets, sessions, rate_limits, user_credentials, users CASCADE');
	} finally {
		await client.end();
	}

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
}
