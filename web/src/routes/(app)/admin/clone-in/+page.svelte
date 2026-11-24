<script lang="ts">
	import { onDestroy, onMount } from 'svelte';
	import { api } from '$lib/api';
	import { when } from '$lib/format';
	import { instance } from '$lib/instance.svelte';
	import { ui } from '$lib/ui.svelte';
	import PersonPicker from '$lib/components/PersonPicker.svelte';

	type Header = { schema: number; taken_at: string; instance?: string; environment?: string; profile: string };
	type Kept = { name: string; email: string; outcome: 'matched' | 'added' | 'not_kept'; detail?: string };
	type SetAside = { app: string; path: string; reason: string; content: string };
	type PackageReport = { records: { type: string; label: string; kept: number; total: number }[]; people: number; cleared_total: number };
	type LoadReport = {
		source: Header;
		package: PackageReport;
		rows: Record<string, number>;
		people: Kept[];
		dropped: string[];
		identities: number;
		identities_dropped: number;
		api_keys: number;
		studio_kept: string[];
		set_aside: SetAside[];
		runs_cancelled: number;
		approvals_closed: number;
		clocks_settled: number;
		switched_off: string[];
		files: number;
		missing_files: number;
	};
	type Load = {
		id: string;
		status: 'uploaded' | 'pending' | 'running' | 'done' | 'failed';
		error: string;
		source: Header;
		size: number;
		keep_studio: boolean;
		keep_people: boolean;
		report: LoadReport | null;
		requested_by_name: string;
		created_at: string;
		finished_at: string | null;
	};
	type Preview = {
		schema: number;
		schema_here: number;
		report: PackageReport;
		people: Kept[];
		dropped: string[];
		studio_kept: string[];
		set_aside: SetAside[];
		problems: string[];
	};
	type Settings = { allow_into_production: boolean; hold_http: boolean; hold_schedules: boolean; hold_webhooks: boolean };
	type CloneIn = {
		loads: Load[];
		keep: { user_id: string; name: string; email: string }[];
		settings: Settings;
		switched: { label: string; on: boolean; page: string }[];
		environment: string;
		instance: string;
		key_configured: boolean;
		schema_version: number;
	};

	let data = $state<CloneIn | null>(null);
	let error = $state('');
	let busy = $state(false);
	let timer: ReturnType<typeof setTimeout> | null = null;

	// The package being previewed and applied.
	let open = $state<string | null>(null);
	let passphrase = $state('');
	let keepPeople = $state(true);
	let keepStudio = $state(true);
	let preview = $state<Preview | null>(null);
	let confirmText = $state('');
	let deleting = $state<string | null>(null);

	async function load() {
		try {
			data = await api<CloneIn>('/api/v1/clone-in');
		} catch (e) {
			error = e instanceof Error ? e.message : 'Could not load Clone in';
		}
		if (timer) clearTimeout(timer);
		if (data?.loads.some((l) => l.status === 'pending' || l.status === 'running')) timer = setTimeout(load, 3000);
	}
	onMount(load);
	onDestroy(() => timer && clearTimeout(timer));

	async function run(what: () => Promise<unknown>, done: string) {
		busy = true;
		error = '';
		try {
			await what();
			ui.announce = done;
			await load();
			return true;
		} catch (e) {
			error = e instanceof Error ? e.message : 'That didn’t work';
			return false;
		} finally {
			busy = false;
		}
	}

	async function upload(e: Event) {
		const input = e.currentTarget as HTMLInputElement;
		const file = input.files?.[0];
		input.value = '';
		if (!file) return;
		await run(() => api('/api/v1/clone-in/upload', { method: 'POST', body: new Blob([file], { type: 'application/octet-stream' }) }), `${file.name} uploaded`);
	}

	function start(l: Load) {
		open = l.id;
		passphrase = '';
		keepPeople = true;
		keepStudio = true;
		preview = null;
		confirmText = '';
		deleting = null;
	}

	async function runPreview(l: Load) {
		busy = true;
		error = '';
		preview = null;
		try {
			preview = await api<Preview>(`/api/v1/clone-in/${l.id}/preview`, {
				method: 'POST',
				body: { passphrase, keep_people: keepPeople, keep_studio: keepStudio }
			});
		} catch (e) {
			error = e instanceof Error ? e.message : 'The preview failed';
		} finally {
			busy = false;
		}
	}

	async function apply(l: Load) {
		busy = true;
		error = '';
		try {
			await api(`/api/v1/clone-in/${l.id}/apply`, {
				method: 'POST',
				body: { passphrase, keep_people: keepPeople, keep_studio: keepStudio, confirm: confirmText }
			});
			open = null;
			await instance.load(); // shows the maintenance screen
		} catch (e) {
			error = e instanceof Error ? e.message : 'The clone was not started';
		} finally {
			busy = false;
		}
	}

	async function saveSettings(next: Partial<Settings>, done: string) {
		if (!data) return;
		const body = { ...data.settings, ...next };
		await run(() => api('/api/v1/clone-in/settings', { method: 'PUT', body }), done);
	}

	const confirmWord = $derived(data?.instance || 'clone');
	const held = $derived(!!data && (data.settings.hold_http || data.settings.hold_schedules || data.settings.hold_webhooks));
	const size = (n: number) => (n < 1024 ** 2 ? `${Math.max(1, Math.round(n / 1024))} KB` : `${(n / 1024 ** 2).toFixed(1)} MB`);
	const envLabel: Record<string, string> = { production: 'Production', uat: 'UAT', development: 'Development', other: 'Test' };
	const statusLabel = { uploaded: 'Not applied', pending: 'Waiting for the worker', running: 'Loading', done: 'Loaded', failed: 'Failed' } as const;
	const outcomeLabel = { matched: 'production’s, with this instance’s password, roles, groups and keys', added: 'added as they are', not_kept: 'not kept' } as const;
	const from = (h: Header) => [h.instance || 'an unnamed instance', h.environment ? envLabel[h.environment] ?? h.environment : ''].filter(Boolean).join(', ');
</script>

{#snippet people(list: Kept[])}
	<ul class="plain">
		{#each list as k, i (i)}
			<li><strong>{k.name}</strong> <span class="hint">{k.email}</span> · {outcomeLabel[k.outcome]}{k.detail ? `: ${k.detail}` : ''}</li>
		{/each}
	</ul>
{/snippet}

{#snippet setAside(list: SetAside[])}
	<ul class="plain">
		{#each list as s, i (i)}
			<li>
				<details>
					<summary><span class="mono">{s.app}/{s.path}</span> · {s.reason}</summary>
					<pre>{s.content}</pre>
				</details>
			</li>
		{/each}
	</ul>
{/snippet}

<svelte:head><title>Clone in – ServiceLater</title></svelte:head>

<div class="page">
	<h1>Clone in</h1>
	<p class="lede">
		Load a package made on production (Admin → Clones there) into this instance. Its records, people and apps replace this
		instance’s; this instance keeps its own settings, secrets, single sign-on, the people listed below and, if you choose, its studio
		edits. People production also has take production’s id, so its records point at them.
	</p>

	{#if error}<p class="error" role="alert">{error}</p>{/if}

	{#if data}
		{#if data.environment === 'production'}
			<div class="warn" role="note">
				<p>
					This instance is marked <strong>production</strong> (Admin → Instance), so it refuses clones: loading one replaces every
					record. Mark it UAT or development first, or allow it here.
				</p>
				<label class="check">
					<input type="checkbox" checked={data.settings.allow_into_production} disabled={busy}
						onchange={(e) => saveSettings({ allow_into_production: e.currentTarget.checked }, 'Saved')} />
					Allow clones into this production instance
				</label>
			</div>
		{/if}

		<section aria-labelledby="switch-h">
			<h2 id="switch-h">Safety switch</h2>
			<p class="hint">
				Every clone turns these off, so this copy of production does not email customers, post to production’s channels, call
				production’s partners or read production’s mailbox. Turn each back on once it is pointed somewhere safe.
			</p>
			<ul class="switches">
				{#each data.switched as s (s.label)}
					<li><span>{s.label}</span> <span class="state" class:off={!s.on}>{s.on ? 'On' : 'Off'}</span> <a href={s.page}>Settings</a></li>
				{/each}
				<li>
					<label class="check"><input type="checkbox" checked={!data.settings.hold_http} disabled={busy}
							onchange={(e) => saveSettings({ hold_http: !e.currentTarget.checked }, 'Saved')} /> Outbound calls from workflows</label>
				</li>
				<li>
					<label class="check"><input type="checkbox" checked={!data.settings.hold_schedules} disabled={busy}
							onchange={(e) => saveSettings({ hold_schedules: !e.currentTarget.checked }, 'Saved')} /> Scheduled workflows</label>
				</li>
				<li>
					<label class="check"><input type="checkbox" checked={!data.settings.hold_webhooks} disabled={busy}
							onchange={(e) => saveSettings({ hold_webhooks: !e.currentTarget.checked }, 'Saved')} /> Incoming webhooks</label>
				</li>
			</ul>
			{#if held}<p class="hint">Unticked: held since the last clone.</p>{/if}
		</section>

		<section aria-labelledby="keep-h">
			<h2 id="keep-h">People kept on a clone</h2>
			<p class="hint">
				Test agents and integration accounts of this instance. They keep their password, roles, groups, phone number and API keys;
				if production has the same person (by email address, or a service account by name) they take production’s id. Whoever
				applies a clone is always kept.
			</p>
			{#if data.keep.length}
				<ul class="plain">
					{#each data.keep as k (k.user_id)}
						<li>
							<strong>{k.name}</strong> <span class="hint">{k.email}</span>
							<button class="btn quiet-btn" type="button" aria-label="Stop keeping {k.name}" disabled={busy}
								onclick={() => run(() => api(`/api/v1/clone-in/keep/${k.user_id}`, { method: 'DELETE' }), `${k.name} is no longer kept`)}>Remove</button>
						</li>
					{/each}
				</ul>
			{/if}
			<div class="picker field">
				<PersonPicker id="keep-person" label="Keep someone" value={null} name={null} exclude={data.keep.map((k) => k.user_id)} disabled={busy}
					onchange={(id, who) => id && run(() => api(`/api/v1/clone-in/keep/${id}`, { method: 'PUT' }), `${who} is kept on a clone`)} />
			</div>
		</section>

		<section aria-labelledby="packages-h">
			<div class="head">
				<h2 id="packages-h">Packages</h2>
				<label class="btn upload" class:disabled={busy || !data.key_configured}>
					Upload a package
					<input type="file" accept=".slclone" disabled={busy || !data.key_configured} onchange={upload} />
				</label>
			</div>
			{#if !data.key_configured}
				<p class="warn" role="note">This server has no secrets key, which holds a package’s passphrase while it loads. Set <code>SL_SECRETS_KEY</code>.</p>
			{/if}
			{#if data.loads.length === 0}
				<p class="hint">None yet.</p>
			{:else}
				<ul class="rows">
					{#each data.loads as l (l.id)}
						<li class:failed={l.status === 'failed'}>
							<div class="what">
								<strong>From {from(l.source)}</strong>
								<span class="tag mono">{l.source.profile}</span>
								<span class="tag" class:busy={l.status === 'pending' || l.status === 'running'} class:bad={l.status === 'failed'}>{statusLabel[l.status]}</span>
							</div>
							<div class="meta">
								<span>taken {when(l.source.taken_at)}</span>
								<span>{size(l.size)}</span>
								<span>uploaded {when(l.created_at)} by {l.requested_by_name}</span>
							</div>
							{#if l.error}<p class="err">{l.error}</p>{/if}
							{#if l.report}
								{@const r = l.report}
								<details>
									<summary>What it did</summary>
									<div class="report">
										<p>
											Loaded {Object.values(r.rows).reduce((a, b) => a + b, 0).toLocaleString()} rows{l.finished_at ? ` on ${when(l.finished_at)}` : ''}
											{r.files ? `, with ${r.files} attachment file${r.files === 1 ? '' : 's'}` : ''}.
											{#if r.missing_files}{r.missing_files} attachment{r.missing_files === 1 ? ' came' : 's came'} without a file.{/if}
										</p>
										{#if r.people.length}<h4>People kept</h4>{@render people(r.people)}{/if}
										{#if r.dropped.length}<h4>Could not follow them</h4><ul class="plain">{#each r.dropped as d, i (i)}<li>{d}</li>{/each}</ul>{/if}
										<ul class="plain">
											<li>{r.identities} single sign-on link{r.identities === 1 ? '' : 's'} kept{r.identities_dropped ? `, ${r.identities_dropped} for people production does not have dropped` : ''}</li>
											<li>{r.api_keys} API key{r.api_keys === 1 ? '' : 's'} kept</li>
											<li>{r.runs_cancelled} workflow run{r.runs_cancelled === 1 ? '' : 's'} still going cancelled, {r.approvals_closed} approval{r.approvals_closed === 1 ? '' : 's'} closed</li>
											<li>{r.clocks_settled} SLA clock{r.clocks_settled === 1 ? '' : 's'} caught up without alerting</li>
											<li>{r.switched_off.length ? `Turned off: ${r.switched_off.join(', ')}` : 'Nothing else was on to turn off'}; outbound calls, schedules and webhooks held</li>
											{#if r.studio_kept.length}<li>{r.studio_kept.length} studio edit{r.studio_kept.length === 1 ? '' : 's'} kept</li>{/if}
										</ul>
										{#if r.set_aside.length}<h4>Studio edits set aside</h4>{@render setAside(r.set_aside)}{/if}
									</div>
								</details>
							{/if}
							{#if l.status !== 'pending' && l.status !== 'running'}
								<div class="row-actions">
									<button class="btn" type="button" aria-label="Preview and apply the package from {from(l.source)}" onclick={() => start(l)}>
										{l.status === 'uploaded' ? 'Preview and apply' : 'Apply again'}
									</button>
									{#if deleting === l.id}
										<button class="btn reject" type="button" disabled={busy} onclick={() => run(() => api(`/api/v1/clone-in/${l.id}`, { method: 'DELETE' }), 'Package deleted')}>Delete it</button>
										<button class="btn" type="button" onclick={() => (deleting = null)}>Keep it</button>
									{:else}
										<button class="btn quiet-btn" type="button" onclick={() => (deleting = l.id)}>Delete</button>
									{/if}
								</div>
							{/if}
							{#if open === l.id}
								<form class="inset" onsubmit={(e) => (e.preventDefault(), runPreview(l))} aria-label="Apply this package">
									<div class="grid">
										<div class="field">
											<label for="ci-pass">Passphrase</label>
											<input id="ci-pass" type="password" bind:value={passphrase} autocomplete="off" />
										</div>
									</div>
									<label class="check"><input type="checkbox" bind:checked={keepPeople} /> Keep the people listed above</label>
									<label class="check"><input type="checkbox" bind:checked={keepStudio} /> Keep this instance’s studio edits (workflows, forms, catalog) over production’s</label>
									<div class="row-actions">
										<button class="btn" disabled={busy || !passphrase}>{busy && !preview ? 'Reading it…' : 'Preview'}</button>
										<button class="btn" type="button" onclick={() => (open = null)}>Cancel</button>
									</div>
									{#if preview}
										<div class="report" role="region" aria-label="Preview">
											{#if preview.problems.length}
												<ul class="plain err">{#each preview.problems as p, i (i)}<li>{p}</li>{/each}</ul>
											{/if}
											<table>
												<thead><tr><th scope="col">Comes in</th><th scope="col" class="num">Records</th></tr></thead>
												<tbody>
													{#each preview.report.records.filter((c) => c.kept > 0) as c (c.type)}
														<tr><td>{c.label}</td><td class="num">{c.kept.toLocaleString()}</td></tr>
													{/each}
													<tr><td>People</td><td class="num">{preview.report.people.toLocaleString()}</td></tr>
												</tbody>
											</table>
											{#if preview.schema < preview.schema_here}
												<p class="hint">The package is from an older version of ServiceLater; what it lacks takes its defaults.</p>
											{/if}
											<h4>People kept</h4>
											{@render people(preview.people)}
											{#if preview.dropped.length}<h4>Could not follow them</h4><ul class="plain">{#each preview.dropped as d, i (i)}<li>{d}</li>{/each}</ul>{/if}
											{#if preview.studio_kept.length}<p>{preview.studio_kept.length} studio edit{preview.studio_kept.length === 1 ? '' : 's'} kept.</p>{/if}
											{#if preview.set_aside.length}<h4>Studio edits set aside</h4>{@render setAside(preview.set_aside)}{/if}

											{#if preview.problems.length === 0}
												<div class="danger">
													<p>
														<strong>Every record in {data.instance || 'this instance'} is replaced</strong> by production’s. Everyone is signed out,
														and the instance is unavailable until it finishes. A clone that fails changes nothing.
													</p>
													<div class="field">
														<label for="ci-confirm">Type <strong>{confirmWord}</strong> to confirm</label>
														<input id="ci-confirm" bind:value={confirmText} autocomplete="off" />
													</div>
													<div>
														<button class="btn reject" type="button" disabled={busy || confirmText.trim() !== confirmWord} onclick={() => apply(l)}>
															Replace this instance’s records
														</button>
													</div>
												</div>
											{/if}
										</div>
									{/if}
								</form>
							{/if}
						</li>
					{/each}
				</ul>
			{/if}
		</section>
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
	h4 {
		margin: 0.5rem 0 0.25rem;
		font-size: 0.9rem;
	}
	.lede {
		margin: 0.25rem 0 1rem;
		color: var(--ink-2);
	}
	section {
		display: grid;
		gap: 0.75rem;
		padding: 1.25rem 0;
		border-top: 1px solid var(--rule);
	}
	section p {
		margin: 0;
	}
	.head {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: 1rem;
	}
	.warn {
		display: grid;
		gap: 0.5rem;
		margin: 0 0 1rem;
		padding: 0.6rem 0.75rem;
		border-left: 3px solid var(--marker);
		background: var(--marker-soft);
		color: var(--marker-ink);
		font-size: 0.9rem;
	}
	.warn p {
		margin: 0;
	}
	.switches,
	.plain,
	.rows {
		list-style: none;
		margin: 0;
		padding: 0;
	}
	.switches li {
		display: flex;
		align-items: center;
		gap: 0.75rem;
		padding: 0.3rem 0;
	}
	.switches li > span:first-child {
		min-width: 10rem;
	}
	.state {
		font-size: 0.8rem;
		padding: 0.05rem 0.45rem;
		border-radius: 999px;
		background: var(--hover);
	}
	.state.off {
		background: var(--marker-soft);
		color: var(--marker-ink);
	}
	.plain li {
		padding: 0.2rem 0;
		font-size: 0.9rem;
	}
	.picker {
		max-width: 24rem;
	}
	.rows > li {
		display: grid;
		gap: 0.4rem;
		padding: 0.75rem 0;
		border-bottom: 1px solid var(--rule);
	}
	.rows > li:last-child {
		border-bottom: 0;
	}
	.rows > li.failed {
		border-left: 3px solid var(--red);
		padding-left: 0.6rem;
	}
	.what {
		display: flex;
		flex-wrap: wrap;
		align-items: baseline;
		gap: 0.5rem;
	}
	.meta {
		display: flex;
		flex-wrap: wrap;
		gap: 0.25rem 1rem;
		color: var(--ink-2);
		font-size: 0.875rem;
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
	.mono,
	pre {
		font-family: var(--font-mono);
	}
	pre {
		margin: 0.4rem 0;
		padding: 0.5rem;
		max-height: 16rem;
		overflow: auto;
		font-size: 0.8rem;
		background: var(--raised);
		border-radius: 8px;
	}
	.row-actions {
		display: flex;
		flex-wrap: wrap;
		gap: 0.5rem;
	}
	.inset {
		display: grid;
		gap: 0.75rem;
		padding: 0.9rem;
		border: 1px solid var(--rule);
		border-left: 3px solid var(--blue);
		border-radius: 8px;
		background: var(--surface);
	}
	.report {
		display: grid;
		gap: 0.4rem;
		font-size: 0.9rem;
	}
	.report p {
		margin: 0;
	}
	.danger {
		display: grid;
		gap: 0.6rem;
		margin-top: 0.5rem;
		padding: 0.75rem;
		border-left: 3px solid var(--red);
		background: var(--raised);
	}
	.danger p {
		margin: 0;
	}
	.grid {
		display: grid;
		grid-template-columns: repeat(auto-fill, minmax(16rem, 1fr));
		gap: 0.75rem 1rem;
	}
	.check {
		display: flex;
		align-items: center;
		gap: 0.5rem;
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
	table {
		border-collapse: collapse;
		max-width: 22rem;
	}
	th,
	td {
		padding: 0.25rem 0.75rem 0.25rem 0;
		text-align: left;
		border-bottom: 1px solid var(--rule);
	}
	th {
		color: var(--ink-2);
		font-weight: 600;
	}
	.num {
		text-align: right;
		font-variant-numeric: tabular-nums;
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
