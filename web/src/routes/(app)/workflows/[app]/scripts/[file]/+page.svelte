<script lang="ts">
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { api, ApiError } from '$lib/api';
	import ScriptEditor from '$lib/builder/ScriptEditor.svelte';
	import { when } from '$lib/format';
	import { can, P } from '$lib/types';
	import { ui } from '$lib/ui.svelte';

	/** A script file of an app: functions its script steps and rules load. Saving reinstalls the app with it. */
	interface ScriptFile {
		app: string;
		name: string;
		source: string;
		edited: boolean;
		shipped: boolean;
		edited_at?: string;
		edited_by?: string | null;
		used_by: string[];
	}

	let { data } = $props();
	const canScript = $derived(can(data.user, P.scriptsEdit));
	const app = $derived(page.params.app ?? '');
	// The address leaves out .star: the server takes a path with a file extension for a file to send, not a page.
	const name = $derived(`${(page.params.file ?? '').replace(/\.star$/, '')}.star`);
	const base = $derived(name.replace(/\.star$/, ''));
	const isNew = $derived(page.url.searchParams.has('new'));

	let file = $state<ScriptFile | null>(null);
	let source = $state('');
	let version = $state(0);
	let loadError = $state('');
	let saving = $state(false);
	let error = $state('');
	let problems = $state<string[]>([]);
	let confirmingReset = $state(false);
	const dirty = $derived(!!file && source !== file.source);

	const starter = (file: string) => `# Functions the app's script steps and rules load:
#   load("${file}", "cancel_open_tasks")
def cancel_open_tasks(record, reason):
    """Cancel the open records under one, each with a note."""
    for task in children(record, open=True):
        update(task, {"status": "cancelled"}, comment=reason)
`;

	onMount(async () => {
		try {
			file = await api<ScriptFile>(`/api/v1/scripts/${app}/${name}`);
			source = file.source;
		} catch (e) {
			if (e instanceof ApiError && e.status === 404 && isNew && canScript) {
				file = { app, name, source: '', edited: false, shipped: false, used_by: [] };
				source = starter(name);
			} else {
				loadError = e instanceof ApiError && e.status === 404 ? `${app} has no script file ${name}.` : 'The script file could not be loaded.';
			}
		}
	});

	async function save() {
		saving = true;
		error = '';
		problems = [];
		try {
			file = await api<ScriptFile>(`/api/v1/scripts/${app}/${name}`, { method: 'PUT', body: { source } });
			source = file.source;
			version++;
			ui.announce = `${name} saved`;
			if (isNew) void goto(`/workflows/${app}/scripts/${base}`, { replaceState: true });
		} catch (e) {
			error = e instanceof Error ? e.message : 'It was not saved';
			if (e instanceof ApiError) problems = ((e.body as { problems?: string[] })?.problems ?? []).slice(0, 20);
		} finally {
			saving = false;
		}
	}

	async function reset() {
		saving = true;
		error = '';
		try {
			const after = await api<ScriptFile | undefined>(`/api/v1/scripts/${app}/${name}/edits`, { method: 'DELETE' });
			if (!after) {
				void goto('/workflows');
				return;
			}
			file = after;
			source = after.source;
			version++;
			ui.announce = 'Changes undone';
		} catch (e) {
			error = e instanceof Error ? e.message : 'That did not work';
			if (e instanceof ApiError) problems = ((e.body as { problems?: string[] })?.problems ?? []).slice(0, 20);
		} finally {
			saving = false;
			confirmingReset = false;
		}
	}

	const link = (used: string) => {
		const [kind, n] = used.split(':');
		return kind === 'workflow' ? `/workflows/${app}/${n}` : null;
	};
</script>

<svelte:head><title>{name} – Scripts – ServiceLater</title></svelte:head>

<div class="page">
	<nav class="crumbs" aria-label="Breadcrumb">
		<ol>
			<li><a href="/workflows">Workflows</a></li>
			<li>{app}</li>
			<li aria-current="page">{name}</li>
		</ol>
	</nav>
	{#if loadError}
		<p class="error" role="alert">{loadError}</p>
	{:else if file}
		<div class="top">
			<div>
				<h1>{name}</h1>
				<p class="lede">
					A script file of {app}: functions its script steps and rules load with <code>load("{name}", "function")</code>.
					{#if !file.shipped && !file.edited}Not saved yet.{:else if file.edited}{file.shipped ? 'Edited here' : 'Added here'}{file.edited_by ? ` by ${file.edited_by}` : ''}{file.edited_at ? ` on ${when(file.edited_at)}` : ''}.{/if}
				</p>
			</div>
			{#if canScript}
				<div class="actions">
					{#if dirty}<span class="pill">Unsaved changes</span>{/if}
					{#if file.edited}
						{#if confirmingReset}
							<span class="confirm" role="group" aria-label="Undo the changes made here?">
								<button type="button" class="btn danger" disabled={saving} onclick={reset}>{file.shipped ? 'Undo changes' : 'Remove the file'}</button>
								<button type="button" class="btn" onclick={() => (confirmingReset = false)}>Keep it</button>
							</span>
						{:else}
							<button type="button" class="btn" onclick={() => (confirmingReset = true)}>{file.shipped ? 'Undo the changes made here' : 'Remove this file'}</button>
						{/if}
					{/if}
					<button type="button" class="btn primary" disabled={saving || (!dirty && (file.shipped || file.edited))} onclick={save}>{saving ? 'Saving…' : 'Save'}</button>
				</div>
			{/if}
		</div>
		{#if error}
			<div class="error" role="alert">
				<p>{error}</p>
				{#if problems.length}<ul>{#each problems as p, i (i)}<li>{p}</li>{/each}</ul>{/if}
			</div>
		{/if}
		{#key version}
			<ScriptEditor value={source} readonly={!canScript} label="Script file {name}" {app} file={name} onchange={(t) => (source = t)} />
		{/key}
		{#if !canScript}<p class="hint">Changing a script needs the scripts:edit permission.</p>{/if}
		<section aria-labelledby="used-h">
			<h2 id="used-h">Loaded by</h2>
			{#if file.used_by.length}
				<ul>
					{#each file.used_by as u (u)}
						{@const href = link(u)}
						<li>{#if href}<a {href}>{u.replace(':', ' ')}</a>{:else}{u.replace(':', ' ')}{/if}</li>
					{/each}
				</ul>
				<p class="hint">Saving checks each of these against the change. Runs already going keep the code they started with.</p>
			{:else}
				<p class="hint">Nothing loads it yet.</p>
			{/if}
		</section>
	{/if}
</div>

<style>
	.page {
		height: 100%;
		overflow-y: auto;
		padding: 1rem 1.25rem 2rem;
		max-width: 60rem;
		display: grid;
		gap: 0.9rem;
		align-content: start;
	}
	.top {
		display: flex;
		flex-wrap: wrap;
		justify-content: space-between;
		gap: 0.75rem;
	}
	h1 {
		font: 650 1.375rem/1.3 var(--font-mono);
	}
	h2 {
		font: 650 1rem/1.3 var(--font-display);
		margin-bottom: 0.3rem;
	}
	.lede {
		color: var(--ink-2);
		margin-top: 0.2rem;
	}
	code {
		font: 0.8125rem var(--font-mono);
	}
	.actions,
	.confirm {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		gap: 0.5rem;
	}
	.pill {
		font-size: 0.75rem;
		padding: 0.15rem 0.55rem;
		border-radius: 99px;
		background: var(--marker-soft);
		color: var(--marker-ink);
		font-weight: 600;
	}
	.danger {
		color: var(--red);
	}
	.hint {
		color: var(--ink-2);
		font-size: 0.875rem;
	}
	ul {
		margin: 0;
		padding-left: 1.2rem;
	}
	.error {
		padding: 0.6rem 0.75rem;
		border-left: 3px solid var(--red);
		background: var(--raised);
		color: var(--red);
		font-weight: 600;
	}
	.error ul {
		font-weight: 400;
		color: var(--ink);
	}
	.crumbs ol {
		display: flex;
		flex-wrap: wrap;
		gap: 0.35rem;
		margin: 0;
		padding: 0;
		list-style: none;
		font-size: 0.8125rem;
		color: var(--ink-2);
	}
	.crumbs li + li::before {
		content: '/';
		margin-right: 0.35rem;
		color: color-mix(in srgb, var(--ink-2) 55%, transparent);
	}
	.crumbs a {
		color: var(--ink-2);
		font-weight: 550;
		text-decoration: none;
	}
</style>
