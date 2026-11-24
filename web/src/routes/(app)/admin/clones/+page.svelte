<script lang="ts">
	import { onDestroy, onMount } from 'svelte';
	import { api, download } from '$lib/api';
	import { when } from '$lib/format';
	import { ui } from '$lib/ui.svelte';

	type Exclusion = { record_type: string; when: string };
	type Profile = {
		record_types: string[];
		records: 'all' | 'open' | 'recent';
		recent_days: number;
		exclude: Exclusion[];
		internal_notes: boolean;
		attachments: 'none' | 'kept';
		attachment_max_mb: number;
		history: 'none' | 'recent' | 'all';
		history_days: number;
		logs: boolean;
		people: 'all' | 'referenced';
		passwords: boolean;
		mask: { names: boolean; emails: boolean; phones: boolean; text: boolean; fields: boolean };
	};
	type ProfileRow = { name: string; description: string; config: Profile; updated_at: string };
	type Report = {
		records: { type: string; label: string; kept: number; total: number }[];
		excluded: number;
		people: number;
		people_total: number;
		attachments: number;
		attachment_bytes: number;
		missing_files: number;
		cleared: { record: string; link: string; to: string }[];
		cleared_total: number;
	};
	type Package = {
		id: string;
		profile: string;
		status: 'pending' | 'running' | 'ready' | 'failed';
		error: string;
		destination: 'folder' | 's3';
		name: string;
		size: number;
		taken_at: string | null;
		rows: number;
		report: Report | null;
		requested_by_name: string;
		created_at: string;
	};
	type Clones = {
		profiles: ProfileRow[];
		packages: Package[];
		default: Profile;
		record_types: { type: string; label: string }[];
		masked_fields: string[];
		key_configured: boolean;
		s3_configured: boolean;
		folder: string;
	};

	let data = $state<Clones | null>(null);
	let error = $state('');
	let busy = $state(false);
	let timer: ReturnType<typeof setTimeout> | null = null;

	// The profile being edited: null for none, with isNew for one not saved yet.
	let editing = $state<{ isNew: boolean; name: string; description: string; config: Profile } | null>(null);
	let preview = $state<Report | null>(null);
	let previewing = $state(false);
	// The profile a package is being asked for.
	let packing = $state<string | null>(null);
	let passphrase = $state('');
	let again = $state('');
	let destination = $state<'folder' | 's3'>('folder');
	let deleting = $state<string | null>(null);

	async function load() {
		try {
			data = await api<Clones>('/api/v1/clones');
		} catch (e) {
			error = e instanceof Error ? e.message : 'Could not load the clones';
		}
		if (timer) clearTimeout(timer);
		if (data?.packages.some((p) => p.status === 'pending' || p.status === 'running')) timer = setTimeout(load, 3000);
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

	const copy = (p: Profile): Profile => JSON.parse(JSON.stringify(p));

	function newProfile() {
		if (!data) return;
		editing = { isNew: true, name: '', description: '', config: copy(data.default) };
		preview = null;
		packing = null;
	}

	function edit(p: ProfileRow) {
		editing = { isNew: false, name: p.name, description: p.description, config: copy(p.config) };
		preview = null;
		packing = null;
	}

	function toggleType(t: string, on: boolean) {
		if (!editing) return;
		const set = new Set(editing.config.record_types);
		if (on) set.add(t);
		else set.delete(t);
		editing.config.record_types = [...set].sort();
	}

	async function runPreview() {
		if (!editing) return;
		previewing = true;
		error = '';
		try {
			preview = await api<Report>('/api/v1/clones/preview', { method: 'POST', body: { config: editing.config } });
		} catch (e) {
			preview = null;
			error = e instanceof Error ? e.message : 'The preview failed';
		} finally {
			previewing = false;
		}
	}

	async function save(e: Event) {
		e.preventDefault();
		if (!editing) return;
		const ed = editing;
		const body = { name: ed.name, description: ed.description, config: ed.config };
		const ok = await run(
			() =>
				ed.isNew
					? api('/api/v1/clones/profiles', { method: 'POST', body })
					: api(`/api/v1/clones/profiles/${ed.name}`, { method: 'PUT', body }),
			`Profile ${ed.name} saved`
		);
		if (ok) editing = null;
	}

	function startPackage(name: string) {
		packing = name;
		passphrase = '';
		again = '';
		destination = 'folder';
		editing = null;
	}

	async function makePackage(e: Event) {
		e.preventDefault();
		const profile = packing;
		const ok = await run(
			() => api('/api/v1/clones/packages', { method: 'POST', body: { profile, passphrase, destination } }),
			'Package asked for'
		);
		if (ok) {
			packing = null;
			passphrase = again = '';
		}
	}

	const size = (n: number) =>
		n < 1024 ? `${n} B` : n < 1024 ** 2 ? `${(n / 1024).toFixed(0)} KB` : n < 1024 ** 3 ? `${(n / 1024 ** 2).toFixed(1)} MB` : `${(n / 1024 ** 3).toFixed(2)} GB`;

	function summary(p: Profile, types: { type: string; label: string }[]): string {
		const label = (t: string) => types.find((x) => x.type === t)?.label ?? t;
		const which = p.records === 'open' ? 'open' : p.records === 'recent' ? `changed in the last ${p.recent_days} days` : 'all';
		const kinds = p.record_types.length ? p.record_types.map(label).join(', ') : 'every type';
		const masked = Object.entries(p.mask)
			.filter(([, on]) => on)
			.map(([k]) => k);
		const parts = [`${which} records of ${kinds}`, p.people === 'referenced' ? 'only the people they name' : 'everyone'];
		if (p.exclude.length) parts.push(`${p.exclude.length} exclusion${p.exclude.length === 1 ? '' : 's'}`);
		parts.push(masked.length ? `masks ${masked.join(', ')}` : 'nothing masked');
		return parts.join(' · ');
	}

	const passphraseProblem = $derived(
		passphrase.length > 0 && passphrase.length < 12 ? 'At least 12 characters' : again && again !== passphrase ? 'The two do not match' : ''
	);
</script>

{#snippet reportView(r: Report)}
	<div class="report">
		<table>
			<thead><tr><th scope="col">What</th><th scope="col" class="num">Kept</th><th scope="col" class="num">Of</th></tr></thead>
			<tbody>
				{#each r.records as c (c.type)}
					<tr><td>{c.label}</td><td class="num">{c.kept.toLocaleString()}</td><td class="num">{c.total.toLocaleString()}</td></tr>
				{/each}
				<tr><td>People</td><td class="num">{r.people.toLocaleString()}</td><td class="num">{r.people_total.toLocaleString()}</td></tr>
			</tbody>
		</table>
		<ul class="facts">
			{#if r.excluded}<li>{r.excluded.toLocaleString()} left out by the exclusions</li>{/if}
			<li>{r.attachments.toLocaleString()} attachment{r.attachments === 1 ? '' : 's'}{r.attachment_bytes ? `, ${size(r.attachment_bytes)}` : ''}</li>
			{#if r.missing_files}<li class="err">{r.missing_files} attachment file{r.missing_files === 1 ? ' was' : 's were'} missing from storage</li>{/if}
			<li>{r.cleared_total === 0 ? 'No links cleared' : `${r.cleared_total.toLocaleString()} link${r.cleared_total === 1 ? '' : 's'} to records left behind cleared`}</li>
		</ul>
		{#if r.cleared.length}
			<details>
				<summary>Links cleared</summary>
				<ul class="cleared">
					{#each r.cleared as c, i (i)}
						<li><span class="ref">{c.record}</span> {c.link} <span class="ref">{c.to}</span></li>
					{/each}
					{#if r.cleared_total > r.cleared.length}<li class="hint">and {r.cleared_total - r.cleared.length} more</li>{/if}
				</ul>
			</details>
		{/if}
	</div>
{/snippet}

<svelte:head><title>Clones – ServiceLater</title></svelte:head>

<div class="page">
	<h1>Clones</h1>
	<p class="lede">
		Copies of this instance for UAT or development. A profile says which records and people go and what is masked; what it leaves
		out never leaves this instance. A package made from it is encrypted with a passphrase you choose, and the target instance asks
		for it when the package is loaded.
	</p>

	{#if error}<p class="error" role="alert">{error}</p>{/if}

	{#if data}
		{#if !data.key_configured}
			<p class="warn" role="note">
				This server has no secrets key. A package’s passphrase waits for the worker sealed under it, so none can be made until
				<code>SL_SECRETS_KEY</code> is set (make one with <code>servicelater secrets gen-key</code>).
			</p>
		{/if}

		<section aria-labelledby="profiles-h">
			<div class="head">
				<h2 id="profiles-h">Profiles</h2>
				<button class="btn" type="button" onclick={newProfile} disabled={busy}>New profile</button>
			</div>
			{#if data.profiles.length === 0 && !editing?.isNew}
				<p class="hint">None yet. A profile is what a clone takes: start with one called uat or dev.</p>
			{/if}
			<ul class="rows">
				{#each data.profiles as p (p.name)}
					<li>
						<div class="what">
							<strong class="mono">{p.name}</strong>
							{#if p.description}<span>{p.description}</span>{/if}
						</div>
						<p class="hint">{summary(p.config, data.record_types)}</p>
						<div class="row-actions">
							<button class="btn primary" type="button" disabled={busy || !data.key_configured} onclick={() => startPackage(p.name)}>Make a package</button>
							<button class="btn" type="button" aria-label="Edit {p.name}" onclick={() => edit(p)}>Edit</button>
							{#if deleting === p.name}
								<button class="btn reject" type="button" disabled={busy} onclick={() => run(() => api(`/api/v1/clones/profiles/${p.name}`, { method: 'DELETE' }), `Profile ${p.name} removed`)}>Remove it</button>
								<button class="btn" type="button" onclick={() => (deleting = null)}>Keep it</button>
							{:else}
								<button class="btn quiet-btn" type="button" aria-label="Remove {p.name}" onclick={() => (deleting = p.name)}>Remove</button>
							{/if}
						</div>
						{#if packing === p.name}
							<form class="inset" onsubmit={makePackage} aria-label="Make a package of {p.name}">
								<p>
									The package is encrypted with this passphrase. Give it to whoever loads the package, separately from the file: anyone
									with both can read what the profile takes.
								</p>
								<div class="grid">
									<div class="field">
										<label for="pk-pass">Passphrase</label>
										<input id="pk-pass" type="password" bind:value={passphrase} autocomplete="new-password" minlength="12" />
									</div>
									<div class="field">
										<label for="pk-again">Again</label>
										<input id="pk-again" type="password" bind:value={again} autocomplete="new-password" />
									</div>
									<div class="field">
										<label for="pk-dest">Keep it in</label>
										<select id="pk-dest" bind:value={destination}>
											<option value="folder">The snapshots folder</option>
											<option value="s3" disabled={!data.s3_configured}>The S3 bucket of Admin → Storage{data.s3_configured ? '' : ' (none set)'}</option>
										</select>
									</div>
								</div>
								{#if passphraseProblem}<p class="err">{passphraseProblem}</p>{/if}
								<div class="row-actions">
									<button class="btn primary" disabled={busy || passphrase.length < 12 || again !== passphrase}>Make it</button>
									<button class="btn" type="button" onclick={() => (packing = null)}>Cancel</button>
								</div>
							</form>
						{/if}
					</li>
				{/each}
			</ul>

			{#if editing}
				{@const ed = editing}
				<form class="editor" onsubmit={save} aria-label={ed.isNew ? 'New profile' : `Profile ${ed.name}`}>
					<h3>{ed.isNew ? 'New profile' : `Profile ${ed.name}`}</h3>
					<div class="grid">
						{#if ed.isNew}
							<div class="field">
								<label for="pf-name">Name</label>
								<input id="pf-name" bind:value={ed.name} placeholder="uat" autocomplete="off" />
								<small class="hint">Lowercase letters, digits and dashes.</small>
							</div>
						{/if}
						<div class="field wide">
							<label for="pf-desc">Description</label>
							<input id="pf-desc" bind:value={ed.description} placeholder="Weekly refresh of UAT" />
						</div>
					</div>

					<fieldset>
						<legend>Records</legend>
						<div class="types" role="group" aria-label="Record types">
							{#each data.record_types as t (t.type)}
								<label class="check">
									<input type="checkbox" checked={ed.config.record_types.includes(t.type)} onchange={(e) => toggleType(t.type, e.currentTarget.checked)} />
									{t.label}
								</label>
							{/each}
						</div>
						<small class="hint">{ed.config.record_types.length ? 'Only the types ticked.' : 'None ticked: every type.'}</small>
						<div class="grid">
							<div class="field">
								<label for="pf-records">Which</label>
								<select id="pf-records" bind:value={ed.config.records}>
									<option value="all">All of them</option>
									<option value="open">Open and waiting ones</option>
									<option value="recent">Those changed recently</option>
								</select>
							</div>
							{#if ed.config.records === 'recent'}
								<div class="field">
									<label for="pf-recent">Changed in the last (days)</label>
									<input id="pf-recent" type="number" min="1" max="3650" bind:value={ed.config.recent_days} />
								</div>
							{/if}
						</div>
					</fieldset>

					<fieldset>
						<legend>Leave out</legend>
						{#each ed.config.exclude as x, i (i)}
							<div class="exclusion">
								<div class="field">
									<label for="ex-type-{i}">Records of</label>
									<select id="ex-type-{i}" bind:value={x.record_type}>
										{#each data.record_types as t (t.type)}<option value={t.type}>{t.label}</option>{/each}
									</select>
								</div>
								<div class="field grow">
									<label for="ex-when-{i}">When</label>
									<input id="ex-when-{i}" class="mono" bind:value={x.when} placeholder="record.assignment_group == 'HR'" autocomplete="off" />
								</div>
								<button class="btn quiet-btn" type="button" aria-label="Remove exclusion {i + 1}" onclick={() => ed.config.exclude.splice(i, 1)}>Remove</button>
							</div>
						{/each}
						<div>
							<button class="btn" type="button" onclick={() => ed.config.exclude.push({ record_type: data!.record_types[0]?.type ?? 'incident', when: '' })}>Add an exclusion</button>
						</div>
						<small class="hint">An expression like a rule’s condition, over <code>record</code>: records it holds for stay behind, with their comments, files and history.</small>
					</fieldset>

					<fieldset>
						<legend>With them</legend>
						<label class="check"><input type="checkbox" bind:checked={ed.config.internal_notes} /> Internal notes (without them, only public comments)</label>
						<div class="grid">
							<div class="field">
								<label for="pf-files">Attachments</label>
								<select id="pf-files" bind:value={ed.config.attachments}>
									<option value="kept">The kept records’ files</option>
									<option value="none">None</option>
								</select>
							</div>
							{#if ed.config.attachments === 'kept'}
								<div class="field">
									<label for="pf-max">Up to (MB each, 0 for any size)</label>
									<input id="pf-max" type="number" min="0" bind:value={ed.config.attachment_max_mb} />
								</div>
							{/if}
							<div class="field">
								<label for="pf-hist">Field history</label>
								<select id="pf-hist" bind:value={ed.config.history}>
									<option value="all">All of it</option>
									<option value="recent">Recent only</option>
									<option value="none">None</option>
								</select>
							</div>
							{#if ed.config.history === 'recent'}
								<div class="field">
									<label for="pf-hdays">For the last (days)</label>
									<input id="pf-hdays" type="number" min="1" max="3650" bind:value={ed.config.history_days} />
								</div>
							{/if}
						</div>
						<label class="check"><input type="checkbox" bind:checked={ed.config.logs} /> What was sent and run: notifications, deliveries, incoming email, rule and query runs</label>
					</fieldset>

					<fieldset>
						<legend>People</legend>
						<div class="field">
							<label for="pf-people">Who</label>
							<select id="pf-people" bind:value={ed.config.people}>
								<option value="all">Everyone</option>
								<option value="referenced">Only those the kept records and the groups name</option>
							</select>
						</div>
						<label class="check"><input type="checkbox" bind:checked={ed.config.passwords} /> Their passwords, so they sign in as they do here</label>
					</fieldset>

					<fieldset>
						<legend>Mask</legend>
						<label class="check"><input type="checkbox" bind:checked={ed.config.mask.names} /> Names (become “Person 12”)</label>
						<label class="check"><input type="checkbox" bind:checked={ed.config.mask.emails} /> Email addresses (become user-12@masked.invalid)</label>
						<label class="check"><input type="checkbox" bind:checked={ed.config.mask.phones} /> Phone numbers (left out)</label>
						<label class="check"><input type="checkbox" bind:checked={ed.config.mask.text} /> Descriptions, comments and workflow variables</label>
						<label class="check">
							<input type="checkbox" bind:checked={ed.config.mask.fields} /> Form fields marked as personal data
							{#if data.masked_fields.length}({data.masked_fields.join(', ')}){:else}(none are marked yet){/if}
						</label>
						<small class="hint">Service accounts and apps are never masked. The target still matches its own people to these by their real address.</small>
					</fieldset>

					<div class="row-actions">
						<button class="btn primary" disabled={busy || (ed.isNew && !ed.name)}>Save</button>
						<button class="btn" type="button" disabled={previewing} onclick={runPreview}>{previewing ? 'Working it out…' : 'Preview'}</button>
						<button class="btn" type="button" onclick={() => ((editing = null), (preview = null))}>Cancel</button>
					</div>
					{#if preview}
						<div aria-label="Preview" role="region">
							<h4>What it would take now</h4>
							{@render reportView(preview)}
						</div>
					{/if}
				</form>
			{/if}
		</section>

		<section aria-labelledby="packages-h">
			<h2 id="packages-h">Packages</h2>
			{#if data.packages.length === 0}
				<p class="hint">None yet.</p>
			{:else}
				<ul class="rows">
					{#each data.packages as k (k.id)}
						<li class:failed={k.status === 'failed'}>
							<div class="what">
								<strong>{k.taken_at ? when(k.taken_at) : when(k.created_at)}</strong>
								<span class="tag mono">{k.profile}</span>
								{#if k.status === 'pending' || k.status === 'running'}<span class="tag busy">{k.status === 'pending' ? 'Waiting for the worker' : 'Being made'}</span>{/if}
								{#if k.status === 'failed'}<span class="tag bad">Failed</span>{/if}
							</div>
							<div class="meta">
								{#if k.status === 'ready'}
									<span>{size(k.size)}</span>
									<span>{k.rows.toLocaleString()} rows</span>
									<span>{k.destination === 's3' ? 'S3' : 'folder'}</span>
								{/if}
								<span>by {k.requested_by_name}</span>
							</div>
							{#if k.error}<p class="err">{k.error}</p>{/if}
							{#if k.report}
								<details>
									<summary>What it holds</summary>
									{@render reportView(k.report)}
								</details>
							{/if}
							{#if k.status === 'ready' || k.status === 'failed'}
								<div class="row-actions">
									{#if k.status === 'ready'}
										<button class="btn" type="button" onclick={() => run(() => download(`/api/v1/clones/packages/${k.id}/file`, k.name), 'Download started')}>Download</button>
									{/if}
									{#if deleting === k.id}
										<button class="btn reject" type="button" disabled={busy} onclick={() => run(() => api(`/api/v1/clones/packages/${k.id}`, { method: 'DELETE' }), 'Package deleted')}>Delete it</button>
										<button class="btn" type="button" onclick={() => (deleting = null)}>Keep it</button>
									{:else}
										<button class="btn quiet-btn" type="button" onclick={() => (deleting = k.id)}>Delete</button>
									{/if}
								</div>
							{/if}
						</li>
					{/each}
				</ul>
				<small class="hint">Packages stay until you delete them. Folder: <span class="path">{data.folder}</span></small>
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
	h2,
	h3 {
		margin: 0;
		font: 650 1.0625rem/1.3 var(--font-display);
	}
	h4 {
		margin: 0 0 0.5rem;
		font-size: 0.95rem;
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
	.head {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: 1rem;
	}
	.warn {
		margin: 0 0 1rem;
		padding: 0.6rem 0.75rem;
		border-left: 3px solid var(--marker);
		background: var(--marker-soft);
		color: var(--marker-ink);
		font-size: 0.9rem;
	}
	.rows {
		list-style: none;
		margin: 0;
		padding: 0;
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
	.rows p {
		margin: 0;
	}
	.what {
		display: flex;
		flex-wrap: wrap;
		align-items: baseline;
		gap: 0.5rem;
	}
	.mono,
	.ref,
	.path {
		font-family: var(--font-mono);
	}
	.path {
		font-size: 0.8rem;
		overflow-wrap: anywhere;
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
	.row-actions {
		display: flex;
		flex-wrap: wrap;
		gap: 0.5rem;
	}
	.inset,
	.editor {
		display: grid;
		gap: 0.75rem;
		padding: 0.9rem;
		border: 1px solid var(--rule);
		border-radius: 8px;
		background: var(--surface);
	}
	.inset {
		border-left: 3px solid var(--blue);
	}
	.inset p {
		margin: 0;
		font-size: 0.9rem;
	}
	fieldset {
		display: grid;
		gap: 0.5rem;
		margin: 0;
		padding: 0.75rem 0 0;
		border: 0;
		border-top: 1px solid var(--rule);
		min-width: 0;
	}
	legend {
		padding: 0;
		font-weight: 700;
		font-size: 0.9rem;
	}
	.grid {
		display: grid;
		grid-template-columns: repeat(auto-fill, minmax(13rem, 1fr));
		align-items: start;
		gap: 0.75rem 1rem;
	}
	.wide {
		grid-column: 1 / -1;
	}
	.types {
		display: flex;
		flex-wrap: wrap;
		gap: 0.35rem 1.25rem;
	}
	.exclusion {
		display: flex;
		flex-wrap: wrap;
		align-items: end;
		gap: 0.5rem 0.75rem;
	}
	.exclusion .grow {
		flex: 1 1 16rem;
	}
	.check {
		display: flex;
		align-items: center;
		gap: 0.5rem;
	}
	.report {
		display: grid;
		gap: 0.5rem;
		padding-top: 0.25rem;
	}
	table {
		border-collapse: collapse;
		font-size: 0.9rem;
		max-width: 28rem;
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
	.facts,
	.cleared {
		margin: 0;
		padding-left: 1.1rem;
		font-size: 0.9rem;
	}
	details summary {
		cursor: pointer;
		color: var(--ink-2);
		font-size: 0.9rem;
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
