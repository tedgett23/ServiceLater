export type Role = 'admin' | 'agent' | 'end_user';
/** A status id. Which ones exist, and what they are called, comes from the installed lifecycle. */
export type Status = string;

/** What core code understands about a status; the app decides the name and label. */
export type Category = 'open' | 'waiting' | 'done' | 'closed';

export interface User {
	id: string;
	email: string;
	name: string;
	/** The base role. What someone may do is `permissions`, which roles granted to them or their groups add to. */
	role: Role;
	permissions?: string[];
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
	/** The team the ticket is routed to. Its assignee, if any, is a member. */
	assignment_group_id: string | null;
	assignment_group_name: string | null;
	created_at: string;
	updated_at: string;
	/** A change's own fields; absent on other types. */
	change?: ChangeFields;
	/** The ticket's SLA clocks (single-ticket responses). */
	slas?: SlaClock[];
	/** Lists: when the soonest running SLA clock falls due, and whether any clock has breached. */
	sla_due_at?: string;
	sla_breached?: boolean;
	/** Extra fields, the ones the type's form declares, by name. A field that is not set is absent. */
	fields?: Record<string, FieldValue>;
	/** Single-ticket responses: each form field's state now. A field with no entry is shown, optional and writable. */
	field_states?: Record<string, FieldState>;
	/** Single-ticket responses, for those who read the CMDB: the configuration items linked to it. */
	cis?: CISummary[];
	/** Single-ticket responses: the knowledge articles linked to it that the reader may read. */
	articles?: ArticleRef[];
}

/** What a field holds: text (also dates as YYYY-MM-DD and times as ISO), a whole number, or true/false. */
export type FieldValue = string | number | boolean;

/** Whether a field is shown, required and writable right now, and which options a choice offers. */
export interface FieldState {
	hidden?: boolean;
	required?: boolean;
	read_only?: boolean;
	/** The values offered, when not all of a choice's options are. */
	options?: string[];
}

export type FieldType = 'text' | 'textarea' | 'number' | 'boolean' | 'date' | 'datetime' | 'choice' | 'priority';

/** One field of a form: the record's own (core) or an extra field. */
export interface FormField {
	name: string;
	type: FieldType;
	label: string;
	help?: string;
	core: boolean;
	options?: { value: string; label: string }[];
	required?: boolean;
	default?: FieldValue;
	max_length?: number;
	min?: number;
	max?: number;
}

/** How a record type is filled in and shown, from GET /forms/{type}. */
export interface FormDef {
	record_type: string;
	fields: FormField[];
	/** The fields on the form for filing one, in order. */
	create: string[];
	/** The sections shown on the record. */
	detail: { label?: string; fields: string[] }[];
	/** Fields the form's conditions read: ask for the states again when one changes while filing. */
	reactive: string[];
	app?: string;
}

/** An SLA clock on a ticket. The server works everything out; the page only counts down to due_at. */
export interface SlaClock {
	id: string;
	app: string;
	name: string;
	/** What people call it: Respond, Resolve. */
	label: string;
	/** A stopped clock was met, unless breached_at is set. */
	stage: 'running' | 'paused' | 'stopped';
	target_ms: number;
	/** Working time used, as of the response. */
	used_ms: number;
	/** The business calendar it counts by; empty means every hour. */
	calendar: string;
	due_at: string | null;
	breached_at: string | null;
	started_at: string;
	stopped_at: string | null;
	/** Why the clock could not be worked out on the last save, or empty. */
	problem: string;
}

/** A business calendar: the working hours SLAs count, in a time zone, less holidays. */
export interface BusinessCalendar {
	id: string;
	/** What an SLA definition names it by. */
	name: string;
	label: string;
	time_zone: string;
	/** Day 0 is Sunday; times are HH:MM, and end may be 24:00. */
	hours: { day: number; start: string; end: string }[];
	holidays: string[];
	created_by_name: string | null;
	created_at: string;
	updated_at: string;
}

/** What a change has beyond the base: its kind, assessment, planned window and plans. */
export interface ChangeFields {
	change_type: 'standard' | 'normal' | 'emergency';
	/** Empty until assessed. */
	risk: '' | Level;
	impact: '' | Level;
	planned_start: string | null;
	planned_end: string | null;
	implementation_plan: string;
	backout_plan: string;
	test_plan: string;
}

export type Level = 'low' | 'medium' | 'high';

/** One record's planned window on the calendar. */
export interface CalendarEntry {
	id: string;
	type: string;
	number: number;
	reference: string;
	title: string;
	status: Status;
	state_category: Category;
	priority: 1 | 2 | 3 | 4;
	assignee_name: string | null;
	change_type: ChangeFields['change_type'];
	risk: ChangeFields['risk'];
	planned_start: string;
	planned_end: string;
	/** Other entries still to be done whose windows overlap this one. */
	overlaps: string[];
	/** Ids of the change freezes this window overlaps. */
	freezes: string[];
}

/** A change freeze: a period when changes may not be planned (emergency changes excepted, by change-management). */
export interface Freeze {
	id: string;
	name: string;
	reason: string;
	starts_at: string;
	ends_at: string;
	created_by_name: string | null;
	created_at: string;
	updated_at: string;
	/** The configuration items it is limited to; empty means it applies to every change. */
	item_ids: string[];
	/** Those of them the reader may see (with cmdb:read), with their names. */
	items: CISummary[];
}

/** The type's prefix of a reference: INC for INC42. */
export const prefixOf = (t: Pick<Ticket, 'reference' | 'number'>) => t.reference.slice(0, t.reference.length - String(t.number).length);

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
	/** Set when an app wrote the comment: the person it acted for (author_name is then the app). */
	on_behalf_of_name?: string;
	body: string;
	internal: boolean;
	created_at: string;
	pending?: boolean;
}

/** Permission names, as the server checks them (internal/authz). */
export const P = {
	commentsInternal: 'comments:internal',
	workflowsReadAll: 'workflows:read_all',
	usersManage: 'users:manage',
	groupsManage: 'groups:manage',
	rolesManage: 'roles:manage',
	freezesManage: 'freezes:manage',
	calendarsManage: 'calendars:manage',
	cmdbRead: 'cmdb:read',
	cmdbManage: 'cmdb:manage',
	settingsManage: 'settings:manage',
	knowledgeWrite: 'knowledge:write',
	knowledgePublish: 'knowledge:publish',
	workflowsEdit: 'workflows:edit'
} as const;

/** Whether someone may do something. The server decides for real; this only shows and hides things. */
export const can = (u: User | null | undefined, perm: string) => !!u?.permissions?.includes(perm);

/** A record verb: what records:<verb>:<type> allows on records of that type. */
export type Verb = 'read' | 'create' | 'update' | 'delete';

/** Whether someone may do verb to records of a type, by name or for every type (records:update:*). */
export const canRecord = (u: User | null | undefined, verb: Verb, type: string) =>
	can(u, `records:${verb}:${type}`) || can(u, `records:${verb}:*`);

/** Whether someone may do verb to records of at least one type: reading any is what makes someone staff in the UI. */
export const canRecordAny = (u: User | null | undefined, verb: Verb) =>
	!!u?.permissions?.some((p) => p.startsWith(`records:${verb}:`));

/** A record type in use (an installed app defines its lifecycle), and whether the person may file one. */
export interface RecordType {
	type: string;
	prefix: string;
	label: string;
	can_file: boolean;
	can_file_for_others: boolean;
}

/** Whether someone may reach anything in the admin section. */
export const canAdmin = (u: User | null | undefined) =>
	can(u, P.usersManage) || can(u, P.groupsManage) || can(u, P.rolesManage) || can(u, P.calendarsManage) || can(u, P.settingsManage);

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
	start?: string;
	graph?: GraphStep[];
	vars?: Record<string, unknown>;
	steps?: RunStep[];
}

/** One way out of a step. kind is absent for the ordinary "next", and 'then' | 'else' | 'timeout' for the branch a
 * condition, wait or approval step can take. */
export interface GraphEdge {
	to: string;
	kind?: 'then' | 'else' | 'timeout';
}

export interface GraphStep {
	id: string;
	type: string;
	next: GraphEdge[];
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

/** One pending "approval" workflow step asking this person to decide. Answering it is not staff-only: a workflow
 * can name anyone as an approver. */
export interface Approval {
	id: string;
	execution_id: string;
	step_id: string;
	/** "app/workflow". */
	workflow: string;
	/** The ticket it is about, if any: empty for a workflow with none, or if it has since been removed. */
	reference: string;
	title: string;
	/** The group they are asked to decide for; the first of its members to answer decides for it. */
	group?: string;
	created_at: string;
}

/** A hint that something needs this person's attention. The server fills in title, body and link (from an app's
 * template, for kind "template"), so the UI keeps no copy of what each kind says. */
export interface Notification {
	id: string;
	kind: 'approval_pending' | 'template';
	approval_id?: string;
	/** The ticket a template notification is about. */
	task_id?: string;
	reference?: string;
	title: string;
	body?: string;
	link: string;
	created_at: string;
	read_at?: string;
}

/** A team: what a ticket is routed to, and what group('name') in a workflow asks. */
export interface Group {
	id: string;
	name: string;
	description: string;
	member_count: number;
	created_at: string;
	updated_at: string;
}

export interface Member extends User {
	added_at: string;
	/** Set for someone who can no longer sign in; group() approvals leave them out. */
	deactivated_at: string | null;
}

/** Someone as an admin sees them in the user list. */
export interface Person extends User {
	created_at: string;
	/** Set when an admin deactivated them: they cannot sign in, and pickers leave them out. */
	deactivated_at: string | null;
}

export interface PersonDetail extends Person {
	groups: { id: string; name: string }[];
	roles: HeldRole[];
}

export const roleLabel: Record<Role, string> = { admin: 'Admin', agent: 'Agent', end_user: 'End user' };

export interface GroupDetail extends Group {
	members: Member[];
	/** Roles granted to the group: everyone in it holds them. */
	roles: HeldRole[];
}

/** A named set of permissions: a built-in base role, or one an app ships in roles/. */
export interface RoleInfo {
	name: string;
	label: string;
	description: string;
	permissions: string[];
	/** The app that defines it; empty for built-in roles. */
	app: string;
	/** Base roles are set on each person, never granted. */
	built_in: boolean;
	grants: number;
}

export interface Grant {
	id: string;
	user_id?: string;
	user_name?: string;
	user_email?: string;
	group_id?: string;
	group_name?: string;
	created_at: string;
}

export interface RoleDetail extends RoleInfo {
	granted_to: Grant[];
}

/** A role someone holds on top of their base role: granted to them, or (group_id set) through a group. */
export interface HeldRole {
	grant_id: string;
	role: string;
	label: string;
	group_id?: string;
	group_name?: string;
}

export interface PermissionInfo {
	name: string;
	description: string;
}

/** What a configuration item is in use for: planned, active, maintenance or retired. */
export type CIStatus = 'planned' | 'active' | 'maintenance' | 'retired';

/** A configuration item as lists show it. */
export interface CISummary {
	id: string;
	class: string;
	/** The class's label; the class name itself when no installed app defines it any more. */
	class_label: string;
	name: string;
	status: CIStatus;
	support_group_name: string | null;
}

/** One configuration item, from GET /cis/{id}. */
export interface CIDetail extends CISummary {
	description: string;
	owner_group_id: string | null;
	owner_group_name: string | null;
	support_group_id: string | null;
	fields: Record<string, FieldValue>;
	created_by_name: string | null;
	created_at: string;
	updated_at: string;
	relationships: CIRelation[];
	tickets: CITicket[];
}

/** A relationship as seen from one item: label reads from it ("Depends on", or "Used by" when it comes second). */
export interface CIRelation {
	id: string;
	type: string;
	label: string;
	outgoing: boolean;
	impact: boolean;
	item: CISummary;
}

/** A record linked to an item. */
export interface CITicket {
	id: string;
	reference: string;
	type: string;
	title: string;
	status: Status;
	state_category: Category;
	priority: 1 | 2 | 3 | 4;
	updated_at: string;
}

/** A kind of configuration item: its fields are those of the classes it extends first, then its own. */
export interface CIClass {
	name: string;
	label: string;
	description?: string;
	extends?: string;
	ancestors: string[];
	fields: FormField[];
	app: string;
}

/** A way items relate: label reads from the first item, reverse from the second; impact spreads along it. */
export interface CIRelationshipType {
	name: string;
	label: string;
	reverse: string;
	impact: boolean;
}

/** What configuration items can be, from GET /cmdb/model. */
export interface CMDBModel {
	classes: CIClass[];
	relationships: CIRelationshipType[];
	statuses: { value: CIStatus; label: string }[];
}

/** An item trouble reaches, how far away, and through which item. */
export interface CIAffected extends CISummary {
	depth: number;
	via_id: string;
	via_name: string;
}

/** What trouble with some items reaches, nearest first. */
export interface CIImpact {
	items: CIAffected[];
	truncated: boolean;
}

/** How the instance sends email, from GET /settings/email. The password is never sent back. */
export interface EmailSettings {
	enabled: boolean;
	host: string;
	port: number;
	tls: 'starttls' | 'tls' | 'none';
	username: string;
	from_address: string;
	from_name: string;
	base_url: string;
	approvals: boolean;
	password_set: boolean;
	password_readable: boolean;
	password_updated_at: string | null;
	password_updated_by: string | null;
	key_configured: boolean;
}

/** One email in the delivery log. */
export interface EmailDelivery {
	id: string;
	to_address: string;
	user_name: string | null;
	kind: 'notification' | 'test';
	what?: string;
	status: 'queued' | 'sent' | 'failed' | 'skipped';
	detail: string;
	attempts: number;
	created_at: string;
	updated_at: string;
}

/** The signed-in person's own preferences. */
export interface Account {
	email: string;
	email_notifications: boolean;
	email_enabled: boolean;
}

/** Where a knowledge article is: drafted, waiting for review, published, or retired. */
export type ArticleStatus = 'draft' | 'review' | 'published' | 'retired';

/** A knowledge article, from GET /kb/articles/{id}. */
export interface Article {
	id: string;
	reference: string;
	number: number;
	title: string;
	/** Markdown, as written. Render it with lib/markdown, which sanitises it. */
	body: string;
	category: string;
	tags: string[];
	audience: 'everyone' | 'internal';
	status: ArticleStatus;
	version: number;
	author_name: string | null;
	updated_by_name: string | null;
	created_at: string;
	updated_at: string;
	published_at: string | null;
	/** The status changes the reader may make now. */
	moves?: ArticleStatus[];
	/** For staff: the records it is linked to. */
	tickets?: { id: string; reference: string; type: string; title: string; status: Status; state_category: Category }[];
}

/** An article in a list or search result. The snippet marks matches between ⸢ and ⸣ (lib/components/Snippet). */
export interface ArticleHit {
	id: string;
	reference: string;
	title: string;
	category: string;
	tags: string[];
	audience: 'everyone' | 'internal';
	status: ArticleStatus;
	updated_at: string;
	snippet: string;
}

/** An article linked to a record. */
export interface ArticleRef {
	id: string;
	reference: string;
	title: string;
	status: ArticleStatus;
	audience: 'everyone' | 'internal';
}

/** One saved version of an article; the list leaves out the content. */
export interface ArticleVersion {
	version: number;
	title: string;
	status: ArticleStatus;
	saved_at: string;
	saved_by_name: string | null;
	body?: string;
	category?: string;
	tags?: string[];
	audience?: string;
}

/** One thing that happened to a record or an item (GET /tickets/{id}/history, /cis/{id}/history). */
export interface HistoryEntry {
	id: number;
	at: string;
	actor_name: string | null;
	actor_app?: string;
	on_behalf_of_name?: string;
	action: 'created' | 'changed' | 'linked' | 'unlinked' | 'deleted';
	field: string;
	label: string;
	old: unknown;
	new: unknown;
	old_text: string;
	new_text: string;
	kind: 'text' | 'time' | 'long';
}
