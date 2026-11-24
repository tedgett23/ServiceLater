import path from 'node:path';
import { AUTH_DIR } from '../playwright.config';

export const PASSWORD = 'e2e-password-123';

export const USERS = {
	admin: { email: 'ada@e2e.test', name: 'Ada Okafor', role: 'admin' },
	agent: { email: 'priya@e2e.test', name: 'Priya Raman', role: 'agent' },
	agent2: { email: 'tomas@e2e.test', name: 'Tomás Ibarra', role: 'agent' },
	lena: { email: 'lena@e2e.test', name: 'Lena Fischer', role: 'end_user' },
	marcus: { email: 'marcus@e2e.test', name: 'Marcus Webb', role: 'end_user' }
} as const;

export type Who = keyof typeof USERS;

/** A saved signed-in browser session per user, created once in global setup. */
export const stateFile = (who: Who) => path.join(AUTH_DIR, `${who}.json`);
