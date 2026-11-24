<script lang="ts">
	import { api } from '$lib/api';

	type Person = { id: string; name: string; email: string };

	/**
	 * The comment box. Typing @ and part of a name offers the people who may read the comment (`lookup` is the record's
	 * /mentionable; only for those who may read the directory); picking one writes @ and their name and adds them to
	 * `mentions`. Only mentions still named in the text are kept, so
	 * deleting "@Priya Shah" takes her off. The server checks that each person may read the comment.
	 */
	let {
		value = $bindable(''),
		mentions = $bindable<{ id: string; name: string }[]>([]),
		enabled,
		lookup,
		label,
		placeholder,
		internal = false,
		onkeydown
	}: {
		value: string;
		mentions: { id: string; name: string }[];
		enabled: boolean;
		lookup: string;
		label: string;
		placeholder: string;
		internal?: boolean;
		onkeydown?: (e: KeyboardEvent) => void;
	} = $props();

	const listId = `mention-${Math.random().toString(36).slice(2)}`;
	let area: HTMLTextAreaElement;
	let found = $state<Person[]>([]);
	let active = $state(0);
	let at = $state(-1); // where the @ being typed is, or -1
	let timer: ReturnType<typeof setTimeout>;
	let asked = 0;
	const open = $derived(at >= 0 && found.length > 0);

	// The @ before the caret that starts a mention being typed: at the start or after a space, with at most a first and
	// last name after it.
	function typing(): { start: number; query: string } | null {
		const caret = area.selectionStart;
		const before = value.slice(0, caret);
		const i = before.lastIndexOf('@');
		if (i < 0 || (i > 0 && !/\s/.test(before[i - 1]))) return null;
		const query = before.slice(i + 1);
		if (query.length > 40 || /\n/.test(query) || (query.match(/ /g) ?? []).length > 1) return null;
		return { start: i, query };
	}

	function look() {
		if (!enabled) return;
		const t = typing();
		clearTimeout(timer);
		if (!t || !t.query.trim()) {
			at = -1;
			found = [];
			return;
		}
		at = t.start;
		const n = ++asked;
		timer = setTimeout(async () => {
			try {
				const params = new URLSearchParams({ q: t.query.trim(), internal: String(internal) });
				const items = (await api<{ items: Person[] }>(`${lookup}?${params}`)).items;
				if (n === asked) {
					found = items;
					active = 0;
				}
			} catch {
				if (n === asked) found = [];
			}
		}, 150);
	}

	function pick(u: Person) {
		const caret = area.selectionStart;
		const text = `@${u.name} `;
		value = value.slice(0, at) + text + value.slice(caret);
		if (!mentions.some((m) => m.id === u.id)) mentions = [...mentions, { id: u.id, name: u.name }];
		const to = at + text.length;
		at = -1;
		found = [];
		queueMicrotask(() => {
			area.focus();
			area.setSelectionRange(to, to);
		});
	}

	function keydown(e: KeyboardEvent) {
		if (open) {
			if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
				e.preventDefault();
				active = (active + (e.key === 'ArrowDown' ? 1 : found.length - 1)) % found.length;
				return;
			}
			if ((e.key === 'Enter' && !e.metaKey && !e.ctrlKey) || e.key === 'Tab') {
				e.preventDefault();
				pick(found[active]);
				return;
			}
			if (e.key === 'Escape') {
				e.preventDefault();
				at = -1;
				found = [];
				return;
			}
		}
		onkeydown?.(e);
	}
</script>

<div class="mention-input">
	<label class="field">
		<span class="sr-only">{label}</span>
		<textarea
			bind:this={area}
			bind:value
			oninput={look}
			onclick={look}
			onkeydown={keydown}
			onblur={() => setTimeout(() => (at = -1), 150)}
			{placeholder}
			class:internal
			maxlength="10000"
			aria-autocomplete={enabled ? 'list' : undefined}
			aria-controls={enabled ? listId : undefined}
			aria-activedescendant={open ? `${listId}-${active}` : undefined}
		></textarea>
	</label>
	{#if open}
		<ul class="people" id={listId} role="listbox" aria-label="People to mention">
			{#each found as u, i (u.id)}
				<li
					id="{listId}-{i}"
					role="option"
					aria-selected={i === active}
					onmousedown={(e) => {
						e.preventDefault();
						pick(u);
					}}
				>
					<strong>{u.name}</strong> <span>{u.email}</span>
				</li>
			{/each}
		</ul>
	{/if}
</div>

<style>
	.mention-input {
		position: relative;
	}
	textarea.internal {
		background: var(--marker-soft);
		border-color: var(--marker);
	}
	.people {
		position: absolute;
		z-index: 20;
		left: 0;
		top: calc(100% + 0.25rem);
		min-width: min(22rem, 100%);
		margin: 0;
		padding: 0.25rem;
		list-style: none;
		background: var(--raised);
		border: 1px solid var(--rule);
		border-radius: 8px;
		box-shadow: var(--shadow-pop);
	}
	.people li {
		padding: 0.4rem 0.55rem;
		border-radius: 6px;
		cursor: pointer;
		font-size: 0.875rem;
		display: flex;
		gap: 0.5rem;
		align-items: baseline;
	}
	.people li span {
		color: var(--ink-2);
		font-size: 0.8125rem;
		overflow: hidden;
		text-overflow: ellipsis;
	}
	.people li[aria-selected='true'] {
		background: var(--hover);
	}
</style>
