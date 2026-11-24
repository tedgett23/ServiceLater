<script lang="ts">
	import { onDestroy, onMount } from 'svelte';
	import { api, download } from '$lib/api';
	import { when } from '$lib/format';
	import { instance } from '$lib/instance.svelte';
	import { ui } from '$lib/ui.svelte';

	type Settings = {
		enabled: boolean;
		cron: string;
		timezone: string;
		destination: 'folder' | 's3';
		attachments: boolean;
		keep_last: number;
		keep_days: number;
		keep_weeks: number;
	};
	type Snapshot = {
		id: string;
		kind: 'scheduled' | 'manual' | 'uploaded';
		status: 'pending' | 'running' | 'ready' | 'failed';
		error: string;
		destination: 'folder' | 's3';
		name: string;
		size: number;
		include_attachments: boolean;
		taken_at: string | null;
		schema_version: number | null;
		key_id: string;
		instance: string;
		rows: number;
		attachment_files: number;
		requested_by_name: string;
		created_at: string;
	};
	type Restore = { id: string; snapshot_taken_at: string | null; status: string; error: string; requested_by_name: string; created_at: string; finished_at: string | null };
	type Backups = {
		settings: Settings;
		key_configured: boolean;
		key_id: string;
		folder: string;
		s3_configured: boolean;
		schema_version: number;
		snapshots: Snapshot[];
		restores: Restore[];
	};

	let data = $state<Backups | null>(null);
	let form = $state<Settings | null>(null);
	let error = $state('');
	let busy = $state(false);
	let restoring = $state<string | null>(null);
	let confirmText = $state('');
	let changeSchema = $state(false);
	let deleting = $state<string | null>(null);
	let timer: ReturnType<typeof setTimeout> | null = null;

	async function load() {
		try {
			const d = await api<Backups>('/api/v1/backups');
			data = d;
			form ??= { ...d.settings };
		} catch (e) {
			error = e instanceof Error ? e.message : 'Could not load the backups';
		}
		// While a snapshot is being taken, look again every few seconds.
		if (timer) clearTimeout(timer);
		if (data?.snapshots.some((s) => s.status === 'pending' || s.status === 'running')) timer = setTimeout(load, 3000);
	}
	onMount(load);
	onDestroy(() => timer && clearTimeout(timer));

	const dirty = $derived(!!form && !!data && JSON.stringify(form) !== JSON.stringify(data.settings));
	const confirmWord = $derived(instance.name || 'restore');

	async function run(what: () => Promise<unknown>, done: string) {
		busy = true;
		error = '';
		try {
			await what();
			ui.announce = done;
			await load();
		} catch (e) {
			error = e instanceof Error ? e.message : 'That didn’t work';
		} finally {
			busy = false;
		}
	}

	async function saveSettings(e: Event) {
		e.preventDefault();
		await run(async () => {
			const d = await api<Backups>('/api/v1/backups/settings', { method: 'PUT', body: form });
			form = { ...d.settings };
		}, 'Backup settings saved');
	}

	function take() {
		void run(() => api('/api/v1/backups', { method: 'POST' }), 'Snapshot asked for');
	}

	async function upload(e: Event) {
		const input = e.currentTarget as HTMLInputElement;
		const file = input.files?.[0];
		input.value = '';
		if (!file) return;
		await run(() => api('/api/v1/backups/upload', { method: 'POST', body: new Blob([file], { type: 'application/octet-stream' }) }), `${file.name} uploaded`);
	}

	function startRestore(s: Snapshot) {
		restoring = s.id;
		confirmText = '';
		changeSchema = false;
	}

	async function restore(s: Snapshot) {
		busy = true;
		error = '';
		try {
			await api(`/api/v1/backups/${s.id}/restore`, { method: 'POST', body: { confirm: confirmText, change_schema: changeSchema } });
			restoring = null;
			await instance.load(); // shows the maintenance screen
		} catch (e) {
			error = e instanceof Error ? e.message : 'The restore was not started';
		} finally {
			busy = false;
		}
	}

	const size = (n: number) =>
		n < 1024 ? `${n} B` : n < 1024 ** 2 ? `${(n / 1024).toFixed(0)} KB` : n < 1024 ** 3 ? `${(n / 1024 ** 2).toFixed(1)} MB` : `${(n / 1024 ** 3).toFixed(2)} GB`;
	const kindLabel = { scheduled: 'Scheduled', manual: 'Taken by hand', uploaded: 'Uploaded' } as const;
	const olderSchema = (s: Snapshot) => !!data && s.schema_version !== null && s.schema_version < data.schema_version;
	const newerSchema = (s: Snapshot) => !!data && s.schema_version !== null && s.schema_version > data.schema_version;
	const otherKey = (s: Snapshot) => !!data && s.key_id !== '' && s.key_id !== data.key_id;
</script>

<svelte:head><title>Backups – ServiceLater</title></svelte:head>

<div class="page">
	<h1>Backups</h1>
	<p class="lede">
		Snapshots of the whole instance: every record, setting and app, from one moment, encrypted under the secrets key. Restoring
		one replaces everything. For recovery to any second, also use your database host’s backups or continuous archiving
		(docs/deploy.md, Backups).
	</p>

	{#if error}<p class="error" role="alert">{error}</p>{/if}

	{#if data && form}
		{#if !data.key_configured}
			<p class="warn" role="note">
				This server has no secrets key, so it can neither take nor read snapshots. Set <code>SL_SECRETS_KEY</code> (make one with
				<code>servicelater secrets gen-key</code>) on every server, and keep it safe: a snapshot cannot be restored without it.
			</p>
		{:else}
			<p class="note">
				Snapshots are encrypted under the secrets key <code>{data.key_id}</code>. Keep a copy of it apart from the snapshots: restoring one
				needs that key, as does every stored password and token.
			</p>
		{/if}

		<div class="actions">
			<button class="btn primary" type="button" disabled={busy || !data.key_configured} onclick={take}>Take a snapshot now</button>
			<label class="btn upload" class:disabled={busy || !data.key_configured}>
				Upload a snapshot
				<input type="file" accept=".slsnap" disabled={busy || !data.key_configured} onchange={upload} />
			</label>
		</div>

		<section aria-labelledby="snaps-h">
			<h2 id="snaps-h">Snapshots</h2>
			{#if data.snapshots.length === 0}
				<p class="hint">None yet.</p>
			{:else}
				<ul class="snaps">
					{#each data.snapshots as s (s.id)}
						<li class:failed={s.status === 'failed'}>
							<div class="what">
								<strong>{s.taken_at ? when(s.taken_at) : when(s.created_at)}</strong>
								<span class="tag">{kindLabel[s.kind]}</span>
								{#if s.status === 'pending' || s.status === 'running'}<span class="tag busy">{s.status === 'pending' ? 'Waiting for the worker' : 'Being taken'}</span>{/if}
								{#if s.status === 'failed'}<span class="tag bad">Failed</span>{/if}
							</div>
							<div class="meta">
								{#if s.status === 'ready'}
									<span>{size(s.size)}</span>
									<span>{s.rows.toLocaleString()} rows</span>
									{#if s.include_attachments}<span>{s.attachment_files} files</span>{/if}
									{#if s.instance}<span>from {s.instance}</span>{/if}
									<span>{s.destination === 's3' ? 'S3' : 'folder'}</span>
								{/if}
								<span>{s.kind === 'scheduled' ? 'by the schedule' : `by ${s.requested_by_name}`}</span>
							</div>
							{#if s.error}<p class="err">{s.error}</p>{/if}
							{#if s.status === 'ready' || s.status === 'failed'}
								<div class="row-actions">
									{#if s.status === 'ready'}
										<button class="btn" type="button" onclick={() => run(() => download(`/api/v1/backups/${s.id}/file`, s.name), 'Download started')}>Download</button>
										<button class="btn" type="button" aria-label="Restore the snapshot of {s.taken_at ? when(s.taken_at) : ''}" onclick={() => startRestore(s)}>Restore</button>
									{/if}
									{#if deleting === s.id}
										<button class="btn reject" type="button" disabled={busy} onclick={() => run(() => api(`/api/v1/backups/${s.id}`, { method: 'DELETE' }), 'Snapshot deleted')}>Delete it</button>
										<button class="btn" type="button" onclick={() => (deleting = null)}>Keep it</button>
									{:else}
										<button class="btn quiet-btn" type="button" onclick={() => (deleting = s.id)}>Delete</button>
									{/if}
								</div>
							{/if}
							{#if restoring === s.id}
								<form class="restore" onsubmit={(e) => (e.preventDefault(), restore(s))} aria-label="Restore this snapshot">
									{#if newerSchema(s)}
										<p class="err">This snapshot is from a newer version of ServiceLater. Update this one first.</p>
									{:else if otherKey(s)}
										<p class="err">This snapshot is encrypted under the key {s.key_id}; this server’s is {data.key_id}. Add that key to SL_SECRETS_OLD_KEYS to restore it.</p>
									{:else}
										<p>
											<strong>Everything in {instance.name || 'this instance'} is replaced</strong> by what it was on {s.taken_at ? when(s.taken_at) : 'that day'}:
											records, people, settings and apps. What happened since is lost. Everyone is signed out, and the instance is unavailable
											until it finishes. A restore that fails changes nothing.
										</p>
										{#if olderSchema(s)}
											<label class="check">
												<input type="checkbox" bind:checked={changeSchema} />
												It is from an older version: migrate the database to that version, restore it, then migrate up again
											</label>
										{/if}
										<div class="field">
											<label for="confirm-{s.id}">Type <strong>{confirmWord}</strong> to confirm</label>
											<input id="confirm-{s.id}" bind:value={confirmText} autocomplete="off" />
										</div>
										<div class="row-actions">
											<button class="btn reject" disabled={busy || confirmText.trim() !== confirmWord || (olderSchema(s) && !changeSchema)}>Restore it</button>
											<button class="btn" type="button" onclick={() => (restoring = null)}>Cancel</button>
										</div>
									{/if}
								</form>
							{/if}
						</li>
					{/each}
				</ul>
			{/if}
		</section>

		<form onsubmit={saveSettings} aria-labelledby="sched-h">
			<h2 id="sched-h">Schedule</h2>
			<label class="check"><input type="checkbox" bind:checked={form.enabled} disabled={!data.key_configured} /> Take snapshots on a schedule</label>
			<div class="grid">
				<div class="field">
					<label for="b-cron">When (cron)</label>
					<input id="b-cron" bind:value={form.cron} placeholder="0 2 * * *" />
					<small class="hint">Minute hour day month weekday: 0 2 * * * is 2:00 every night.</small>
				</div>
				<div class="field">
					<label for="b-tz">Time zone</label>
					<input id="b-tz" bind:value={form.timezone} placeholder="UTC, or Europe/London" />
				</div>
				<div class="field">
					<label for="b-dest">Keep them in</label>
					<select id="b-dest" bind:value={form.destination}>
						<option value="folder">The snapshots folder</option>
						<option value="s3" disabled={!data.s3_configured}>The S3 bucket of Admin → Storage{data.s3_configured ? '' : ' (none set)'}</option>
					</select>
					{#if form.destination === 'folder'}<small class="hint path">{data.folder}</small>{/if}
				</div>
			</div>
			<label class="check"><input type="checkbox" bind:checked={form.attachments} /> With the attachment files kept in a folder (S3 attachments stay in their bucket)</label>
			<fieldset>
				<legend>Keep</legend>
				<div class="grid">
					<div class="field"><label for="b-last">The newest</label><input id="b-last" type="number" min="1" max="1000" bind:value={form.keep_last} /></div>
					<div class="field"><label for="b-days">One a day for (days)</label><input id="b-days" type="number" min="0" max="3650" bind:value={form.keep_days} /></div>
					<div class="field"><label for="b-weeks">One a week for (weeks)</label><input id="b-weeks" type="number" min="0" max="520" bind:value={form.keep_weeks} /></div>
				</div>
				<small class="hint">Scheduled snapshots beyond these are deleted. Snapshots taken by hand or uploaded stay until you delete them.</small>
			</fieldset>
			<div><button class="btn primary" disabled={!dirty || busy}>Save</button></div>
		</form>

		{#if data.restores.length}
			<section aria-labelledby="restores-h">
				<h2 id="restores-h">Restores</h2>
				<ul class="restores">
					{#each data.restores as r (r.id)}
						<li>
							<span>
								<strong>{r.status === 'done' ? 'Restored' : r.status === 'failed' ? 'Failed' : 'Restoring'}</strong>
								the snapshot of {r.snapshot_taken_at ? when(r.snapshot_taken_at) : 'an unknown time'}
							</span>
							<span class="hint">{when(r.created_at)} by {r.requested_by_name}</span>
							{#if r.error}<span class="err">{r.error}</span>{/if}
						</li>
					{/each}
				</ul>
			</section>
		{/if}
	{/if}
</div>

<style>
	.page {
		height: 100%;
		overflow-y: auto;
		padding: 1rem 1.25rem 2rem;
		max-width: 52rem;
	}
	h1 {
		font: 650 1.375rem/1.3 var(--font-display);
	}
	h2 {
		margin: 0;
		font: 650 1.0625rem/1.3 var(--font-display);
	}
	.lede {
		margin: 0.25rem 0 1rem;
		color: var(--ink-2);
	}
	section,
	form[aria-labelledby] {
		display: grid;
		gap: 0.75rem;
		padding: 1.25rem 0;
		border-top: 1px solid var(--rule);
	}
	.note,
	.warn {
		margin: 0 0 1rem;
		padding: 0.6rem 0.75rem;
		border-left: 3px solid var(--blue);
		background: var(--raised);
		font-size: 0.9rem;
	}
	.warn {
		border-left-color: var(--marker);
		background: var(--marker-soft);
		color: var(--marker-ink);
	}
	.actions,
	.row-actions {
		display: flex;
		flex-wrap: wrap;
		gap: 0.5rem;
	}
	.actions {
		margin-bottom: 1rem;
	}
	.upload {
		position: relative;
		cursor: pointer;
	}
	.upload input {
		position: absolute;
		inset: 0;
		opacity: 0;
		cursor: pointer;
	}
	.upload.disabled {
		opacity: 0.55;
		pointer-events: none;
	}
	.upload:focus-within {
		outline: 2px solid var(--blue);
		outline-offset: 2px;
	}
	.snaps,
	.restores {
		list-style: none;
		margin: 0;
		padding: 0;
	}
	.snaps li {
		display: grid;
		gap: 0.4rem;
		padding: 0.75rem 0;
		border-bottom: 1px solid var(--rule);
	}
	.snaps li:last-child {
		border-bottom: 0;
	}
	.snaps li.failed {
		border-left: 3px solid var(--red);
		padding-left: 0.6rem;
	}
	.what {
		display: flex;
		flex-wrap: wrap;
		align-items: baseline;
		gap: 0.5rem;
	}
	.tag {
		padding: 0.05rem 0.45rem;
		border-radius: 999px;
		background: var(--hover);
		color: var(--ink-2);
		font-size: 0.8rem;
	}
	.tag.busy {
		background: var(--marker-soft);
		color: var(--marker-ink);
	}
	.tag.bad {
		color: var(--red);
	}
	.meta {
		display: flex;
		flex-wrap: wrap;
		gap: 0.25rem 1rem;
		color: var(--ink-2);
		font-size: 0.875rem;
	}
	.restore {
		display: grid;
		gap: 0.6rem;
		padding: 0.75rem;
		border-left: 3px solid var(--red);
		background: var(--raised);
	}
	.restore p {
		margin: 0;
	}
	.restores li {
		display: grid;
		gap: 0.15rem;
		padding: 0.5rem 0;
		border-bottom: 1px solid var(--rule);
	}
	.grid {
		display: grid;
		grid-template-columns: repeat(auto-fill, minmax(13rem, 1fr));
		align-items: start;
		gap: 0.75rem 1rem;
	}
	.path {
		font-family: var(--font-mono);
		font-size: 0.8rem;
		overflow-wrap: anywhere;
	}
	fieldset {
		display: grid;
		gap: 0.5rem;
		margin: 0;
		padding: 0;
		border: 0;
		min-width: 0;
	}
	legend {
		margin-bottom: 0.35rem;
		font-weight: 700;
		font-size: 0.9rem;
	}
	.check {
		display: flex;
		align-items: center;
		gap: 0.5rem;
	}
	.hint {
		color: var(--ink-2);
		font-size: 0.85rem;
	}
	.err {
		margin: 0;
		color: var(--red);
		font-size: 0.875rem;
	}
	.error {
		color: var(--red);
		font-weight: 600;
	}
</style>
