import type { Attachment, Comment, HistoryEntry } from './types';

export type ActivityMode = 'comments' | 'history' | 'all';
type ActivityItem = { key: string; at: string; comment?: Comment; change?: HistoryEntry; file?: Attachment };

export function activityFeed(show: ActivityMode, comments: Comment[], history: HistoryEntry[], files: Attachment[]): ActivityItem[] {
	const here = new Set(files.map((f) => f.id));
	const fileOf = (e: HistoryEntry) => (e.new as { id?: string } | null)?.id ?? '';
	// Present files stand for their attachment event. Once removed, both history entries stay in the conversation.
	const changes = history.filter((e) => !(e.field === 'attachment' && e.action === 'linked' && here.has(fileOf(e))));
	const hs = (show === 'comments' ? changes.filter((e) => e.field === 'attachment') : changes).map((e) => ({
		key: `h${e.id}`, at: e.at, change: e
	}));
	if (show === 'history') return hs;
	return [
		...comments.map((c) => ({ key: `c${c.id}`, at: c.created_at, comment: c })),
		...files.map((f) => ({ key: `f${f.id}`, at: f.attached_at ?? f.created_at, file: f })),
		...hs
	].sort((a, b) => a.at.localeCompare(b.at));
}
