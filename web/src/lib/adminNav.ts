import { can, P, type User } from './types';

/** An admin page and who sees it. */
export interface AdminPage {
	href: string;
	label: string;
}

/**
 * The admin section's pages, grouped, as this person may open them: the main navigation lists them under Admin, and
 * on a phone or with the navigation folded to icons they are a bar above the page. The API reference leaves the app
 * shell (it needs no sign-in).
 */
export function adminGroups(user: User | null | undefined): { label: string; pages: AdminPage[] }[] {
	const groups = [
		{
			label: 'People',
			pages: [
				can(user, P.usersManage) && { href: '/admin/users', label: 'Users' },
				can(user, P.groupsManage) && { href: '/admin/groups', label: 'Groups' },
				{ href: '/admin/roles', label: 'Roles' }
			]
		},
		{
			label: 'Work',
			pages: [
				can(user, P.catalogManage) && { href: '/admin/catalog', label: 'Catalog' },
				can(user, P.formsManage) && { href: '/admin/forms', label: 'Forms' },
				can(user, P.calendarsManage) && { href: '/admin/calendars', label: 'Calendars' }
			]
		},
		{
			label: 'Platform',
			pages: [
				can(user, P.settingsManage) && { href: '/admin/sign-in', label: 'Sign-in' },
				can(user, P.settingsManage) && { href: '/admin/email', label: 'Email' },
				can(user, P.settingsManage) && { href: '/admin/incoming', label: 'Incoming email' },
				can(user, P.settingsManage) && { href: '/admin/messaging', label: 'Messaging' },
				can(user, P.settingsManage) && { href: '/admin/connections', label: 'Connections' },
				can(user, P.settingsManage) && { href: '/admin/storage', label: 'Storage' },
				can(user, P.settingsManage) && { href: '/admin/history', label: 'History' },
				{ href: '/docs/api', label: 'API reference' }
			]
		}
	];
	return groups
		.map((g) => ({ label: g.label, pages: g.pages.filter((p): p is AdminPage => !!p) }))
		.filter((g) => g.pages.length > 0);
}
