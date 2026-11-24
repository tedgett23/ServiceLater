import assert from 'node:assert/strict';
import test from 'node:test';
import { activityFeed } from '../src/lib/activity.ts';

test('files stay in the conversation before and after removal, without duplicating their history entry', () => {
	const comments = [{id: '1', created_at: '2026-01-02'}];
	const file = {id: 'photo', attached_at: '2026-01-01'};
	const attached = {id: '1', at: file.attached_at, action: 'linked', field: 'attachment', new: {id: file.id}};
	const removed = {id: '2', at: '2026-01-03', action: 'unlinked', field: 'attachment'};
	const changed = {id: '3', at: '2026-01-04', action: 'changed', field: 'status'};
	assert.deepEqual(activityFeed('all', comments, [attached, changed], [file]).map(i => i.key), ['fphoto','c1','h3']);
	assert.deepEqual(activityFeed('comments', comments, [attached, removed, changed], []).map(i => i.key), ['h1','c1','h2']);
	assert.deepEqual(activityFeed('history', comments, [attached, removed, changed], []).map(i => i.key), ['h1','h2','h3']);
});
