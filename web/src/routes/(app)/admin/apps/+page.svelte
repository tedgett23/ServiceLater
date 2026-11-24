<script lang="ts">
	import { onMount } from 'svelte';
	import { api, ApiError } from '$lib/api';
	import { when } from '$lib/format';
	import { instance } from '$lib/instance.svelte';
	import { session } from '$lib/session.svelte';
	import { can, P } from '$lib/types';
	import { ui } from '$lib/ui.svelte';

	type App = {
		name: string;
		version: string;
		description: string;
		author: string;
		definitions: number;
		local: number;
		permissions: string[];
		dependencies: { name: string; constraint: string }[];
		first_party: boolean;
		installed_by: string;
		installed_at: string;
		updated_at: string;
	};
	type Plan = {
		app: string;
		from_version: string;
		to_version: string;
		added: string[];
		changed: string[];
		removed: string[];
		renames: string[];
		permissions: string[];
		new_permissions: string[];
		runs_as_app: string[];
		notes: string[];
		no_change: boolean;
	};

	let installed = $state<App[] | null>(null);
	let error = $state('');
	let problems = $state<string[]>([]);
	let busy = $state(false);
	let file = $state<File | null>(null);
	let plan = $state<Plan | null>(null);
	let approve = $state(false);
	let removing = $state<string | null>(null);
	let confirmText = $state('');

	const canApprove = $derived(can(session.user, P.rolesManage));
	const devInstance = $derived(instance.environment === 'development');

	async function load() {
		try {
			installed = (await api<{ items: App[] }>('/api/v1/apps')).items;
		} catch (e) {
			error = e instanceof Error ? e.message : 'Could not load the apps';
		}
	}
	onMount(load);

	function fail(e: unknown, fallback: string) {
		error = e instanceof Error ? e.message : fallback;
		const body = e instanceof ApiError ? (e.body as { problems?: string[] } | undefined) : undefined;
		problems = body?.problems ?? [];
	}

	async function choose(e: Event) {
		const input = e.currentTarget as HTMLInputElement;
		const f = input.files?.[0];
		input.value = '';
		if (!f) return;
		busy = true;
		error = '';
		problems = [];
		plan = null;
		approve = false;
		try {
			plan = await api<Plan>('/api/v1/apps/plan', { method: 'POST', body: new Blob([f], { type: 'application/zip' }) });
			file = f;
		} catch (err) {
			file = null;
			fail(err, 'That file could not be read');
		} finally {
			busy = false;
		}
	}

	function cancel() {
		plan = null;
		file = null;
		error = '';
		problems = [];
	}

	async function install() {
		if (!file || !plan) return;
		busy = true;
		error = '';
		problems = [];
		try {
			const q = approve ? '?approve_permissions=true' : '';
			const done = await api<Plan>(`/api/v1/apps/install${q}`, { method: 'POST', body: new Blob([file], { type: 'application/zip' }) });
			ui.announce = done.from_version ? `${done.app} updated to ${done.to_version}` : `${done.app} ${done.to_version} installed`;
			plan = null;
			file = null;
			await load();
		} catch (err) {
			fail(err, 'The app was not installed');
		} finally {
			busy = false;
		}
	}

	async function remove(a: App) {
		busy = true;
		error = '';
		problems = [];
		try {
			await api(`/api/v1/apps/${a.name}?${new URLSearchParams({ confirm: confirmText })}`, { method: 'DELETE' });
			ui.announce = `${a.name} removed`;
			removing = null;
			await load();
		} catch (err) {
			fail(err, 'The app was not removed');
		} finally {
			busy = false;
		}
	}

	const verb = (p: Plan) => (p.no_change ? 'Install again' : p.from_version ? `Update to ${p.to_version}` : 'Install');
	const needsApproval = $derived(!!plan && plan.new_permissions.length > 0);
</script>

<svelte:head><title>Apps – ServiceLater</title></svelte:head>

<div class="page">
	<h1>Apps</h1>
	<p class="lede">
		Apps add record types, lifecycles, forms, rules, workflows, catalog items and roles. Install or update one from its
		<code>.swapp</code> file (<code>servicelater app pack</code> makes one): you see what it changes before anything does. Changes made here in
		the builders, and local files, stay with this instance when an app is updated.
	</p>

	{#if error}
		<div class="error" role="alert">
			<p>{error}</p>
			{#if problems.length}<ul>{#each problems as p, i (i)}<li>{p}</li>{/each}</ul>{/if}
		</div>
	{/if}

	<div class="actions">
		<label class="btn primary upload" class:disabled={busy}>
			Upload an app
			<input type="file" accept=".swapp,application/zip" disabled={busy} onchange={choose} />
		</label>
	</div>

	{#if plan}
		<section class="plan" aria-label="What installing does">
			<h2>
				{plan.app}
				{#if plan.no_change}{plan.to_version}: no changes{:else if plan.from_version}{plan.from_version} → {plan.to_version}{:else}{plan.to_version}, new{/if}
			</h2>
			{#if plan.added.length || plan.changed.length || plan.removed.length || plan.renames.length}
				<dl>
					{#each [['Adds', plan.added], ['Changes', plan.changed], ['Removes', plan.removed], ['Renames', plan.renames]] as const as [label, items] (label)}
						{#if items.length}
							<dt>{label}</dt>
							<dd><ul>{#each items as x (x)}<li>{x}</li>{/each}</ul></dd>
						{/if}
					{/each}
				</dl>
			{/if}
			{#if plan.permissions.length}
				<p>
					Its own actions (async rules, workflows{plan.runs_as_app.length ? ', and rules that act as the app' : ''}) may:
					{#each plan.permissions as perm (perm)}<code class="perm" class:new={plan.new_permissions.includes(perm)}>{perm}</code>{' '}{/each}
				</p>
			{/if}
			{#if plan.runs_as_app.length}<p class="hint">Rules that act as the app, not as the person saving: {plan.runs_as_app.join(', ')}.</p>{/if}
			{#each plan.notes as n, i (i)}<p class="note">{n}</p>{/each}
			{#if needsApproval}
				<div class="warn" role="note">
					<p>
						It asks for permissions it does not have yet: <strong>{plan.new_permissions.join(', ')}</strong>. Its actions use them for anyone,
						whoever set them off.
					</p>
					{#if canApprove}
						<label class="check"><input type="checkbox" bind:checked={approve} /> Approve these permissions</label>
					{:else}
						<p>Approving them needs the roles:manage permission: ask someone who holds it to install this.</p>
					{/if}
				</div>
			{/if}
			<div class="row-actions">
				<button class="btn primary" type="button" disabled={busy || (needsApproval && !approve)} onclick={install}>{verb(plan)}</button>
				<button class="btn" type="button" disabled={busy} onclick={cancel}>Cancel</button>
			</div>
		</section>
	{/if}

	<section aria-labelledby="installed-h">
		<h2 id="installed-h">Installed</h2>
		{#if installed}
			<ul class="apps">
				{#each installed as a (a.name)}
					<li>
						<div class="what">
							<strong>{a.name}</strong>
							<span class="version">{a.version}</span>
							{#if a.first_party}<span class="tag">Comes with ServiceLater</span>{/if}
							{#if a.local}<span class="tag local">{a.local} changed here</span>{/if}
						</div>
						{#if a.description}<p class="desc">{a.description}</p>{/if}
						<div class="meta">
							<span>{a.definitions} definitions</span>
							{#if a.author}<span>by {a.author}</span>{/if}
							{#if a.dependencies.length}<span>needs {a.dependencies.map((d) => `${d.name} ${d.constraint}`).join(', ')}</span>{/if}
							<span>{a.first_party ? 'installed at start' : `installed by ${a.installed_by || 'someone'}`}, {when(a.updated_at)}</span>
						</div>
						{#if a.permissions.length}
							<div class="perms">{#each a.permissions as perm (perm)}<code class="perm">{perm}</code>{/each}</div>
						{/if}
						{#if !a.first_party}
							{#if removing === a.name}
								<form
									class="remove"
									aria-label="Remove {a.name}"
									onsubmit={(e) => {
										e.preventDefault();
										void remove(a);
									}}
								>
									<p>
										Removing it removes everything it defines. Records are kept: its record types retire (they still open by reference) and
										its records fall back to the built-in statuses. Its roles are taken from everyone holding them.
									</p>
									<label class="field">
										<span>Type <strong>{a.name}</strong> to confirm</span>
										<input bind:value={confirmText} autocomplete="off" />
									</label>
									<div class="row-actions">
										<button class="btn danger" disabled={busy || confirmText !== a.name}>Remove {a.name}</button>
										<button class="btn" type="button" onclick={() => (removing = null)}>Keep it</button>
									</div>
								</form>
							{:else}
								<div class="row-actions">
									<button
										class="btn"
										type="button"
										aria-label="Remove {a.name}"
										onclick={() => {
											removing = a.name;
											confirmText = '';
										}}>Remove</button
									>
								</div>
							{/if}
						{/if}
					</li>
				{/each}
			</ul>
		{/if}
	</section>

	<section aria-labelledby="dev-h">
		<h2 id="dev-h">Developing an app</h2>
		<p>
			<code>servicelater app dev -url {location.origin} ./my-app</code> installs a folder and installs it again on every save, with an API key
			in <code>SL_API_KEY</code> whose service account holds apps:manage. It works only on an instance marked development in Admin → Instance{devInstance
				? ', as this one is'
				: '; this one is not'}. Pipelines install a packed app with <code>servicelater app install -url</code>.
		</p>
	</section>
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
	section {
		display: grid;
		gap: 0.75rem;
		padding: 1.25rem 0;
		border-top: 1px solid var(--rule);
	}
	section p {
		margin: 0;
	}
	.error {
		margin-bottom: 1rem;
		padding: 0.6rem 0.75rem;
		border-left: 3px solid var(--red);
		background: var(--raised);
		color: var(--red);
		font-size: 0.9rem;
	}
	.error > p {
		font-weight: 600;
	}
	.btn.danger {
		color: var(--red);
	}
	.error ul {
		margin: 0.4rem 0 0;
		padding-left: 1.1rem;
	}
	.error p {
		margin: 0;
	}
	.error li {
		color: var(--ink);
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
	.plan {
		padding: 1rem;
		margin-bottom: 1rem;
		border: 1px solid var(--rule);
		border-radius: 8px;
		background: var(--surface);
	}
	dl {
		display: grid;
		grid-template-columns: max-content 1fr;
		gap: 0.35rem 1rem;
		margin: 0;
	}
	dt {
		color: var(--ink-2);
		font-size: 0.875rem;
	}
	dd {
		margin: 0;
	}
	dd ul {
		margin: 0;
		padding: 0;
		list-style: none;
		font-family: var(--font-mono);
		font-size: 0.8125rem;
	}
	.note,
	.warn {
		padding: 0.6rem 0.75rem;
		border-left: 3px solid var(--blue);
		background: var(--raised);
		font-size: 0.9rem;
	}
	.warn {
		display: grid;
		gap: 0.5rem;
		border-left-color: var(--marker);
		background: var(--marker-soft);
		color: var(--marker-ink);
	}
	.warn p {
		margin: 0;
	}
	.check {
		display: inline-flex;
		align-items: center;
		gap: 0.4rem;
		font-weight: 600;
	}
	.perm {
		display: inline-block;
		margin: 0 0.25rem 0.25rem 0;
		padding: 0.05rem 0.4rem;
		border-radius: 5px;
		background: var(--hover);
		font-size: 0.75rem;
	}
	.perm.new {
		background: var(--marker);
		color: var(--on-marker);
	}
	.apps {
		list-style: none;
		margin: 0;
		padding: 0;
	}
	.apps li {
		display: grid;
		gap: 0.35rem;
		padding: 0.75rem 0;
		border-bottom: 1px solid var(--rule);
	}
	.apps li:last-child {
		border-bottom: 0;
	}
	.what {
		display: flex;
		flex-wrap: wrap;
		align-items: baseline;
		gap: 0.5rem;
	}
	.version {
		font-family: var(--font-mono);
		font-size: 0.8125rem;
		color: var(--ink-2);
	}
	.tag {
		padding: 0.05rem 0.45rem;
		border-radius: 999px;
		background: var(--hover);
		color: var(--ink-2);
		font-size: 0.8rem;
	}
	.tag.local {
		background: var(--marker-soft);
		color: var(--marker-ink);
	}
	.desc {
		margin: 0;
	}
	.meta {
		display: flex;
		flex-wrap: wrap;
		gap: 0.25rem 1rem;
		color: var(--ink-2);
		font-size: 0.875rem;
	}
	.remove {
		display: grid;
		gap: 0.6rem;
		padding: 0.75rem;
		border-left: 3px solid var(--red);
		background: var(--raised);
	}
	.remove p {
		margin: 0;
	}
	code {
		overflow-wrap: anywhere;
	}
</style>
