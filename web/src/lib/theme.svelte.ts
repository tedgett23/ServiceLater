import type { Theme } from './types';

// The person's theme lives on their account (GET|PATCH /account) so it follows them between browsers. A copy in
// localStorage lets app.html apply it before the first paint, and draws the sign-in page, so nothing flashes.
const KEY = 'theme';

function stored(): Theme {
	try {
		const t = localStorage.getItem(KEY);
		return t === 'light' || t === 'dark' ? t : 'auto';
	} catch {
		return 'auto';
	}
}

class ThemeState {
	current = $state<Theme>(typeof window === 'undefined' ? 'auto' : stored());

	/** Draws the page in t and remembers it on this device. auto removes the override so the media query decides. */
	apply(t: Theme) {
		this.current = t;
		const root = document.documentElement;
		if (t === 'auto') delete root.dataset.theme;
		else root.dataset.theme = t;
		try {
			if (t === 'auto') localStorage.removeItem(KEY);
			else localStorage.setItem(KEY, t);
		} catch {
			// Storage may be blocked; the account still has it, so the next load applies it.
		}
	}
}

export const theme = new ThemeState();
