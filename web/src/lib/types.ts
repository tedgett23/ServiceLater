export type Role = 'admin' | 'agent' | 'end_user';
/** A status id. Which ones exist, and what they are called, comes from the installed lifecycle. */
export type Status = string;

/** What core code understands about a status; the app decides the name and label. */
export type Category = 'open' | 'waiting' | 'done' | 'closed';

export interface User {
	id: string;
	email: string;
	name: string;
	role: Role;
}

export interface Ticket {
	id: string;
	/** The kind of ticket (incident, change ...). Numbers count within a type. */
	type: string;
	number: number;
	/** How people name it: the type's prefix and its number, such as INC42. */
	reference: string;
	title: string;
	description: string;
	/** How an incident was fixed. Lives in the incident's own table; empty for other types. */
	resolution: string;
	status: Status;
	state_category: Category;
	/** Only on single-ticket responses, and only for staff. */
	moves?: Move[];
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

export interface LifecycleState {
	id: string;
	label: string;
	category: Category;
}

/** One thing a person can do to a ticket right now. The server decides what is offered, and why one is blocked. */
export interface Move {
	to: string;
	verb: string;
	primary: boolean;
	blocked?: string;
}

export interface LifecycleData {
	record_type: string;
	initial: string;
	states: LifecycleState[];
	app: string;
	version: string;
}

export type RunStatus = 'queued' | 'running' | 'waiting' | 'done' | 'failed' | 'cancelled';

export interface WorkflowRun {
	id: string;
	app: string;
	workflow: string;
	app_version: string;
	ticket_id: string | null;
	reference: string;
	started_by: string;
	status: RunStatus;
	current_step: string;
	steps_run: number;
	error: string;
	wake_at: string | null;
	created_at: string;
	updated_at: string;
	finished_at: string | null;
	// Only when one run is fetched: the graph it started with, its variables now, and what happened.
	graph?: { id: string; type: string; next: string[] }[];
	vars?: Record<string, unknown>;
	steps?: RunStep[];
}

export interface RunStep {
	seq: number;
	step_id: string;
	type: string;
	outcome: 'ok' | 'waiting' | 'timeout' | 'failed';
	detail: Record<string, unknown>;
	error: string;
	duration_ms: number;
	at: string;
}

export const runFinished = (s: RunStatus) => s === 'done' || s === 'failed' || s === 'cancelled';
