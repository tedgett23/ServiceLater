const NAV_KEY = 'nav.collapsed';

function storedCollapsed() {
	try {
		return localStorage.getItem(NAV_KEY) === '1';
	} catch {
		return false;
	}
}

/** App-wide UI state that more than one component needs. */
export const ui = $state({
	newTicketOpen: false,
	announce: '',
	/** The navigation at the left shows only its icons. Remembered on this device. */
	navCollapsed: typeof window === 'undefined' ? false : storedCollapsed()
});

export function setNavCollapsed(collapsed: boolean) {
	ui.navCollapsed = collapsed;
	try {
		if (collapsed) localStorage.setItem(NAV_KEY, '1');
		else localStorage.removeItem(NAV_KEY);
	} catch {
		// Storage may be blocked: it just will not be remembered.
	}
}
