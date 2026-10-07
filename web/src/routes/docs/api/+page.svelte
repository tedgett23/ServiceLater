<script lang="ts">
	import { onMount, tick } from 'svelte';
	import SchemaFields from '$lib/components/SchemaFields.svelte';
	import { byTag, inlineObject, referenced, typeName, type Document, type Endpoint } from '$lib/openapi';

	// The document is generated from the server's source and served by the server itself, so this page always
	// describes the API it is talking to. It describes the API, not anyone's data, so it needs no sign-in.
	const SPEC = '/api/v1/openapi.json';

	let doc = $state<Document | null>(null);
	let error = $state('');
	let filter = $state('');
	// The endpoint list starts open beside the content on a wide screen, and folded above it on a phone.
	let navOpen = $state(true);

	onMount(async () => {
		navOpen = !matchMedia('(max-width: 52rem)').matches;
		try {
			const res = await fetch(SPEC);
			if (!res.ok) throw new Error(res.statusText);
			doc = await res.json();
			// A link to one endpoint (#updateTicket) arrives before the page has anything to scroll to.
			await tick();
			if (location.hash) document.getElementById(decodeURIComponent(location.hash.slice(1)))?.scrollIntoView();
		} catch {
			error = 'Could not load the API description.';
		}
	});

	const tags = $derived(doc ? byTag(doc) : []);
	const shown = $derived.by(() => {
		const q = filter.trim().toLowerCase();
		if (!q) return tags;
		return tags
			.map(([tag, eps]) => [tag, eps.filter((e) => match(e, q))] as [string, Endpoint[]])
			.filter(([, eps]) => eps.length > 0);
	});
	const schemas = $derived(doc ? Object.entries(doc.components.schemas).sort(([a], [b]) => a.localeCompare(b)) : []);

	function match(e: Endpoint, q: string) {
		return [e.method, e.path, e.op.summary, e.op.description ?? ''].some((s) => s.toLowerCase().includes(q));
	}

	/** Paragraphs, with `code` spans. */
	function paragraphs(text: string) {
		return text.split('\n\n').map((p) => p.split('`').map((part, i) => ({ code: i % 2 === 1, text: part })));
	}

	function access(e: Endpoint) {
		if (e.op.security.length === 0) return 'No sign-in needed';
		const perms = e.op['x-permissions'];
		return perms ? `Signed in, with the ${perms.join(' or ')} permission` : 'Signed in, anyone';
	}

	const ok = (code: string) => code.startsWith('2');
</script>

<svelte:head><title>API – ServiceLater</title></svelte:head>

{#snippet rich(text: string)}
	{#each paragraphs(text) as para, i (i)}
		<p>
			{#each para as part, j (j)}{#if part.code}<code>{part.text}</code>{:else}{part.text}{/if}{/each}
		</p>
	{/each}
{/snippet}

<div class="shell">
	<header>
		<a class="mark" href="/tickets">ServiceLater</a>
		<span class="title">API</span>
		<a class="raw" href={SPEC}>OpenAPI JSON</a>
	</header>

	{#if error}
		<p class="msg" role="alert">{error}</p>
	{:else if doc}
		<div class="layout">
			<nav aria-label="Endpoints">
				<label class="field">
					<span class="sr-only">Filter endpoints</span>
					<input type="search" bind:value={filter} placeholder="Filter endpoints" />
				</label>
				<details bind:open={navOpen}>
					<summary>Endpoints</summary>
					{#each shown as [tag, eps] (tag)}
						<p class="navtag">{tag}</p>
						<ul>
							{#each eps as e (e.op.operationId)}
								<li><a href="#{e.op.operationId}"><span class="m">{e.method}</span> {e.op.summary}</a></li>
							{/each}
						</ul>
					{/each}
					<p class="navtag">Types</p>
					<ul>
						{#each schemas as [name] (name)}
							<li><a href="#schema-{name}">{name}</a></li>
						{/each}
					</ul>
				</details>
			</nav>

			<main>
				<h1>{doc.info.title} <span class="ver">{doc.info.version}</span></h1>
				<div class="intro">{@render rich(doc.info.description)}</div>

				{#each shown as [tag, eps] (tag)}
					<section class="tag" aria-labelledby="tag-{tag}">
						<h2 id="tag-{tag}">{tag}</h2>
						{#each eps as e (e.op.operationId)}
							{@const req = e.op.requestBody?.content['application/json'].schema}
							<article id={e.op.operationId} aria-labelledby="{e.op.operationId}-h">
								<h3 id="{e.op.operationId}-h">{e.op.summary}</h3>
								<p class="route"><span class="m">{e.method}</span> <code>{e.path}</code></p>
								<p class="access">{access(e)}</p>
								{#if e.op.description}<div class="desc">{@render rich(e.op.description)}</div>{/if}

								{#if e.op.parameters?.length}
									<h4>Parameters</h4>
									<dl class="params">
										{#each e.op.parameters as p (p.in + p.name)}
											<div>
												<dt><code>{p.name}</code> <span class="type">{typeName(p.schema)}, in the {p.in}</span></dt>
											</div>
										{/each}
									</dl>
								{/if}

								{#if req}
									<h4>Request body</h4>
									{#if referenced(req)}
										<p><a href="#schema-{referenced(req)}">{typeName(req)}</a></p>
									{:else if inlineObject(req)}
										<SchemaFields schema={inlineObject(req)!} request />
									{/if}
								{/if}

								<h4>Responses</h4>
								{#each Object.entries(e.op.responses).filter(([c]) => ok(c)) as [code, r] (code)}
									{@const body = r.content?.['application/json'].schema}
									<p class="status"><strong>{code}</strong> {r.description}{#if body && referenced(body)}: <a href="#schema-{referenced(body)}">{typeName(body)}</a>{/if}</p>
									{#if body && !referenced(body) && inlineObject(body)}
										<SchemaFields schema={inlineObject(body)!} />
									{/if}
								{/each}
								<p class="errors">
									Errors:
									{#each Object.entries(e.op.responses).filter(([c]) => !ok(c)) as [code, r], i (code)}{i ? ', ' : ''}<span title={r.description}>{code} {r.description}</span>{/each}
								</p>
							</article>
						{/each}
					</section>
				{/each}

				{#if !filter.trim()}
					<section class="tag" aria-labelledby="types-h">
						<h2 id="types-h">Types</h2>
						{#each schemas as [name, s] (name)}
							<article id="schema-{name}" aria-labelledby="schema-{name}-h">
								<h3 id="schema-{name}-h">{name}</h3>
								{#if s.description}<div class="desc">{@render rich(s.description)}</div>{/if}
								<SchemaFields schema={s} />
							</article>
						{/each}
					</section>
				{/if}
			</main>
		</div>
	{:else}
		<p class="msg" aria-live="polite">Loading…</p>
	{/if}
</div>

<style>
	.shell {
		height: 100dvh;
		display: flex;
		flex-direction: column;
		background: var(--surface);
	}
	header {
		display: flex;
		align-items: center;
		gap: 0.9rem;
		height: 3.25rem;
		padding: 0 1rem;
		background: var(--chrome);
		color: var(--on-chrome);
		flex: none;
	}
	.mark {
		font: 900 1.6rem/1 var(--font-display);
		letter-spacing: 0.01em;
		text-decoration: none;
		color: var(--marker);
	}
	.title {
		font: 700 1.3rem/1 var(--font-display);
	}
	.raw {
		margin-left: auto;
		color: var(--on-chrome);
		font-size: 0.9rem;
		font-weight: 600;
	}
	.layout {
		flex: 1;
		min-height: 0;
		display: grid;
		grid-template-columns: 17rem minmax(0, 1fr);
	}
	nav {
		overflow-y: auto;
		padding: 1rem;
		border-right: 1px solid var(--rule);
		background: var(--ground);
	}
	nav summary {
		display: none;
	}
	nav ul {
		list-style: none;
		margin: 0 0 0.5rem;
		padding: 0;
	}
	nav li a {
		display: block;
		padding: 0.2rem 0.3rem;
		border-radius: var(--radius);
		font-size: 0.875rem;
		color: var(--ink);
		text-decoration: none;
	}
	nav li a:hover {
		background: var(--blue-soft);
	}
	.navtag {
		margin: 0.9rem 0 0.2rem;
		font-weight: 700;
		font-size: 0.85rem;
		color: var(--ink-2);
	}
	.m {
		display: inline-block;
		min-width: 3.4rem;
		font: 700 0.75rem/1 ui-monospace, monospace;
		color: var(--ink-2);
	}
	main {
		overflow-y: auto;
		padding: 1.25rem 2rem 4rem;
	}
	main > * {
		max-width: 52rem;
	}
	h1 {
		font: 800 2.2rem/1.1 var(--font-display);
	}
	.ver {
		font-size: 1.2rem;
		color: var(--ink-2);
	}
	.intro {
		margin: 0.75rem 0 1.5rem;
		display: grid;
		gap: 0.6rem;
	}
	h2 {
		font: 800 1.6rem/1.2 var(--font-display);
		text-transform: capitalize;
		margin: 2rem 0 0.25rem;
		padding-bottom: 0.3rem;
		border-bottom: 2px solid var(--ink);
	}
	article {
		padding: 1.1rem 0 1.25rem;
		border-bottom: 1px solid var(--rule);
		display: grid;
		gap: 0.45rem;
		scroll-margin-top: 1rem;
	}
	h3 {
		font: 700 1.25rem/1.2 var(--font-display);
	}
	h4 {
		margin-top: 0.4rem;
		font-size: 0.9rem;
		font-weight: 700;
	}
	.route code {
		font-weight: 700;
		overflow-wrap: anywhere;
	}
	.access,
	.errors {
		font-size: 0.85rem;
		color: var(--ink-2);
	}
	.desc {
		display: grid;
		gap: 0.4rem;
		overflow-wrap: anywhere;
	}
	.params {
		margin: 0;
		display: grid;
		gap: 0.3rem;
	}
	.type {
		font-size: 0.85rem;
		color: var(--ink-2);
	}
	.status {
		font-size: 0.95rem;
	}
	code {
		font-family: ui-monospace, 'SFMono-Regular', Menlo, monospace;
		font-size: 0.88em;
	}
	.msg {
		padding: 1rem;
		color: var(--ink-2);
	}

	/* Narrow: the endpoint list folds away above the content. */
	@media (max-width: 52rem) {
		.layout {
			display: block;
			overflow-y: auto;
		}
		nav {
			border-right: 0;
			border-bottom: 1px solid var(--rule);
			overflow: visible;
		}
		nav summary {
			display: list-item;
			margin-top: 0.75rem;
			font-weight: 700;
			cursor: pointer;
		}
		main {
			overflow: visible;
			padding: 1rem 1rem 3rem;
		}
	}
</style>
