export type Role = 'admin' | 'agent' | 'end_user';
export type Status = 'new' | 'in_progress' | 'on_hold' | 'resolved' | 'closed';

export interface User {
	id: string;
	email: string;
	name: string;
	role: Role;
}

export interface Ticket {
	id: string;
	number: number;
	title: string;
	description: string;
	status: Status;
	priority: 1 | 2 | 3 | 4;
	requester_id: string;
	requester_name: string;
	assignee_id: string | null;
	assignee_name: string | null;
	created_at: string;
	updated_at: string;
}

export interface TicketPage {
	items: Ticket[];
	limit: number;
	offset: number;
	has_more: boolean;
}

export interface Comment {
	id: string;
	ticket_id: string;
	author_id: string;
	author_name: string;
	body: string;
	internal: boolean;
	created_at: string;
	pending?: boolean;
}

export const isStaff = (u: User | null | undefined) => u?.role === 'admin' || u?.role === 'agent';
