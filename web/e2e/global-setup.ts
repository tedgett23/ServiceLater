import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import { request, type FullConfig } from '@playwright/test';
import pg from 'pg';
import { ATTACHMENTS_DIR, DATABASE_URL } from '../playwright.config';
import { PASSWORD, stateFile, USERS, type Who } from './users';

/**
 * Runs after the server is up (so the schema is migrated, and it has installed its bundled apps). Resets the scratch
 * database, creates the test users with the real `servicelater` commands, and saves a signed-in session per user.
 *
 * Sessions are saved once and reused, so the suite doesn't trip the login rate limit. Tests that sign out or
 * refresh tokens must use their own login (see helpers.freshLogin), because doing so would invalidate a
 * shared session for everyone else.
 */
export default async function globalSetup(config: FullConfig) {
	const client = new pg.Client({ connectionString: DATABASE_URL });
	await client.connect();
	try {
		await client.query('TRUNCATE tasks, jobs, action_runs, sessions, rate_limits, user_credentials, users, groups CASCADE');
		// users cascades to business calendars: put back the one incident-management's SLAs name.
		await client.query('SELECT app_seed_business_calendars()');
		// Files go to an empty folder, which the server claims afresh (its id is a setting).
		await client.query(`DELETE FROM settings WHERE name LIKE 'storage.%'`);
		fs.rmSync(ATTACHMENTS_DIR, { recursive: true, force: true });
		// Incoming mail starts off, with an empty log (a run that died could have left it reading the fake mailbox).
		await client.query(`DELETE FROM settings WHERE name LIKE 'inbound.%'`);
		await client.query('TRUNCATE inbound_emails');
		// Restart each type's numbering, so ticket numbers start at 1 on every run.
		await client.query(`DO $$ DECLARE t text; BEGIN FOR t IN SELECT type FROM task_types LOOP EXECUTE format('ALTER SEQUENCE %I RESTART', 'task_number_' || t); END LOOP; END $$`);
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

	// The server installed its bundled apps while starting (see start-server.sh); wait until it serves the lifecycle.
	const agent = await request.newContext({ baseURL, storageState: stateFile('agent') });
	for (let i = 0; i < 50; i++) {
		const res = await agent.get('/api/v1/lifecycles/incident');
		if (res.ok() && (await res.json()).app === 'incident-management') break;
		if (i === 49) throw new Error('the server never picked up the installed apps');
		await new Promise((r) => setTimeout(r, 100));
	}
	await agent.dispose();
}
