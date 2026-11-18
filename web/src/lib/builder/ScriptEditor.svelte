<script lang="ts">
	import { onMount } from 'svelte';
	import { api } from '$lib/api';
	import type { Problem, ScriptView } from '$lib/codemirror';

	/**
	 * A Starlark script in CodeMirror (loaded only when one is shown), checked by the server as it is typed: mistakes
	 * are marked on their lines. Read-only for someone without scripts:edit.
	 */
	let {
		value,
		readonly = false,
		label = 'Script',
		app = '',
		file = '',
		onchange
	}: {
		value: string;
		readonly?: boolean;
		label?: string;
		/** The app it belongs to: its script files are what it may load. */
		app?: string;
		/** Set for a script file of the app (checked as a file), rather than a step's or rule's script. */
		file?: string;
		onchange: (text: string) => void;
	} = $props();

	let host: HTMLDivElement;
	let view: ScriptView | null = null;
	let problems = $state<Problem[]>([]);
	let failed = $state(false);
	let timer: ReturnType<typeof setTimeout> | undefined;
	let asked = 0;

	function check(text: string) {
		if (readonly) return;
		clearTimeout(timer);
		timer = setTimeout(async () => {
			const n = ++asked;
			try {
				const r = await api<{ problems: Problem[] }>('/api/v1/scripts/check', { method: 'POST', body: { script: text, app, file } });
				if (n !== asked) return;
				problems = text.trim() ? r.problems : [];
				view?.setProblems(problems);
			} catch {
				/* the save says it all */
			}
		}, 500);
	}

	onMount(() => {
		let gone = false;
		import('$lib/codemirror')
			.then((cm) => {
				if (gone) return;
				view = cm.createScriptEditor(host, value, readonly, label, (text) => {
					onchange(text);
					check(text);
				});
				if (value.trim()) check(value);
			})
			.catch(() => (failed = true));
		return () => {
			gone = true;
			clearTimeout(timer);
			view?.destroy();
		};
	});

	$effect(() => {
		view?.setReadOnly(readonly);
	});
</script>

<div class="script" bind:this={host}></div>
{#if failed}
	<textarea class="fallback" aria-label={label} {readonly} value={value} oninput={(e) => onchange(e.currentTarget.value)} spellcheck="false"></textarea>
{/if}
{#if problems.length}
	<ul class="problems" role="alert">
		{#each problems as p, i (i)}<li>{p.line ? `Line ${p.line}: ` : ''}{p.message}</li>{/each}
	</ul>
{/if}

<style>
	.script {
		min-width: 0;
	}
	.fallback {
		width: 100%;
		min-height: 9rem;
		font: 0.8125rem/1.6 var(--font-mono);
		padding: 0.5rem;
		border: 1px solid var(--rule);
		border-radius: var(--radius);
		background: var(--raised);
		color: var(--ink);
	}
	.problems {
		margin: 0.4rem 0 0;
		padding: 0.4rem 0.6rem 0.4rem 1.6rem;
		border-left: 3px solid var(--red);
		background: var(--raised);
		font-size: 0.8125rem;
	}
</style>
