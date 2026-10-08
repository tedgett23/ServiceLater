import path from 'node:path';
import { defineConfig } from '@playwright/test';

// The suite drives the real production binary (UI embedded) against a scratch database.
// `make e2e` builds first; see e2e/global-setup.ts for the database and users it expects.
const PORT = Number(process.env.E2E_PORT ?? 18200);
// Where this run's local overrides for bundled apps live (see e2e/apps.spec.ts).
export const APPS_LOCAL_DIR = path.resolve(import.meta.dirname, 'e2e/.local-apps');
// Where this run keeps attachments (emptied by global setup).
export const ATTACHMENTS_DIR = path.resolve(import.meta.dirname, 'e2e/.attachments');
export const DATABASE_URL =
	process.env.SL_E2E_DATABASE_URL ?? 'postgres://servicelater:servicelater@localhost:5432/servicelater_e2e?sslmode=disable';

export default defineConfig({
	testDir: './e2e',
	globalSetup: './e2e/global-setup.ts',
	// Tests share one database and one server, so run them one at a time; each creates its own data.
	workers: 1,
	fullyParallel: false,
	forbidOnly: !!process.env.CI,
	retries: process.env.CI ? 1 : 0,
	reporter: [['list'], ['html', { open: 'never' }]],
	use: {
		baseURL: `http://localhost:${PORT}`,
		trace: 'retain-on-failure',
		screenshot: 'only-on-failure'
	},
	projects: [{ name: 'chromium', use: { browserName: 'chromium', viewport: { width: 1440, height: 900 } } }],
	webServer: {
		command: './e2e/start-server.sh',
		url: `http://localhost:${PORT}/healthz`,
		reuseExistingServer: false,
		timeout: 30_000,
		env: {
			SL_ADDR: `:${PORT}`,
			SL_JWT_SECRET: 'e2e-only-secret-e2e-only-secret-e2e-only',
			SL_DATABASE_URL: DATABASE_URL,
			SL_APPS_LOCAL_DIR: APPS_LOCAL_DIR,
			SL_ATTACHMENTS_DIR: ATTACHMENTS_DIR,
			// Encrypts the mail server password (e2e/email.spec.ts). Any 32 bytes, base64; never a real key.
			SL_SECRETS_KEY: 'ZTJlLW9ubHktc2VjcmV0cy1rZXktMzItYnl0ZXMhISE=',
			// Chromium releases a closed test context's sockets late, and the suite opens many as one user.
			SL_MAX_LIVE_CONNS: '200'
		}
	}
});
