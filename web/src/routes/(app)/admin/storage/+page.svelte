<script lang="ts">
	import { onMount } from 'svelte';
	import { api } from '$lib/api';
	import { bytes, when } from '$lib/format';
	import type { StorageSettings } from '$lib/types';
	import { ui } from '$lib/ui.svelte';

	type Draft = Omit<StorageSettings, 'max_size'> & { max_amount: number; max_unit: number };

	const GiB = 1024 ** 3;
	const units = [
		{ label: 'MB', value: 1024 ** 2 },
		{ label: 'GB', value: GiB },
		{ label: 'TB', value: 1024 ** 4 }
	];
	/** The settings PUT takes; the rest of StorageSettings is about the secret key and this server. */
	const settingKeys = ['backend', 's3_endpoint', 's3_region', 's3_bucket', 's3_prefix', 's3_path_style', 's3_access_key_id', 's3_direct'] as const;

	let current = $state<StorageSettings | null>(null);
	let draft = $state<Draft | null>(null);
	let secretKey = $state('');
	let clearSecret = $state(false);
	let loadError = $state('');
	let saving = $state(false);
	let saveError = $state('');
	let testing = $state(false);
	let testResult = $state<{ ok: boolean; detail?: string; warnings: string[] } | null>(null);

	function toDraft(s: StorageSettings): Draft {
		const unit = [...units].reverse().find((u) => s.max_size % u.value === 0) ?? units[0];
		return { ...s, max_amount: s.max_size / unit.value, max_unit: unit.value };
	}

	async function load() {
		try {
			current = await api<StorageSettings>('/api/v1/settings/storage');
			draft = toDraft(current);
			loadError = '';
		} catch (e) {
			loadError = e instanceof Error ? e.message : 'Could not load the storage settings';
		}
	}
	onMount(load);

	async function save(e: Event) {
		e.preventDefault();
		if (!draft || saving) return;
		saving = true;
		saveError = '';
		testResult = null;
		const body: Record<string, unknown> = Object.fromEntries(settingKeys.map((k) => [k, draft![k]]));
		body.max_size = Math.round(Number(draft.max_amount) * draft.max_unit);
		if (clearSecret) body.s3_secret_key = null;
		else if (secretKey) body.s3_secret_key = secretKey;
		try {
			current = await api<StorageSettings>('/api/v1/settings/storage', { method: 'PUT', body });
			draft = toDraft(current);
			secretKey = '';
			clearSecret = false;
			ui.announce = 'Storage settings saved';
		} catch (err) {
			saveError = err instanceof Error ? err.message : 'The settings didn’t save';
		} finally {
			saving = false;
		}
	}

	async function test() {
		testing = true;
		testResult = null;
		try {
			testResult = await api<{ ok: boolean; detail?: string; warnings: string[] }>('/api/v1/settings/storage/test', { method: 'POST' });
		} catch (err) {
			testResult = { ok: false, detail: err instanceof Error ? err.message : 'Could not test', warnings: [] };
		} finally {
			testing = false;
		}
	}

	const dirty = $derived(!!draft && !!current && (JSON.stringify(draft) !== JSON.stringify(toDraft(current)) || !!secretKey || clearSecret));
	const usage = (b: 'local' | 's3') => current?.usage.find((u) => u.backend === b);
	const origin = typeof location === 'undefined' ? '' : location.origin;
	const cors = $derived(
		JSON.stringify(
			[{ AllowedOrigins: [origin], AllowedMethods: ['GET', 'PUT'], AllowedHeaders: ['*'], ExposeHeaders: ['ETag'], MaxAgeSeconds: 3600 }],
			null,
			2
		)
	);
</script>

<svelte:head><title>Storage – ServiceLater</title></svelte:head>

<div class="page">
	<h1>Storage</h1>
	<p class="lede">Where files attached to records are kept. Files already stored stay where they are when this changes, and can still be downloaded.</p>

	{#if loadError}
		<p class="error" role="alert">{loadError}</p>
	{:else if draft && current}
		<form class="settings" onsubmit={save} aria-labelledby="where-h">
			<fieldset class="wide where">
				<legend id="where-h">New files go to</legend>
				<label class="choice">
					<input type="radio" name="backend" value="local" bind:group={draft.backend} />
					<span>
						<strong>A folder on the server</strong>
						<span class="hint">
							<code>{current.local_dir || '(none set)'}</code>{#if usage('local')}, holding {usage('local')!.files} files ({bytes(usage('local')!.bytes)}){/if}
						</span>
					</span>
				</label>
				<label class="choice">
					<input type="radio" name="backend" value="s3" bind:group={draft.backend} />
					<span>
						<strong>An S3 bucket</strong>
						<span class="hint">
							Amazon S3 or any S3-compatible service (MinIO, Garage, Ceph){#if usage('s3')}, holding {usage('s3')!.files} files ({bytes(usage('s3')!.bytes)}){/if}
						</span>
					</span>
				</label>
			</fieldset>

			{#if draft.backend === 'local'}
				<p class="note wide" role="note">
					The folder is set where the server is deployed (<code>SL_ATTACHMENTS_DIR</code>). With more than one server, every api and worker server must
					mount the same shared folder (NFS, EFS, CephFS, or a ReadWriteMany volume on Kubernetes). A server without it refuses to store files rather
					than keep them where the others cannot read them. If you cannot share a folder, use a bucket.
				</p>
			{:else}
				{#if !current.key_configured}
					<p class="warn wide" role="note">
						This server has no secrets key, so it cannot store a secret access key. Set <code>SL_SECRETS_KEY</code> or <code>SL_SECRETS_KEY_FILE</code>
						on every server, or leave the access key empty to use the servers’ own credentials.
					</p>
				{:else if current.s3_secret_key_set && !current.s3_secret_key_readable}
					<p class="warn wide" role="note">The stored secret access key was encrypted with a key this server does not have. Enter it again.</p>
				{/if}
				<label class="field wide">
					<span>Endpoint</span>
					<input type="url" bind:value={draft.s3_endpoint} placeholder="https://s3.amazonaws.com" />
					<small class="help">The service’s address: https://s3.amazonaws.com, or your own, such as http://minio:9000.</small>
				</label>
				<label class="field">
					<span>Bucket</span>
					<input bind:value={draft.s3_bucket} autocomplete="off" />
				</label>
				<label class="field">
					<span>Region</span>
					<input bind:value={draft.s3_region} placeholder="us-east-1" />
				</label>
				<label class="field">
					<span>Key prefix</span>
					<input bind:value={draft.s3_prefix} placeholder="Optional, such as servicelater/" />
				</label>
				<label class="check align"><input type="checkbox" bind:checked={draft.s3_path_style} /> Path-style addresses (most self-hosted services)</label>
				<label class="field">
					<span>Access key ID</span>
					<input bind:value={draft.s3_access_key_id} autocomplete="off" placeholder="Empty: the servers’ own credentials" />
				</label>
				<div class="field">
					<label for="s3-secret">Secret access key</label>
					<input
						id="s3-secret"
						type="password"
						bind:value={secretKey}
						autocomplete="new-password"
						disabled={!current.key_configured || clearSecret || !draft.s3_access_key_id}
						placeholder={current.s3_secret_key_set ? 'Stored: type a new one to replace it' : ''}
						aria-describedby="secret-help"
					/>
					<small class="help" id="secret-help">
						{current.s3_secret_key_set && current.s3_secret_key_updated_at
							? `Stored encrypted on ${when(current.s3_secret_key_updated_at)}. It is never shown again.`
							: 'Stored encrypted, and never shown again.'}
					</small>
				</div>
				{#if current.s3_secret_key_set}
					<label class="check wide"><input type="checkbox" bind:checked={clearSecret} /> Remove the stored secret access key</label>
				{/if}
				<label class="check wide">
					<input type="checkbox" bind:checked={draft.s3_direct} /> Browsers upload to and download from the bucket themselves
				</label>
				<div class="wide help">
					{#if draft.s3_direct}
						<p>Files then never pass through ServiceLater. The bucket must be reachable from people’s browsers, and its CORS rules must allow this site:</p>
						<pre>{cors}</pre>
					{:else}
						<p>Files pass through ServiceLater on their way, so the bucket need only be reachable from the servers.</p>
					{/if}
				</div>
			{/if}

			<div class="field">
				<label for="max-size">Largest file</label>
				<div class="size">
					<input id="max-size" type="number" min="1" step="any" bind:value={draft.max_amount} />
					<select bind:value={draft.max_unit} aria-label="Unit">
						{#each units as u (u.value)}<option value={u.value}>{u.label}</option>{/each}
					</select>
				</div>
			</div>

			{#if saveError}<p class="error wide" role="alert">{saveError}</p>{/if}
			<div class="wide buttons">
				<button class="btn primary" disabled={!dirty || saving}>Save</button>
				<button class="btn" type="button" disabled={testing || dirty} onclick={test} title={dirty ? 'Save the settings first' : undefined}>
					Test storage
				</button>
			</div>
			<div class="wide" role="status">
				{#if testing}<p class="hint">Testing…</p>{/if}
				{#if testResult}
					{#if testResult.ok}
						<p class="ok">Storage works: this server wrote, read back and removed a file.</p>
					{:else}
						<p class="error">Storage does not work: {testResult.detail}</p>
					{/if}
					{#each testResult.warnings as w, i (i)}<p class="warn">{w}</p>{/each}
				{/if}
			</div>
		</form>
	{/if}
</div>

<style>
	.page {
		height: 100%;
		overflow-y: auto;
		padding: 1rem 1.25rem 2rem;
		max-width: 48rem;
	}
	h1 {
		font: 800 1.6rem/1.2 var(--font-display);
	}
	.lede {
		margin: 0.25rem 0 1rem;
		color: var(--ink-2);
	}
	.settings {
		display: grid;
		grid-template-columns: repeat(2, minmax(0, 1fr));
		gap: 0.9rem 1rem;
		align-items: start;
		padding: 1.25rem 0;
		border-top: 1px solid var(--rule);
	}
	.wide {
		grid-column: 1 / -1;
	}
	.where {
		display: grid;
		gap: 0.6rem;
		margin: 0;
		padding: 0;
		border: 0;
	}
	legend {
		font: 700 1.25rem/1.2 var(--font-display);
		margin-bottom: 0.5rem;
	}
	.choice {
		display: flex;
		align-items: flex-start;
		gap: 0.6rem;
	}
	.choice input {
		margin-top: 0.3rem;
	}
	.choice > span {
		display: grid;
		gap: 0.1rem;
		min-width: 0;
		overflow-wrap: anywhere;
	}
	.check {
		display: flex;
		align-items: center;
		gap: 0.5rem;
	}
	.align {
		align-self: end;
		min-height: 2.5rem;
	}
	.size {
		display: flex;
		gap: 0.5rem;
	}
	.size input {
		flex: 1;
		min-width: 0;
	}
	.size select {
		width: auto;
	}
	.help,
	.hint {
		color: var(--ink-2);
		font-size: 0.85rem;
	}
	pre {
		margin-top: 0.4rem;
		padding: 0.6rem 0.75rem;
		background: var(--raised);
		border: 1px solid var(--rule);
		border-radius: var(--radius);
		color: var(--ink);
		font-size: 0.8rem;
		overflow-x: auto;
	}
	.note,
	.warn {
		padding: 0.6rem 0.75rem;
		border-left: 3px solid var(--marker);
		background: var(--raised);
	}
	.note {
		border-left-color: var(--rule);
		color: var(--ink-2);
		font-size: 0.9rem;
	}
	.warn {
		margin-top: 0.5rem;
	}
	.buttons {
		display: flex;
		gap: 0.5rem;
	}
	.error {
		color: var(--red);
		font-weight: 600;
	}
	.ok {
		font-weight: 600;
	}
	@media (max-width: 40rem) {
		.settings {
			grid-template-columns: minmax(0, 1fr);
		}
	}
</style>
